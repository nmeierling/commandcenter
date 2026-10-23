// Package discovery scans configured roots for projects and derives their
// metadata: technology, how to start them, and which ports they are expected to
// use.
package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/devcc/agent/internal/composefile"
	"github.com/devcc/agent/internal/config"
	"github.com/devcc/agent/internal/model"
)

// Scanner walks roots looking for project markers.
type Scanner struct {
	Roots  []string
	Depth  int
	Ignore map[string]bool
}

// NewScanner builds a scanner from config.
func NewScanner(cfg config.Scan) *Scanner {
	ignore := map[string]bool{}
	for _, name := range cfg.Ignore {
		ignore[name] = true
	}
	depth := cfg.Depth
	if depth <= 0 {
		depth = 3
	}
	return &Scanner{Roots: cfg.Roots, Depth: depth, Ignore: ignore}
}

// Scan returns every project found under the roots, sorted by name.
func (s *Scanner) Scan() []model.Project {
	seen := map[string]bool{}
	var out []model.Project
	for _, root := range s.Roots {
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			continue
		}
		s.walk(root, 0, seen, &out)
	}
	sort.Slice(out, func(i, j int) bool {
		if strings.EqualFold(out[i].Name, out[j].Name) {
			return out[i].Path < out[j].Path
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (s *Scanner) walk(dir string, depth int, seen map[string]bool, out *[]model.Project) {
	if depth > s.Depth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}

	descend := true
	if p, ok := Inspect(dir, names); ok {
		abs, err := filepath.Abs(dir)
		if err == nil && !seen[abs] {
			seen[abs] = true
			*out = append(*out, p)
		}
		// Stop at a detected project, so `apps/web/node_modules/...` and
		// vendored sub-projects do not flood the dashboard. Workspace roots are
		// the exception: their whole point is that the real projects live one
		// level down.
		descend = isWorkspaceRoot(dir, names)
	}
	if !descend {
		return
	}

	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || s.Ignore[e.Name()] {
			continue
		}
		s.walk(filepath.Join(dir, e.Name()), depth+1, seen, out)
	}
}

// Inspect examines a single directory and reports whether it is a project.
// Exported so configured paths get the same treatment as discovered ones.
func Inspect(dir string, names map[string]bool) (model.Project, bool) {
	if names == nil {
		names = map[string]bool{}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return model.Project{}, false
		}
		for _, e := range entries {
			names[e.Name()] = true
		}
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return model.Project{}, false
	}
	abs = filepath.Clean(abs)

	var markers []string
	for _, m := range allMarkers {
		if names[m] {
			markers = append(markers, m)
		}
	}
	if len(markers) == 0 {
		return model.Project{}, false
	}

	p := model.Project{
		ID:      model.ID(abs),
		Name:    filepath.Base(abs),
		Path:    abs,
		Markers: markers,
		Source:  "discovered",
		Runtime: model.RuntimeNone,
	}

	switch {
	case hasCompose(names):
		applyCompose(&p, abs)
	case names["package.json"]:
		applyNode(&p, abs)
	case names["go.mod"]:
		applyGo(&p, abs, names)
	case names["Cargo.toml"]:
		applyRust(&p, abs, names)
	case names["pyproject.toml"] || names["requirements.txt"]:
		p.Kind = model.KindPython
	default:
		p.Kind = model.KindCustom
	}

	p.URLs = defaultURLs(p)
	return p, true
}

var allMarkers = []string{
	"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
	"package.json", "pnpm-workspace.yaml", "go.mod", "Cargo.toml",
	"pyproject.toml", "requirements.txt", "Makefile", "Procfile",
}

func hasCompose(names map[string]bool) bool {
	for _, n := range composefile.Names {
		if names[n] {
			return true
		}
	}
	return false
}

func isWorkspaceRoot(dir string, names map[string]bool) bool {
	if names["pnpm-workspace.yaml"] {
		return true
	}
	if names["package.json"] {
		if pkg, err := readPackageJSON(filepath.Join(dir, "package.json")); err == nil && len(pkg.Workspaces.Values) > 0 {
			return true
		}
	}
	return false
}

func applyCompose(p *model.Project, dir string) {
	p.Kind = model.KindCompose
	p.Runtime = model.RuntimeCompose
	p.ComposeFile = composefile.Find(dir)
	if override := composefile.FindOverride(dir); override != "" {
		p.ComposeExtraFiles = []string{override}
	}
	p.ComposeProject = composeProjectName(dir)

	if p.ComposeFile == "" {
		return
	}
	name, services, err := composefile.Parse(p.ComposeFile)
	if err != nil {
		// A compose file we cannot parse is still a compose project; the CLI is
		// the authority and will report the real error on start.
		return
	}
	if name != "" {
		p.ComposeProject = name
	}
	for _, svc := range services {
		p.Services = append(p.Services, svc.Name)
	}
	for _, hp := range composefile.AllPorts(services) {
		p.Ports = append(p.Ports, model.Port{
			Port:          hp.Host,
			Source:        "compose",
			Service:       hp.Service,
			Variable:      hp.Variable,
			ContainerPort: hp.Container,
			State:         model.PortFree,
		})
	}
}

// composeProjectName mirrors compose's own default: the directory name,
// lowercased with anything outside [a-z0-9_-] stripped.
func composeProjectName(dir string) string {
	base := strings.ToLower(filepath.Base(dir))
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-_")
}

type packageJSON struct {
	Name       string            `json:"name"`
	Scripts    map[string]string `json:"scripts"`
	Workspaces workspaces        `json:"workspaces"`
}

// workspaces accepts both the array form and the {packages: []} object form.
type workspaces struct {
	Values []string
}

func (w *workspaces) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		w.Values = arr
		return nil
	}
	var obj struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		w.Values = obj.Packages
	}
	return nil
}

func readPackageJSON(path string) (packageJSON, error) {
	var pkg packageJSON
	raw, err := os.ReadFile(path)
	if err != nil {
		return pkg, err
	}
	err = json.Unmarshal(raw, &pkg)
	return pkg, err
}

func applyNode(p *model.Project, dir string) {
	p.Kind = model.KindNode
	pkg, err := readPackageJSON(filepath.Join(dir, "package.json"))
	if err != nil {
		return
	}
	if pkg.Name != "" && !strings.HasPrefix(pkg.Name, "@") {
		p.Name = pkg.Name
	}

	script := firstScript(pkg.Scripts, "dev", "start", "serve", "develop")
	if script == "" {
		return
	}
	p.Runtime = model.RuntimeProcess
	p.StartCommand = packageManager(dir) + " run " + script

	if port, ok := portFromScript(pkg.Scripts[script]); ok {
		p.Ports = append(p.Ports, model.Port{Port: port, Source: "script", State: model.PortFree})
	}
	if port, ok := portFromDotEnv(dir); ok && !hasPort(p.Ports, port) {
		p.Ports = append(p.Ports, model.Port{Port: port, Source: "env", State: model.PortFree})
	}
}

func firstScript(scripts map[string]string, candidates ...string) string {
	for _, c := range candidates {
		if _, ok := scripts[c]; ok {
			return c
		}
	}
	return ""
}

// packageManager picks the tool matching the lockfile actually in the repo.
func packageManager(dir string) string {
	for _, c := range []struct{ lock, cmd string }{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lockb", "bun"},
		{"bun.lock", "bun"},
		{"package-lock.json", "npm"},
	} {
		if _, err := os.Stat(filepath.Join(dir, c.lock)); err == nil {
			return c.cmd
		}
	}
	return "npm"
}

func applyGo(p *model.Project, dir string, names map[string]bool) {
	p.Kind = model.KindGo
	if names["main.go"] {
		p.Runtime = model.RuntimeProcess
		p.StartCommand = "go run ."
		return
	}
	// A single cmd/<name> directory is unambiguous; more than one is not, so we
	// leave the project uncontrollable rather than pick arbitrarily.
	entries, err := os.ReadDir(filepath.Join(dir, "cmd"))
	if err != nil {
		return
	}
	var cmds []string
	for _, e := range entries {
		if e.IsDir() {
			cmds = append(cmds, e.Name())
		}
	}
	if len(cmds) == 1 {
		p.Runtime = model.RuntimeProcess
		p.StartCommand = "go run ./cmd/" + cmds[0]
	}
}

func applyRust(p *model.Project, dir string, names map[string]bool) {
	p.Kind = model.KindRust
	if _, err := os.Stat(filepath.Join(dir, "src", "main.rs")); err == nil {
		p.Runtime = model.RuntimeProcess
		p.StartCommand = "cargo run"
	}
}

var portFlagRe = regexp.MustCompile(`(?:--port[= ]|(?:^|\s)-p[= ])(\d{2,5})\b`)

// portFromScript looks for an explicit --port flag. We only trust an explicit
// flag: inferring a framework's default port (3000 for Next, 5173 for Vite,
// 8080 for others) would put numbers in the UI that are wrong the moment
// someone sets PORT, and a wrong port is worse than no port.
func portFromScript(script string) (int, bool) {
	m := portFlagRe.FindStringSubmatch(script)
	if m == nil {
		return 0, false
	}
	p, err := strconv.Atoi(m[1])
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	return p, true
}

func portFromDotEnv(dir string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "PORT" {
			continue
		}
		v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"'`))
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			return p, true
		}
	}
	return 0, false
}

func hasPort(ports []model.Port, port int) bool {
	for _, p := range ports {
		if p.Port == port {
			return true
		}
	}
	return false
}

func defaultURLs(p model.Project) []string {
	if len(p.Ports) == 0 {
		return nil
	}
	return []string{"http://localhost:" + strconv.Itoa(p.Ports[0].Port)}
}
