// Package dockercli drives Docker through its CLI rather than its API socket.
// That is a deliberate v1 choice: the CLI is what developers already have
// configured (contexts, credentials, Docker Desktop vs colima vs rootless), and
// it keeps us out of API-version compatibility work.
package dockercli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devcc/agent/internal/model"
)

// Client runs docker commands.
type Client struct {
	// Bin is the docker executable, "docker" by default.
	Bin string

	availOnce sync.Once
	avail     bool
	availErr  error
	version   string
}

// New returns a client using the docker binary on PATH.
func New() *Client { return &Client{Bin: "docker"} }

// ErrUnavailable reports that docker compose cannot be used.
var ErrUnavailable = errors.New("docker compose is not available")

// Available reports whether `docker compose` works, caching the answer. The
// probe is deliberately `compose version` rather than `docker info`: we only
// need the CLI plugin for discovery-time metadata, and a stopped daemon should
// not make compose projects vanish from the dashboard.
func (c *Client) Available() (bool, string, error) {
	c.availOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, c.bin(), "compose", "version", "--short").Output()
		if err != nil {
			c.availErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
			return
		}
		c.avail = true
		c.version = strings.TrimSpace(string(out))
	})
	return c.avail, c.version, c.availErr
}

// DaemonUp reports whether the docker daemon is reachable. Unlike Available it
// is not cached, because the daemon starts and stops during a work session.
func (c *Client) DaemonUp(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, c.bin(), "info", "--format", "{{.ServerVersion}}").Run() == nil
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "docker"
	}
	return c.Bin
}

// psEntry mirrors the JSON emitted by `docker compose ps --format json`.
type psEntry struct {
	ID         string `json:"ID"`
	Name       string `json:"Name"`
	Image      string `json:"Image"`
	Service    string `json:"Service"`
	State      string `json:"State"`
	Status     string `json:"Status"`
	Health     string `json:"Health"`
	Project    string `json:"Project"`
	ExitCode   int    `json:"ExitCode"`
	Publishers []struct {
		URL           string `json:"URL"`
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

// PsResult is the state of one compose project.
type PsResult struct {
	Containers []model.Container
	// Published maps host port -> service name, learned from the running
	// containers. More reliable than parsing the compose file.
	Published map[int]string
	Project   string
}

// Ps runs `docker compose ps -a` in dir and parses the result.
func (c *Client) Ps(ctx context.Context, dir, composeFile string, opts Options) (*PsResult, error) {
	args := c.composeArgs(composeFile, opts.ExtraFiles, "ps", "--all", "--format", "json")
	out, stderr, err := c.run(ctx, dir, 20*time.Second, opts.Env, args...)
	if err != nil {
		return nil, composeError(err, stderr)
	}
	entries, err := decodePs(out)
	if err != nil {
		return nil, err
	}

	res := &PsResult{Published: map[int]string{}}
	for _, e := range entries {
		res.Project = e.Project
		running := strings.ToLower(e.State) == "running"

		// Publishers repeats a mapping once per host address family, so
		// de-duplicate before showing it.
		// Never nil: this is marshalled straight to the dashboard, which
		// iterates it, and JSON null is not an empty array.
		cports := []model.ContainerPort{}
		seenPort := map[int]bool{}
		for _, p := range e.Publishers {
			if p.PublishedPort <= 0 || seenPort[p.PublishedPort] {
				continue
			}
			seenPort[p.PublishedPort] = true
			cports = append(cports, model.ContainerPort{
				Host:      p.PublishedPort,
				Container: p.TargetPort,
				Protocol:  strings.ToLower(p.Protocol),
			})
			if running && !strings.EqualFold(p.Protocol, "udp") {
				res.Published[p.PublishedPort] = e.Service
			}
		}
		sort.Slice(cports, func(i, j int) bool { return cports[i].Host < cports[j].Host })

		res.Containers = append(res.Containers, model.Container{
			ID:      shortID(e.ID),
			Name:    e.Name,
			Service: e.Service,
			State:   strings.ToLower(e.State),
			Health:  strings.ToLower(e.Health),
			Status:  e.Status,
			Image:   e.Image,
			Ports:   cports,
			URL:     containerURL(cports, running),
		})
	}
	return res, nil
}

// decodePs accepts both formats compose has emitted over the years: a single
// JSON array (older v2) and newline-delimited objects (v2.21+).
func decodePs(out []byte) ([]psEntry, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var entries []psEntry
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("parse compose ps output: %w", err)
		}
		return entries, nil
	}
	var entries []psEntry
	sc := bufio.NewScanner(bytes.NewReader(trimmed))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e psEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parse compose ps line: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

// containerURL picks the address to open for a container: its lowest published
// TCP port. A container that publishes nothing has no URL.
func containerURL(ports []model.ContainerPort, running bool) string {
	if !running {
		return ""
	}
	for _, p := range ports {
		if p.Protocol == "udp" {
			continue
		}
		return "http://localhost:" + strconv.Itoa(p.Host)
	}
	return ""
}

// Options carry per-invocation environment and extra compose files, used when
// starting a project on a different port.
type Options struct {
	// Env entries in "KEY=value" form, appended to the agent's environment.
	Env []string
	// ExtraFiles are additional compose files layered after the project's own.
	ExtraFiles []string
}

// Up starts a compose project detached.
//
// Note the absence of --remove-orphans: it stops and deletes containers that
// belong to the project but are not in the current compose file, which is a
// service shutdown the user never asked for. Orphans are reported, not removed.
func (c *Client) Up(ctx context.Context, dir, composeFile string, services []string, opts Options) (string, error) {
	args := c.composeArgs(composeFile, opts.ExtraFiles, "up", "--detach")
	// Naming services explicitly is how a project-wide start skips the ones the
	// user marked as provided elsewhere. An empty list means "all of them".
	args = append(args, services...)
	out, stderr, err := c.run(ctx, dir, 15*time.Minute, opts.Env, args...)
	return combined(out, stderr), composeError(err, stderr)
}

// UpService starts a single service, leaving every other service in the
// project exactly as it is. `--no-deps` matters: without it compose would also
// start this service's dependencies, which is a decision the user did not make.
func (c *Client) UpService(ctx context.Context, dir, composeFile, service string, opts Options) (string, error) {
	args := c.composeArgs(composeFile, opts.ExtraFiles, "up", "--detach", "--no-deps", service)
	out, stderr, err := c.run(ctx, dir, 15*time.Minute, opts.Env, args...)
	return combined(out, stderr), composeError(err, stderr)
}

// StopService stops one service. It uses `stop` rather than `down`, so the
// container is kept and can be started again, and nothing else in the project
// is affected.
func (c *Client) StopService(ctx context.Context, dir, composeFile, service string, timeout int, opts Options) (string, error) {
	if timeout <= 0 {
		timeout = 10
	}
	// compose sends SIGTERM, then SIGKILL once the timeout expires.
	args := c.composeArgs(composeFile, opts.ExtraFiles,
		"stop", "--timeout", strconv.Itoa(timeout), service)
	out, stderr, err := c.run(ctx, dir, 5*time.Minute, opts.Env, args...)
	return combined(out, stderr), composeError(err, stderr)
}

// Down stops and removes a compose project's own containers.
func (c *Client) Down(ctx context.Context, dir, composeFile string, opts Options) (string, error) {
	args := c.composeArgs(composeFile, opts.ExtraFiles, "down")
	out, stderr, err := c.run(ctx, dir, 5*time.Minute, opts.Env, args...)
	return combined(out, stderr), composeError(err, stderr)
}

// Restart restarts a compose project's containers.
func (c *Client) Restart(ctx context.Context, dir, composeFile string, services []string, opts Options) (string, error) {
	args := c.composeArgs(composeFile, opts.ExtraFiles, "restart")
	args = append(args, services...)
	out, stderr, err := c.run(ctx, dir, 10*time.Minute, opts.Env, args...)
	return combined(out, stderr), composeError(err, stderr)
}

// SupportsOverrideTag reports whether the installed compose understands the
// `!override` YAML tag, which arrived in v2.24. Without it, a port override
// file would *append* a second published port instead of replacing the first,
// leaving the original conflict in place.
func (c *Client) SupportsOverrideTag() bool {
	ok, version, _ := c.Available()
	if !ok {
		return false
	}
	major, minor := parseVersion(version)
	if major > 2 {
		return true
	}
	return major == 2 && minor >= 24
}

func parseVersion(v string) (int, int) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return major, minor
}

// Logs returns the last n lines of a project's combined container logs.
func (c *Client) Logs(ctx context.Context, dir, composeFile string, tail int, opts Options) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	args := c.composeArgs(composeFile, opts.ExtraFiles, "logs", "--no-color", "--tail", strconv.Itoa(tail))
	out, stderr, err := c.run(ctx, dir, 30*time.Second, opts.Env, args...)
	if err != nil {
		return combined(out, stderr), composeError(err, stderr)
	}
	return string(out), nil
}

// FollowLogs streams logs until the context is cancelled. The caller must close
// the returned reader, which also terminates the underlying process.
func (c *Client) FollowLogs(ctx context.Context, dir, composeFile string, tail int, opts Options) (io.ReadCloser, error) {
	if tail <= 0 {
		tail = 100
	}
	args := c.composeArgs(composeFile, opts.ExtraFiles, "logs", "--no-color", "--follow", "--tail", strconv.Itoa(tail))
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), opts.Env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout.(io.Writer) // fold stderr into the same stream
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procReader{ReadCloser: stdout, cmd: cmd}, nil
}

type procReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (p *procReader) Close() error {
	err := p.ReadCloser.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
	return err
}

// RunningContainer is a container reported by `docker ps`, whether or not it
// belongs to a project devcc knows about.
type RunningContainer struct {
	ID    string
	Name  string
	Image string
	Ports []model.ContainerPort
	// ComposeProject and ComposeService come from compose's own labels, and are
	// empty for a container started with plain `docker run`.
	ComposeProject string
	ComposeService string
	Status         string
}

// ListContainers returns every running container.
//
// This is the source of truth for "what else is up on this machine": a postgres
// someone started by hand, or a compose stack living outside the scanned roots,
// is invisible to project discovery but is exactly the sort of thing you want to
// point a shared dependency at.
func (c *Client) ListContainers(ctx context.Context) ([]RunningContainer, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin(), "ps", "--format", "{{json .}}")
	cmd.Env = cleanEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var containers []RunningContainer
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e struct {
			ID     string `json:"ID"`
			Names  string `json:"Names"`
			Ports  string `json:"Ports"`
			Labels string `json:"Labels"`
			Image  string `json:"Image"`
			Status string `json:"Status"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		containers = append(containers, RunningContainer{
			ID:             shortID(e.ID),
			Name:           firstName(e.Names),
			Image:          e.Image,
			Ports:          parsePublished(e.Ports),
			ComposeProject: labelValue(e.Labels, "com.docker.compose.project"),
			ComposeService: labelValue(e.Labels, "com.docker.compose.service"),
			Status:         e.Status,
		})
	}
	return containers, sc.Err()
}

// Owner describes this container as the holder of a port.
func (rc RunningContainer) Owner() *model.PortOwner {
	return &model.PortOwner{
		ContainerID:    rc.ID,
		Container:      rc.Name,
		ComposeProject: rc.ComposeProject,
		Command:        rc.Image,
	}
}

// ContainerOwners maps published host ports to the container publishing them.
// This is how we attribute a port to a container rather than to the
// docker-proxy process that actually holds the socket.
func (c *Client) ContainerOwners(ctx context.Context) (map[int]*model.PortOwner, error) {
	containers, err := c.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	owners := map[int]*model.PortOwner{}
	for _, rc := range containers {
		owner := rc.Owner()
		for _, p := range rc.Ports {
			if _, dup := owners[p.Host]; !dup {
				owners[p.Host] = owner
			}
		}
	}
	return owners, nil
}

// publishedRe matches `docker ps` port strings such as
// "0.0.0.0:3000->3000/tcp" and "[::]:3000->3000/tcp", capturing the host port,
// the container port and the protocol.
var publishedRe = regexp.MustCompile(`(?:^|,\s*)(?:\[[^\]]*\]|[0-9.]+):(\d+)->(\d+)/(\w+)`)

// parsePublished extracts the published mappings, collapsing the duplicate
// entries docker emits for IPv4 and IPv6.
func parsePublished(s string) []model.ContainerPort {
	var out []model.ContainerPort
	seen := map[int]bool{}
	for _, m := range publishedRe.FindAllStringSubmatch(s, -1) {
		host, err := strconv.Atoi(m[1])
		if err != nil || seen[host] {
			continue
		}
		target, _ := strconv.Atoi(m[2])
		seen[host] = true
		out = append(out, model.ContainerPort{Host: host, Container: target, Protocol: m[3]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

func labelValue(labels, key string) string {
	for _, kv := range strings.Split(labels, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.TrimSpace(k) == key {
			return v
		}
	}
	return ""
}

func firstName(names string) string {
	if i := strings.Index(names, ","); i >= 0 {
		return names[:i]
	}
	return names
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (c *Client) composeArgs(composeFile string, extraFiles []string, rest ...string) []string {
	args := []string{"compose"}
	if composeFile != "" {
		args = append(args, "--file", composeFile)
		// Pin the base directory for every relative path in the project —
		// build contexts, env_file, volumes.
		//
		// Compose normally infers it from the first --file, but the agent adds
		// a generated override from its cache directory, and how a given
		// Compose version combines several -f files from different directories
		// is exactly the sort of thing that changes between releases. Stating
		// it removes the question: it is always the directory holding the
		// project's own compose file, which is what you get by hand.
		args = append(args, "--project-directory", filepath.Dir(composeFile))
	}
	// Extra files are layered after the project's own, which is how a port
	// override takes effect without touching anything inside the repository.
	for _, f := range extraFiles {
		args = append(args, "--file", f)
	}
	// --ansi never keeps progress output parseable in logs.
	args = append(args, "--ansi", "never")
	return append(args, rest...)
}

func (c *Client) run(ctx context.Context, dir string, timeout time.Duration, env []string, args ...string) (stdout []byte, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), env...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		err = fmt.Errorf("docker %s timed out after %s", strings.Join(args, " "), timeout)
	}
	return outBuf.Bytes(), errBuf.String(), err
}

// cleanEnv keeps the user's docker configuration (DOCKER_HOST, contexts,
// credential helpers) but disables interactive prompts and telemetry noise.
func cleanEnv() []string {
	env := append([]string{}, os.Environ()...)
	return append(env,
		"COMPOSE_INTERACTIVE_NO_CLI=1",
		"DOCKER_CLI_HINTS=false",
		"BUILDKIT_PROGRESS=plain",
	)
}

func composeError(err error, stderr string) error {
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		return err
	}
	// Compose errors are multi-line; the last non-empty line is usually the
	// actionable one, but keep a couple for context.
	lines := []string{}
	for _, l := range strings.Split(msg, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return fmt.Errorf("%s", strings.Join(lines, "; "))
}

func combined(stdout []byte, stderr string) string {
	var b strings.Builder
	b.Write(stdout)
	if stderr != "" {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString(stderr)
	}
	return b.String()
}
