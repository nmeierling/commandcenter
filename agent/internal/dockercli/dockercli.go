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
	"regexp"
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
func (c *Client) Ps(ctx context.Context, dir, composeFile string) (*PsResult, error) {
	args := c.composeArgs(composeFile, "ps", "--all", "--format", "json")
	out, stderr, err := c.run(ctx, dir, 20*time.Second, args...)
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
		res.Containers = append(res.Containers, model.Container{
			ID:      shortID(e.ID),
			Name:    e.Name,
			Service: e.Service,
			State:   strings.ToLower(e.State),
			Health:  strings.ToLower(e.Health),
			Status:  e.Status,
			Image:   e.Image,
		})
		if strings.ToLower(e.State) != "running" {
			continue
		}
		for _, p := range e.Publishers {
			if p.PublishedPort > 0 && !strings.EqualFold(p.Protocol, "udp") {
				res.Published[p.PublishedPort] = e.Service
			}
		}
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

// Up starts a compose project detached.
func (c *Client) Up(ctx context.Context, dir, composeFile string) (string, error) {
	args := c.composeArgs(composeFile, "up", "--detach", "--remove-orphans")
	out, stderr, err := c.run(ctx, dir, 15*time.Minute, args...)
	return combined(out, stderr), composeError(err, stderr)
}

// Down stops and removes a compose project.
func (c *Client) Down(ctx context.Context, dir, composeFile string) (string, error) {
	args := c.composeArgs(composeFile, "down", "--remove-orphans")
	out, stderr, err := c.run(ctx, dir, 5*time.Minute, args...)
	return combined(out, stderr), composeError(err, stderr)
}

// Restart restarts a compose project's containers.
func (c *Client) Restart(ctx context.Context, dir, composeFile string) (string, error) {
	args := c.composeArgs(composeFile, "restart")
	out, stderr, err := c.run(ctx, dir, 10*time.Minute, args...)
	return combined(out, stderr), composeError(err, stderr)
}

// Logs returns the last n lines of a project's combined container logs.
func (c *Client) Logs(ctx context.Context, dir, composeFile string, tail int) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	args := c.composeArgs(composeFile, "logs", "--no-color", "--tail", strconv.Itoa(tail))
	out, stderr, err := c.run(ctx, dir, 30*time.Second, args...)
	if err != nil {
		return combined(out, stderr), composeError(err, stderr)
	}
	return string(out), nil
}

// FollowLogs streams logs until the context is cancelled. The caller must close
// the returned reader, which also terminates the underlying process.
func (c *Client) FollowLogs(ctx context.Context, dir, composeFile string, tail int) (io.ReadCloser, error) {
	if tail <= 0 {
		tail = 100
	}
	args := c.composeArgs(composeFile, "logs", "--no-color", "--follow", "--tail", strconv.Itoa(tail))
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
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

// ContainerOwners maps published host ports to the container publishing them,
// across all running containers. This is how we attribute a port to a container
// rather than to the docker-proxy process that actually holds the socket.
func (c *Client) ContainerOwners(ctx context.Context) (map[int]*model.PortOwner, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin(), "ps", "--format", "{{json .}}")
	cmd.Env = cleanEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	owners := map[int]*model.PortOwner{}
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
		}
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		owner := &model.PortOwner{
			ContainerID:    shortID(e.ID),
			Container:      firstName(e.Names),
			ComposeProject: labelValue(e.Labels, "com.docker.compose.project"),
			Command:        e.Image,
		}
		for _, p := range parsePublished(e.Ports) {
			if _, dup := owners[p]; !dup {
				owners[p] = owner
			}
		}
	}
	return owners, sc.Err()
}

// publishedRe matches the host side of `docker ps` port strings such as
// "0.0.0.0:3000->3000/tcp" and "[::]:3000->3000/tcp".
var publishedRe = regexp.MustCompile(`(?:^|,\s*)(?:\[[^\]]*\]|[0-9.]+):(\d+)->`)

func parsePublished(s string) []int {
	var out []int
	for _, m := range publishedRe.FindAllStringSubmatch(s, -1) {
		if p, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, p)
		}
	}
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

func (c *Client) composeArgs(composeFile string, rest ...string) []string {
	args := []string{"compose"}
	if composeFile != "" {
		args = append(args, "--file", composeFile)
	}
	// --ansi never keeps progress output parseable in logs.
	args = append(args, "--ansi", "never")
	return append(args, rest...)
}

func (c *Client) run(ctx context.Context, dir string, timeout time.Duration, args ...string) (stdout []byte, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
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
