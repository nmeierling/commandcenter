// Command devcc is the local development command center agent.
//
// It discovers projects on this machine, reports their status, starts and stops
// them, and serves a dashboard on localhost. All host access — filesystem,
// processes, docker, ports — happens here; the browser only talks to this API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/devcc/agent/internal/api"
	"github.com/devcc/agent/internal/config"
	"github.com/devcc/agent/internal/core"
	"github.com/devcc/agent/internal/openurl"
	"github.com/devcc/agent/internal/webui"
)

const usage = `devcc — local development command center agent

Usage:
  devcc [flags]           start the agent and serve the dashboard
  devcc url               print the dashboard URL, including the API token
  devcc token             print the API token
  devcc init [path]       write an example config file (default: your user
                          config; pass devcc.yaml for a project-local one)
  devcc config            print which config file is in use
  devcc version           print the version

Config is loaded from $DEVCC_CONFIG, then ./devcc.yaml or ./devcc.yml in the
working directory, then your user config file.

Flags:
`

func main() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		if err := runSubcommand(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type flags struct {
	configPath string
	listen     string
	stateDir   string
	noToken    bool
	devOrigins string
	open       bool
	stopOnExit bool
}

func parseFlags(args []string) (*flags, *flag.FlagSet, error) {
	f := &flags{}
	fs := flag.NewFlagSet("devcc", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&f.configPath, "config", "", "path to the config file (default: "+config.DefaultPath()+")")
	fs.StringVar(&f.listen, "listen", "", "address to bind (default from config, 127.0.0.1:47123)")
	fs.StringVar(&f.stateDir, "state-dir", defaultStateDir(), "directory for the token, process state and logs")
	fs.BoolVar(&f.noToken, "no-token", false, "disable API token checks (insecure: any local page can then control your projects)")
	fs.StringVar(&f.devOrigins, "dev-origin", "", "comma-separated extra allowed origins, e.g. http://localhost:5173 for the Vite dev server")
	fs.BoolVar(&f.open, "open", false, "open the dashboard in the browser at startup")
	fs.BoolVar(&f.stopOnExit, "stop-on-exit", false, "stop supervised processes when the agent exits (default: leave them running)")
	if err := fs.Parse(args); err != nil {
		return nil, fs, err
	}
	return f, fs, nil
}

func run(args []string) error {
	f, _, err := parseFlags(args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(f.configPath)
	if err != nil {
		return err
	}
	if f.listen != "" {
		cfg.Listen = f.listen
	}
	if err := requireLoopback(cfg.Listen); err != nil {
		return err
	}

	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	auth, err := api.NewAuth(f.stateDir, cfg.Listen, splitCSV(f.devOrigins), f.noToken)
	if err != nil {
		return err
	}

	mgr, err := core.New(cfg, f.stateDir)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr.Start(ctx)

	srv := api.New(mgr, auth, webui.FS())
	dashURL := auth.URL(cfg.Listen)

	printBanner(cfg, mgr, dashURL, f)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx, cfg.Listen) }()

	if f.open {
		// Give the listener a moment so the browser does not race it.
		time.AfterFunc(300*time.Millisecond, func() {
			if err := openurl.Open(context.Background(), dashURL); err != nil {
				fmt.Fprintln(os.Stderr, "could not open browser:", err)
			}
		})
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\nshutting down…")
		if f.stopOnExit {
			mgr.StopSupervised()
		} else {
			fmt.Fprintln(os.Stderr, "supervised processes left running; they will be re-attached on the next start")
		}
		// Let the HTTP server drain.
		time.Sleep(300 * time.Millisecond)
		return nil
	}
}

func printBanner(cfg config.Config, mgr *core.Manager, dashURL string, f *flags) {
	st := mgr.Status()
	fmt.Printf("Development Command Center agent %s (%s)\n", core.Version, st.Platform)
	if dev := splitCSV(f.devOrigins); len(dev) > 0 {
		// In dev the embedded bundle is whatever `make build` last produced: no
		// Vite client, so no hot reload. Printing the agent's URL first is how
		// you end up editing files and watching nothing happen.
		fmt.Printf("  dashboard   %s   <- open this, it hot reloads\n", dev[0])
		fmt.Printf("  (embedded)  %s   last built bundle, no hot reload\n", dashURL)
	} else {
		fmt.Printf("  dashboard   %s\n", dashURL)
	}
	if cfg.Path() != "" {
		fmt.Printf("  config      %s\n", cfg.Path())
	} else {
		fmt.Printf("  config      none (run `devcc init` to create %s)\n", config.DefaultPath())
	}
	if len(cfg.Scan.Roots) > 0 {
		fmt.Printf("  scanning    %s (depth %d)\n", strings.Join(cfg.Scan.Roots, ", "), cfg.Scan.Depth)
	} else {
		fmt.Printf("  scanning    no roots configured\n")
	}
	fmt.Printf("  projects    %d\n", st.ProjectCount)
	switch {
	case !st.DockerOK:
		fmt.Printf("  docker      unavailable — compose projects cannot be controlled\n")
	case !st.DaemonOK:
		fmt.Printf("  docker      compose %s, but the daemon is not running\n", st.DockerVersion)
	default:
		fmt.Printf("  docker      compose %s\n", st.DockerVersion)
	}
	if !st.PortOwnersOK {
		fmt.Printf("  ports       ownership lookup unavailable; conflicts will be detected but not attributed\n")
	}
	if missing := cfg.MissingRoots(); len(missing) > 0 {
		// Silently skipping a mistyped root is how you end up staring at an
		// empty dashboard with no idea why.
		fmt.Printf("\n  WARNING: these scan roots do not exist and were skipped:\n")
		for _, root := range missing {
			fmt.Printf("           %s\n", root)
		}
		fmt.Printf("           edit scan.roots in %s\n", configLabel(cfg))
	}
	if f.noToken {
		fmt.Printf("\n  WARNING: running with --no-token. Any web page you visit can control\n" +
			"           your projects and run their configured commands.\n")
	}
	fmt.Println()
}

func configLabel(cfg config.Config) string {
	if p := cfg.Path(); p != "" {
		return p
	}
	return "your config file (run `devcc init` to create one)"
}

func runSubcommand(name string, args []string) error {
	switch name {
	case "version":
		fmt.Println(core.Version)
		return nil

	case "url", "token":
		f, _, err := parseFlags(args)
		if err != nil {
			return err
		}
		cfg, err := config.Load(f.configPath)
		if err != nil {
			return err
		}
		if f.listen != "" {
			cfg.Listen = f.listen
		}
		auth, err := api.NewAuth(f.stateDir, cfg.Listen, nil, f.noToken)
		if err != nil {
			return err
		}
		if name == "token" {
			fmt.Println(auth.Token())
		} else {
			fmt.Println(auth.URL(cfg.Listen))
		}
		return nil

	case "config":
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		if cfg.Path() == "" {
			fmt.Printf("no config file found; using built-in defaults\n")
			fmt.Printf("searched: $DEVCC_CONFIG, ./devcc.yaml, ./devcc.yml, %s\n", config.UserPath())
		} else {
			fmt.Println(cfg.Path())
		}
		if !cfg.ScanEnabled() {
			fmt.Println("scanning:   disabled (scan.enabled: false)")
		} else if len(cfg.Scan.Roots) == 0 {
			fmt.Println("scan roots: none configured")
		} else {
			missing := map[string]bool{}
			for _, r := range cfg.MissingRoots() {
				missing[r] = true
			}
			fmt.Println("scan roots:")
			for _, root := range cfg.Scan.Roots {
				mark := "ok"
				if missing[root] {
					mark = "MISSING — this directory does not exist"
				}
				fmt.Printf("  %-50s %s\n", root, mark)
			}
		}
		fmt.Printf("scan depth: %d\n", cfg.Scan.Depth)
		fmt.Printf("projects declared in config: %d\n", len(cfg.Projects))
		return nil

	case "init":
		path := config.UserPath()
		if len(args) > 0 {
			path = args[0]
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; delete it first if you want a fresh one", path)
		}
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		if err := os.WriteFile(path, []byte(config.Example), 0o600); err != nil {
			return err
		}
		fmt.Println("wrote", path)
		fmt.Println("edit scan.roots in it to choose which directories are scanned")
		return nil

	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil

	default:
		return fmt.Errorf("unknown command %q (try `devcc help`)", name)
	}
}

// requireLoopback refuses to bind a non-loopback address. The agent runs shell
// commands on behalf of its callers; exposing it on a LAN interface would hand
// that capability to the network.
func requireLoopback(addr string) error {
	host, _, err := splitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "":
		return nil
	}
	return fmt.Errorf("refusing to listen on %s: the agent must bind a loopback address", addr)
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("missing port")
	}
	host := strings.Trim(addr[:i], "[]")
	return host, addr[i+1:], nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func defaultStateDir() string {
	if v := os.Getenv("DEVCC_STATE_DIR"); v != "" {
		return v
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "devcc")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".devcc")
}
