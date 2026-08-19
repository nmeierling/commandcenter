// Package procmgr supervises non-Docker projects: dev servers started from a
// shell command.
//
// Three things make this harder than exec.Command:
//
//   - `npm run dev` spawns children, so signalling the shell alone leaves the
//     real server orphaned and the port still bound. We put every project in
//     its own process group and signal the group.
//   - The agent restarts (crash, upgrade, logout). Child output therefore goes
//     to a log file, not a pipe: a pipe whose reader died gives the child EPIPE
//     and often kills it. With a file, supervised processes survive us, and we
//     re-adopt them on the next start.
//   - PIDs are reused. We record the kernel's start time for each PID and
//     verify it before signalling anything, so a recycled PID can never be
//     mistaken for a project we own.
package procmgr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/devcc/agent/internal/logbuf"
	"github.com/devcc/agent/internal/model"
)

// Spec describes a process to supervise.
type Spec struct {
	ID      string
	Dir     string
	Command string
	Env     map[string]string
}

// Proc is a supervised process.
type Proc struct {
	ID        string            `json:"id"`
	PID       int               `json:"pid"`
	PGID      int               `json:"pgid"`
	Command   string            `json:"command"`
	Dir       string            `json:"dir"`
	Env       map[string]string `json:"env,omitempty"`
	StartedAt time.Time         `json:"startedAt"`
	LogPath   string            `json:"logPath"`
	// Argv is what we actually exec'd, kept for display and debugging.
	Argv []string `json:"argv"`
	// StartToken is the kernel's start time for this PID. It is how a restarted
	// agent tells "my dev server is still running" from "that PID belongs to
	// something else now".
	StartToken string `json:"startToken,omitempty"`

	// Runtime-only fields.
	cmd      *exec.Cmd
	adopted  bool
	exited   bool
	exitCode *int
	exitErr  string
	stopping bool
	tailStop chan struct{}
}

// Manager owns all supervised processes.
type Manager struct {
	stateDir string
	mu       sync.Mutex
	procs    map[string]*Proc
	// buffers is supplied by the caller so log output has somewhere to go.
	buffers func(id string) *logbuf.Buffer
	onExit  func(id string)
}

// New returns a manager persisting its state under stateDir.
func New(stateDir string, buffers func(id string) *logbuf.Buffer) (*Manager, error) {
	if err := os.MkdirAll(filepath.Join(stateDir, "logs"), 0o700); err != nil {
		return nil, err
	}
	m := &Manager{stateDir: stateDir, procs: map[string]*Proc{}, buffers: buffers}
	return m, nil
}

// OnExit registers a callback fired when a supervised process exits, so the
// status reconciler can refresh immediately instead of waiting for its tick.
func (m *Manager) OnExit(fn func(id string)) { m.onExit = fn }

var errNotSupervised = errors.New("project is not under process supervision")

// ErrAlreadyRunning is returned when starting an already-running process.
var ErrAlreadyRunning = errors.New("already running")

// Start launches the command in its own process group.
func (m *Manager) Start(spec Spec) (*Proc, error) {
	if spec.Command == "" {
		return nil, errors.New("no start command configured")
	}
	if st, err := os.Stat(spec.Dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("project directory %s is not accessible", spec.Dir)
	}

	m.mu.Lock()
	if existing, ok := m.procs[spec.ID]; ok && existing.alive() {
		m.mu.Unlock()
		return existing, ErrAlreadyRunning
	}
	m.mu.Unlock()

	logPath := filepath.Join(m.stateDir, "logs", spec.ID+".log")
	if err := rotate(logPath); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	argv := shellArgv(spec.Command)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = buildEnv(spec.Env)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start %q: %w", spec.Command, err)
	}
	_ = logFile.Close() // the child holds its own descriptor

	p := &Proc{
		ID:         spec.ID,
		PID:        cmd.Process.Pid,
		PGID:       processGroup(cmd.Process.Pid),
		Command:    spec.Command,
		Dir:        spec.Dir,
		Env:        spec.Env,
		StartedAt:  time.Now(),
		LogPath:    logPath,
		Argv:       argv,
		StartToken: processStartToken(cmd.Process.Pid),
		cmd:        cmd,
		tailStop:   make(chan struct{}),
	}

	m.mu.Lock()
	m.procs[spec.ID] = p
	m.mu.Unlock()
	m.persist()

	if buf := m.buffer(spec.ID); buf != nil {
		buf.Reset()
		buf.Append("agent", fmt.Sprintf("$ %s   (pid %d, cwd %s)", spec.Command, p.PID, spec.Dir))
		go tailFile(logPath, buf, p.tailStop)
	}
	go m.wait(p)
	return p, nil
}

func (m *Manager) wait(p *Proc) {
	err := p.cmd.Wait()

	m.mu.Lock()
	p.exited = true
	code := p.cmd.ProcessState.ExitCode()
	p.exitCode = &code
	if err != nil && code <= 0 {
		p.exitErr = err.Error()
	}
	stopping := p.stopping
	m.mu.Unlock()

	if buf := m.buffer(p.ID); buf != nil {
		if stopping {
			buf.Append("agent", fmt.Sprintf("[process stopped, exit code %d]", code))
		} else {
			buf.Append("agent", fmt.Sprintf("[process exited unexpectedly, exit code %d]", code))
		}
	}
	// Give the tailer a moment to drain the file before it stops.
	time.AfterFunc(1500*time.Millisecond, func() { p.stopTail() })
	m.persist()
	if m.onExit != nil {
		m.onExit(p.ID)
	}
}

// Stop signals the whole process group: SIGTERM, then SIGKILL after grace.
func (m *Manager) Stop(id string, grace time.Duration) error {
	m.mu.Lock()
	p, ok := m.procs[id]
	if !ok {
		m.mu.Unlock()
		return errNotSupervised
	}
	if !p.alive() {
		m.mu.Unlock()
		return nil
	}
	p.stopping = true
	pid, pgid, token := p.PID, p.PGID, p.StartToken
	m.mu.Unlock()

	if buf := m.buffer(id); buf != nil {
		buf.Append("agent", "[sending SIGTERM to process group]")
	}
	if err := terminateGroup(pid, pgid, token); err != nil {
		return err
	}

	if grace <= 0 {
		grace = 10 * time.Second
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		gone := !p.alive()
		m.mu.Unlock()
		if gone {
			m.forget(id)
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}

	if buf := m.buffer(id); buf != nil {
		buf.Append("agent", "[process did not exit in time, sending SIGKILL]")
	}
	if err := killGroup(pid, pgid, token); err != nil {
		return err
	}
	// Wait briefly for the kernel to reap it so status does not flap.
	for i := 0; i < 20; i++ {
		m.mu.Lock()
		gone := !p.alive()
		m.mu.Unlock()
		if gone {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	m.forget(id)
	return nil
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	if p, ok := m.procs[id]; ok && !p.alive() {
		p.stopTail()
	}
	m.mu.Unlock()
	m.persist()
}

// Info reports the current state of a supervised process, or nil.
func (m *Manager) Info(id string) *model.ProcessInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[id]
	if !ok {
		return nil
	}
	info := &model.ProcessInfo{
		PID:           p.PID,
		Command:       p.Command,
		StartedAt:     p.StartedAt,
		Running:       p.alive(),
		ExitError:     p.exitErr,
		StopRequested: p.stopping,
	}
	if !info.Running && p.exitCode != nil && !p.stopping {
		info.ExitCode = p.exitCode
	}
	if p.adopted && info.Running {
		info.Command = p.Command + "  (adopted from a previous agent run)"
	}
	return info
}

// Running reports whether a supervised process for id is alive.
func (m *Manager) Running(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[id]
	return ok && p.alive()
}

// LogPath returns the on-disk log for a project, or "".
func (m *Manager) LogPath(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[id]; ok {
		return p.LogPath
	}
	candidate := filepath.Join(m.stateDir, "logs", id+".log")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// IDs lists supervised project IDs.
func (m *Manager) IDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.procs))
	for id := range m.procs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// StopAll terminates every supervised process, used on agent shutdown when the
// user asked for it. By default the agent leaves processes running.
func (m *Manager) StopAll(grace time.Duration) {
	for _, id := range m.IDs() {
		_ = m.Stop(id, grace)
	}
}

func (p *Proc) alive() bool {
	if p.exited {
		return false
	}
	if !p.adopted {
		// We forked this process and have not reaped it yet, so the kernel is
		// holding its PID: it cannot have been reused, and the wait() goroutine
		// is what flips `exited`. Probing /proc here would only introduce a
		// race where a healthy process briefly looks dead.
		return true
	}
	return pidAlive(p.PID, p.StartToken)
}

func (p *Proc) stopTail() {
	select {
	case <-p.tailStop:
	default:
		if p.tailStop != nil {
			close(p.tailStop)
		}
	}
}

func (m *Manager) buffer(id string) *logbuf.Buffer {
	if m.buffers == nil {
		return nil
	}
	return m.buffers(id)
}

func buildEnv(extra map[string]string) []string {
	env := os.Environ()
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	// Dev servers behave better when they know they are not on a TTY.
	env = append(env, "CI=", "FORCE_COLOR=0", "NO_COLOR=1")
	return env
}

// rotate keeps one previous log so a crash loop does not erase the evidence.
func rotate(path string) error {
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		return nil
	}
	return os.Rename(path, path+".1")
}

// --- persistence -----------------------------------------------------------

type persisted struct {
	Procs []*Proc `json:"procs"`
}

func (m *Manager) stateFile() string { return filepath.Join(m.stateDir, "processes.json") }

func (m *Manager) persist() {
	m.mu.Lock()
	var out persisted
	for _, p := range m.procs {
		if p.alive() {
			out.Procs = append(out.Procs, p)
		}
	}
	m.mu.Unlock()

	sort.Slice(out.Procs, func(i, j int) bool { return out.Procs[i].ID < out.Procs[j].ID })
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	tmp := m.stateFile() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, m.stateFile())
}

// Adopt reloads processes recorded by a previous agent run. Entries whose PID
// is gone, or reused by an unrelated process, are discarded.
func (m *Manager) Adopt() (adopted, stale int) {
	raw, err := os.ReadFile(m.stateFile())
	if err != nil {
		return 0, 0
	}
	var state persisted
	if err := json.Unmarshal(raw, &state); err != nil {
		return 0, 0
	}
	for _, p := range state.Procs {
		if !pidAlive(p.PID, p.StartToken) {
			stale++
			continue
		}
		p.adopted = true
		p.tailStop = make(chan struct{})
		m.mu.Lock()
		m.procs[p.ID] = p
		m.mu.Unlock()
		if buf := m.buffer(p.ID); buf != nil {
			buf.Append("agent", fmt.Sprintf("[re-attached to running process pid %d started %s]",
				p.PID, p.StartedAt.Format(time.RFC3339)))
			go tailFile(p.LogPath, buf, p.tailStop)
		}
		go m.watchAdopted(p)
		adopted++
	}
	m.persist()
	return adopted, stale
}

// watchAdopted polls an adopted process, since we cannot wait() on a process we
// did not fork.
func (m *Manager) watchAdopted(p *Proc) {
	for {
		time.Sleep(2 * time.Second)
		m.mu.Lock()
		alive := p.alive()
		m.mu.Unlock()
		if alive {
			continue
		}
		m.mu.Lock()
		p.exited = true
		m.mu.Unlock()
		if buf := m.buffer(p.ID); buf != nil {
			buf.Append("agent", "[adopted process exited]")
		}
		p.stopTail()
		m.persist()
		if m.onExit != nil {
			m.onExit(p.ID)
		}
		return
	}
}
