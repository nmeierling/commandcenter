// Package core owns the project registry and the background status
// reconciler. Everything the API serves is read from here; nothing in the API
// layer talks to docker, the filesystem or processes directly.
package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devcc/agent/internal/composefile"
	"github.com/devcc/agent/internal/config"
	"github.com/devcc/agent/internal/discovery"
	"github.com/devcc/agent/internal/dockercli"
	"github.com/devcc/agent/internal/health"
	"github.com/devcc/agent/internal/logbuf"
	"github.com/devcc/agent/internal/model"
	"github.com/devcc/agent/internal/openurl"
	"github.com/devcc/agent/internal/ports"
	"github.com/devcc/agent/internal/procmgr"
)

// Version is stamped into the agent status payload.
var Version = "1.0.0"

// entry is a project plus everything mutable about it.
type entry struct {
	project model.Project
	state   model.State
	logs    *logbuf.Buffer

	// refreshing serialises status refresh for this project so a slow
	// `docker compose ps` cannot pile up behind itself.
	refreshing sync.Mutex
	// startedAt is when we last issued a start, used for the settle window
	// during which a failing health check means "starting", not "unhealthy".
	startedAt time.Time
}

// Manager is the agent's brain.
type Manager struct {
	cfg     config.Config
	docker  *dockercli.Client
	procs   *procmgr.Manager
	checker *health.Checker
	hub     *hub

	mu      sync.RWMutex
	byID    map[string]*entry
	order   []string
	scanErr string

	sem chan struct{}

	daemonOK atomicBool
}

// New builds a manager. stateDir holds supervised-process bookkeeping and logs.
func New(cfg config.Config, stateDir string) (*Manager, error) {
	m := &Manager{
		cfg:     cfg,
		docker:  dockercli.New(),
		checker: health.New(cfg.Poll.HealthTimeout),
		hub:     newHub(),
		byID:    map[string]*entry{},
		sem:     make(chan struct{}, max(1, cfg.Poll.MaxConcurrent)),
	}

	procs, err := procmgr.New(stateDir, m.logBuffer)
	if err != nil {
		return nil, err
	}
	m.procs = procs
	m.procs.OnExit(func(id string) { go m.RefreshOne(context.Background(), id) })
	return m, nil
}

// Start performs the first discovery and launches background loops.
func (m *Manager) Start(ctx context.Context) {
	m.Discover()

	adopted, stale := m.procs.Adopt()
	if adopted > 0 || stale > 0 {
		// Re-run discovery so adopted processes attach to their project cards.
		m.RefreshAll(ctx)
	}

	go m.reconcileLoop(ctx)
	go m.discoverLoop(ctx)
	go m.daemonLoop(ctx)
}

// --- registry --------------------------------------------------------------

// Discover rescans roots and merges in configured projects.
func (m *Manager) Discover() {
	var found []model.Project
	if m.cfg.ScanEnabled() {
		found = discovery.NewScanner(m.cfg.Scan).Scan()
	}

	byPath := map[string]*model.Project{}
	var order []string
	for i := range found {
		p := found[i]
		byPath[p.Path] = &p
		order = append(order, p.Path)
	}

	var scanErrs []string
	for _, cp := range m.cfg.Projects {
		base, ok := byPath[cp.Path]
		if !ok {
			// A configured path that discovery did not reach: inspect it
			// directly so we still know what technology it uses.
			p, detected := discovery.Inspect(cp.Path, nil)
			if !detected {
				if _, err := os.Stat(cp.Path); err != nil {
					scanErrs = append(scanErrs, fmt.Sprintf("configured project %q: %s is not accessible", cp.Name, cp.Path))
					continue
				}
				p = model.Project{
					ID:      model.ID(cp.Path),
					Name:    filepath.Base(cp.Path),
					Path:    cp.Path,
					Kind:    model.KindCustom,
					Runtime: model.RuntimeNone,
				}
			}
			p.Source = "configured"
			byPath[cp.Path] = &p
			order = append(order, cp.Path)
			base = &p
		} else {
			base.Source = "both"
		}
		applyOverrides(base, cp)
	}

	m.mu.Lock()
	// Preserve state and log buffers for projects we already knew about.
	newByID := map[string]*entry{}
	var newOrder []string
	for _, path := range order {
		p := byPath[path]
		if p == nil || p.Disabled {
			continue
		}
		e, ok := m.byID[p.ID]
		if !ok {
			e = &entry{logs: logbuf.New(4000), state: model.State{Status: model.StatusUnknown}}
		}
		e.project = *p
		newByID[p.ID] = e
		newOrder = append(newOrder, p.ID)
	}
	m.byID = newByID
	m.order = newOrder
	m.scanErr = strings.Join(scanErrs, "; ")
	m.mu.Unlock()

	m.hub.publish(Event{Type: EventProjects, Projects: m.Views()})
}

// applyOverrides layers a config entry onto a discovered project. Only fields
// the user actually set take effect, so config stays a patch rather than a
// full redefinition.
func applyOverrides(p *model.Project, cp config.Project) {
	if cp.Name != "" {
		p.Name = cp.Name
	}
	if cp.Kind != "" {
		p.Kind = model.Kind(cp.Kind)
	}
	if cp.ComposeFile != "" {
		p.ComposeFile = cp.ComposeFile
		p.Runtime = model.RuntimeCompose
	}
	if cp.Start != "" {
		// An explicit start command wins over compose: the user is telling us
		// how they actually run this project.
		p.StartCommand = cp.Start
		p.Runtime = model.RuntimeProcess
	}
	if cp.Stop != "" {
		p.StopCommand = cp.Stop
	}
	if len(cp.Env) > 0 {
		p.Env = cp.Env
	}
	if len(cp.Ports) > 0 {
		p.Ports = nil
		for _, port := range cp.Ports {
			p.Ports = append(p.Ports, model.Port{Port: port, Source: "config", State: model.PortFree})
		}
	}
	if len(cp.URLs) > 0 {
		p.URLs = cp.URLs
	} else if len(p.URLs) == 0 && len(p.Ports) > 0 {
		p.URLs = []string{"http://localhost:" + strconv.Itoa(p.Ports[0].Port)}
	}
	if cp.Health != nil && cp.Health.URL != "" {
		p.Health = &model.HealthCheck{
			URL:            cp.Health.URL,
			ExpectStatus:   cp.Health.ExpectStatus,
			Timeout:        cp.Health.Timeout,
			TimeoutSeconds: cp.Health.Timeout.Seconds(),
		}
	}
	p.Disabled = cp.Disabled
}

// Views returns every project with its current state.
func (m *Manager) Views() []model.View {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.View, 0, len(m.order))
	for _, id := range m.order {
		if e, ok := m.byID[id]; ok {
			out = append(out, normalize(model.View{Project: e.project, State: e.state}))
		}
	}
	return out
}

// normalize replaces nil slices with empty ones. JSON null and [] mean the same
// thing to Go but not to the UI, and a client should not have to guard every
// list it iterates.
func normalize(v model.View) model.View {
	if v.Ports == nil {
		v.Ports = []model.Port{}
	}
	if v.URLs == nil {
		v.URLs = []string{}
	}
	if v.State.Ports == nil {
		v.State.Ports = []model.Port{}
	}
	if v.State.Containers == nil {
		v.State.Containers = []model.Container{}
	}
	return v
}

// View returns a single project, or false if unknown.
func (m *Manager) View(id string) (model.View, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byID[id]
	if !ok {
		return model.View{}, false
	}
	return normalize(model.View{Project: e.project, State: e.state}), true
}

// ErrUnknownProject is returned for an unrecognised project ID.
var ErrUnknownProject = errors.New("unknown project")

func (m *Manager) entry(id string) (*entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byID[id]
	if !ok {
		return nil, ErrUnknownProject
	}
	return e, nil
}

func (m *Manager) logBuffer(id string) *logbuf.Buffer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, ok := m.byID[id]; ok {
		return e.logs
	}
	return nil
}

// Logs returns the in-memory log buffer for a project.
func (m *Manager) Logs(id string) (*logbuf.Buffer, error) {
	e, err := m.entry(id)
	if err != nil {
		return nil, err
	}
	return e.logs, nil
}

// Subscribe registers an SSE client.
func (m *Manager) Subscribe() (<-chan Event, func()) { return m.hub.subscribe() }

// --- background loops ------------------------------------------------------

func (m *Manager) reconcileLoop(ctx context.Context) {
	interval := m.cfg.Poll.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	m.RefreshAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.RefreshAll(ctx)
		}
	}
}

func (m *Manager) discoverLoop(ctx context.Context) {
	every := m.cfg.Poll.DiscoverEvery
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Discover()
		}
	}
}

// daemonLoop tracks docker daemon availability out of band, so a stopped
// daemon does not add its timeout to every status refresh.
func (m *Manager) daemonLoop(ctx context.Context) {
	if ok, _, _ := m.docker.Available(); !ok {
		return
	}
	check := func() { m.daemonOK.set(m.docker.DaemonUp(ctx)) }
	check()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// RefreshAll updates every project's status concurrently, bounded by the
// configured limit. One expensive snapshot of listening ports and container
// ownership is shared across all projects.
func (m *Manager) RefreshAll(ctx context.Context) {
	snap := m.snapshot(ctx)

	m.mu.RLock()
	ids := append([]string{}, m.order...)
	m.mu.RUnlock()

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			select {
			case m.sem <- struct{}{}:
				defer func() { <-m.sem }()
			case <-ctx.Done():
				return
			}
			m.refresh(ctx, id, snap)
		}(id)
	}
	wg.Wait()
}

// RefreshOne updates a single project immediately.
func (m *Manager) RefreshOne(ctx context.Context, id string) (model.View, error) {
	if _, err := m.entry(id); err != nil {
		return model.View{}, err
	}
	m.refresh(ctx, id, m.snapshot(ctx))
	v, _ := m.View(id)
	return v, nil
}

// hostSnapshot bundles the machine-wide facts gathered once per cycle.
type hostSnapshot struct {
	tcp             *ports.Snapshot
	containerOwners map[int]*model.PortOwner
}

func (m *Manager) snapshot(ctx context.Context) *hostSnapshot {
	s := &hostSnapshot{tcp: ports.Take(), containerOwners: map[int]*model.PortOwner{}}
	if m.daemonOK.get() {
		if owners, err := m.docker.ContainerOwners(ctx); err == nil {
			s.containerOwners = owners
		}
	}
	return s
}

func (m *Manager) refresh(ctx context.Context, id string, snap *hostSnapshot) {
	e, err := m.entry(id)
	if err != nil {
		return
	}
	// Single-flight: if a refresh for this project is already running, skip
	// rather than queue. The next tick will pick up whatever changed.
	if !e.refreshing.TryLock() {
		return
	}
	defer e.refreshing.Unlock()

	m.mu.RLock()
	p := e.project
	busy := e.state.Busy
	startedAt := e.startedAt
	m.mu.RUnlock()

	next := model.State{UpdatedAt: time.Now(), Busy: busy}

	switch p.Runtime {
	case model.RuntimeCompose:
		m.refreshCompose(ctx, &p, &next)
	case model.RuntimeProcess:
		m.refreshProcess(&p, &next)
	}

	next.Ports = m.resolvePorts(&p, &next, snap)
	m.deriveStatus(ctx, &p, &next, startedAt)

	m.mu.Lock()
	e.state = next
	m.mu.Unlock()

	view := normalize(model.View{Project: p, State: next})
	m.hub.publish(Event{Type: EventProject, Project: &view})
}

func (m *Manager) refreshCompose(ctx context.Context, p *model.Project, st *model.State) {
	if ok, _, err := m.docker.Available(); !ok {
		st.Status = model.StatusUnknown
		st.Detail = "docker compose is not installed"
		if err != nil {
			st.Error = err.Error()
		}
		return
	}
	if !m.daemonOK.get() {
		st.Status = model.StatusUnknown
		st.Detail = "docker daemon is not running"
		return
	}

	res, err := m.docker.Ps(ctx, p.Path, p.ComposeFile)
	if err != nil {
		st.Status = model.StatusError
		st.Error = err.Error()
		return
	}
	st.Containers = res.Containers

	// Ports learned from running containers are authoritative; merge them into
	// the expected set so a port defined only at runtime still shows up.
	for port, svc := range res.Published {
		if !hasPort(p.Ports, port) {
			p.Ports = append(p.Ports, model.Port{Port: port, Source: "observed", Service: svc})
		}
	}
	sort.Slice(p.Ports, func(i, j int) bool { return p.Ports[i].Port < p.Ports[j].Port })
}

func (m *Manager) refreshProcess(p *model.Project, st *model.State) {
	st.Process = m.procs.Info(p.ID)
}

// resolvePorts decides, for each expected port, whether it is free, ours, or
// held by something else. Getting this right is the difference between "port
// 3000 is busy" and "port 3000 is busy because your other project owns it".
func (m *Manager) resolvePorts(p *model.Project, st *model.State, snap *hostSnapshot) []model.Port {
	out := make([]model.Port, 0, len(p.Ports))
	for _, port := range p.Ports {
		res := model.Port{Port: port.Port, Source: port.Source, Service: port.Service, State: model.PortFree}

		containerOwner := snap.containerOwners[port.Port]
		processOwner := snap.tcp.Owner(port.Port)
		listening := snap.tcp.Listening(port.Port) || containerOwner != nil

		switch {
		case !listening:
			res.State = model.PortFree
		case containerOwner != nil:
			res.Owner = containerOwner
			if p.Runtime == model.RuntimeCompose && containerOwner.ComposeProject != "" &&
				containerOwner.ComposeProject == p.ComposeProject {
				res.State = model.PortOwned
			} else {
				res.State = model.PortConflict
			}
		case processOwner != nil:
			res.Owner = processOwner
			if p.Runtime == model.RuntimeProcess && m.procs.OwnsPID(p.ID, processOwner.PID) {
				res.State = model.PortOwned
			} else {
				res.State = model.PortConflict
			}
		default:
			// Listening, but we could not attribute it: usually another user's
			// process, or docker publishing through iptables with no host-side
			// listener we can see.
			if st.Process != nil && st.Process.Running {
				res.State = model.PortOwned
			} else if p.Runtime == model.RuntimeCompose && anyContainerRunning(st.Containers) {
				res.State = model.PortOwned
			} else {
				res.State = model.PortUnknownOwner
			}
		}
		out = append(out, res)
	}
	return out
}

func anyContainerRunning(cs []model.Container) bool {
	for _, c := range cs {
		if c.State == "running" {
			return true
		}
	}
	return false
}

// deriveStatus folds every signal into the single state the dashboard shows.
func (m *Manager) deriveStatus(ctx context.Context, p *model.Project, st *model.State, startedAt time.Time) {
	// A lifecycle action in flight always wins: the user just clicked
	// something and wants to see it happening.
	switch st.Busy {
	case "start", "restart":
		st.Status = model.StatusStarting
		return
	case "stop":
		st.Status = model.StatusStopping
		return
	}
	if st.Status == model.StatusError || (st.Status == model.StatusUnknown && st.Detail != "") {
		return // already decided by the runtime-specific refresh
	}

	running := false
	switch p.Runtime {
	case model.RuntimeCompose:
		total, up, exitedBad := 0, 0, 0
		for _, c := range st.Containers {
			total++
			switch c.State {
			case "running":
				up++
			case "exited", "dead":
				exitedBad++
			}
		}
		running = up > 0
		switch {
		case total == 0:
			st.Status = model.StatusStopped
			st.Detail = "no containers"
		case up == total:
			st.Status = model.StatusRunning
			st.Detail = plural(up, "container")
		case up == 0:
			st.Status = model.StatusStopped
			st.Detail = plural(total, "container") + " stopped"
		default:
			st.Status = model.StatusUnhealthy
			st.Detail = fmt.Sprintf("%d of %d containers running", up, total)
		}
		if unhealthy := unhealthyContainers(st.Containers); unhealthy != "" && st.Status == model.StatusRunning {
			st.Status = model.StatusUnhealthy
			st.Detail = unhealthy
		}

	case model.RuntimeProcess:
		if st.Process != nil && st.Process.Running {
			running = true
			st.Status = model.StatusRunning
			st.Detail = "pid " + strconv.Itoa(st.Process.PID)
		} else {
			st.Status = model.StatusStopped
			switch {
			case st.Process == nil:
				st.Detail = "not started"
			case st.Process.StopRequested:
				st.Detail = "stopped"
			case st.Process.ExitCode != nil && *st.Process.ExitCode != 0:
				// Crashed on its own. This is the case worth surfacing loudly:
				// the dev server died and the developer has not noticed yet.
				st.Status = model.StatusError
				st.Detail = fmt.Sprintf("exited with code %d — see logs", *st.Process.ExitCode)
			default:
				st.Detail = "stopped"
			}
		}

	default:
		// Not controllable: infer purely from ports and health.
		if anyPortOwned(st.Ports) {
			running = true
			st.Status = model.StatusRunning
		} else {
			st.Status = model.StatusStopped
		}
	}

	// Port conflicts matter most when the project is *not* running: that is
	// the case where starting it will fail. A crash keeps its own status
	// though — the exit code is the cause, the busy port only the symptom.
	if !running {
		if conflict := firstConflict(st.Ports); conflict != nil {
			if st.Status == model.StatusError {
				st.Detail += "; " + conflictDetail(conflict)
			} else {
				st.Status = model.StatusConflict
				st.Detail = conflictDetail(conflict)
			}
		}
	}

	if !running || p.Health == nil || p.Health.URL == "" {
		return
	}
	st.Health = m.checker.Check(ctx, p.Health)
	if st.Health.OK {
		return
	}
	// Within the settle window after a start, a failing probe means the app is
	// still booting, not that it is broken.
	settle := m.cfg.Poll.StartupSettleFor
	if !startedAt.IsZero() && time.Since(startedAt) < settle {
		st.Status = model.StatusStarting
		st.Detail = "waiting for health check"
		return
	}
	st.Status = model.StatusUnhealthy
	if st.Health.Error != "" {
		st.Detail = "health check: " + st.Health.Error
	}
}

func unhealthyContainers(cs []model.Container) string {
	var bad []string
	for _, c := range cs {
		if c.Health == "unhealthy" || c.Health == "starting" {
			bad = append(bad, c.Service+" "+c.Health)
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return strings.Join(bad, ", ")
}

func conflictDetail(p *model.Port) string {
	if p.Owner == nil {
		return fmt.Sprintf("port %d is occupied by an unidentified process", p.Port)
	}
	switch {
	case p.Owner.ComposeProject != "":
		return fmt.Sprintf("port %d is occupied by compose project %q (container %s)",
			p.Port, p.Owner.ComposeProject, p.Owner.Container)
	case p.Owner.Container != "":
		return fmt.Sprintf("port %d is occupied by container %s", p.Port, p.Owner.Container)
	case p.Owner.Process != "":
		return fmt.Sprintf("port %d is occupied by %s (pid %d)", p.Port, p.Owner.Process, p.Owner.PID)
	default:
		return fmt.Sprintf("port %d is occupied by pid %d", p.Port, p.Owner.PID)
	}
}

func firstConflict(ps []model.Port) *model.Port {
	for i := range ps {
		if ps[i].State == model.PortConflict {
			return &ps[i]
		}
	}
	return nil
}

func anyPortOwned(ps []model.Port) bool {
	for _, p := range ps {
		if p.State == model.PortOwned || p.State == model.PortUnknownOwner {
			return true
		}
	}
	return false
}

func hasPort(ps []model.Port, port int) bool {
	for _, p := range ps {
		if p.Port == port {
			return true
		}
	}
	return false
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// --- lifecycle actions -----------------------------------------------------

// ErrBusy means another lifecycle action is already running for this project.
var ErrBusy = errors.New("another action is already running for this project")

// ErrNotControllable means we have no way to start or stop this project.
var ErrNotControllable = errors.New("this project has no start mechanism; set a start command in the config file")

// Action runs start, stop or restart. It returns once the underlying command
// finishes, so the HTTP handler can report the real error; the UI keeps the
// card marked busy in the meantime via events.
func (m *Manager) Action(ctx context.Context, id, action string) (model.View, error) {
	e, err := m.entry(id)
	if err != nil {
		return model.View{}, err
	}

	m.mu.Lock()
	if e.state.Busy != "" {
		m.mu.Unlock()
		return model.View{}, ErrBusy
	}
	p := e.project
	if p.Runtime == model.RuntimeNone {
		m.mu.Unlock()
		return model.View{}, ErrNotControllable
	}
	e.state.Busy = action
	e.state.Error = ""
	switch action {
	case "start", "restart":
		e.state.Status = model.StatusStarting
		e.state.Detail = action + "ing…"
		e.startedAt = time.Now()
	case "stop":
		e.state.Status = model.StatusStopping
		e.state.Detail = "stopping…"
	}
	view := normalize(model.View{Project: p, State: e.state})
	m.mu.Unlock()
	m.hub.publish(Event{Type: EventProject, Project: &view})

	actionErr := m.runAction(ctx, e, p, action)

	m.mu.Lock()
	e.state.Busy = ""
	if actionErr != nil {
		e.state.Error = actionErr.Error()
	}
	m.mu.Unlock()

	m.refresh(ctx, id, m.snapshot(ctx))
	out, _ := m.View(id)
	if actionErr != nil {
		return out, actionErr
	}
	return out, nil
}

func (m *Manager) runAction(ctx context.Context, e *entry, p model.Project, action string) error {
	e.logs.Append("agent", fmt.Sprintf("--- %s %s ---", action, p.Name))

	switch p.Runtime {
	case model.RuntimeCompose:
		return m.runComposeAction(ctx, e, p, action)
	case model.RuntimeProcess:
		return m.runProcessAction(ctx, e, p, action)
	default:
		return ErrNotControllable
	}
}

func (m *Manager) runComposeAction(ctx context.Context, e *entry, p model.Project, action string) error {
	if ok, _, err := m.docker.Available(); !ok {
		return fmt.Errorf("docker compose is not available: %w", err)
	}
	// Verify the compose file still exists; a moved project should produce a
	// clear message rather than compose's own error.
	if p.ComposeFile != "" {
		if _, err := os.Stat(p.ComposeFile); err != nil {
			return fmt.Errorf("compose file %s is missing", p.ComposeFile)
		}
	} else if composefile.Find(p.Path) == "" {
		return fmt.Errorf("no compose file in %s", p.Path)
	}

	var (
		out string
		err error
	)
	switch action {
	case "start":
		out, err = m.docker.Up(ctx, p.Path, p.ComposeFile)
	case "stop":
		out, err = m.docker.Down(ctx, p.Path, p.ComposeFile)
	case "restart":
		// `compose restart` does not pick up changed compose files or images.
		// `up -d` reconciles instead, which is what a developer clicking
		// Restart almost always wants; fall back to restart for a project that
		// is already fully up.
		out, err = m.docker.Up(ctx, p.Path, p.ComposeFile)
		if err == nil {
			var restartOut string
			restartOut, err = m.docker.Restart(ctx, p.Path, p.ComposeFile)
			out += restartOut
		}
	default:
		return fmt.Errorf("unknown action %q", action)
	}

	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" {
			e.logs.Append("agent", line)
		}
	}
	return err
}

func (m *Manager) runProcessAction(ctx context.Context, e *entry, p model.Project, action string) error {
	grace := 10 * time.Second
	switch action {
	case "stop":
		return m.stopProcess(ctx, p, grace)
	case "start":
		return m.startProcess(p)
	case "restart":
		if err := m.stopProcess(ctx, p, grace); err != nil {
			return err
		}
		// Give the OS a moment to release the listening socket, otherwise the
		// restarted server frequently fails with EADDRINUSE.
		time.Sleep(400 * time.Millisecond)
		return m.startProcess(p)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

func (m *Manager) startProcess(p model.Project) error {
	if p.StartCommand == "" {
		return ErrNotControllable
	}
	// Refuse to start into a known conflict: the process would either fail
	// immediately or, worse, silently bind a different port.
	if conflict := m.conflictingPort(p); conflict != nil {
		return fmt.Errorf("cannot start: %s", conflictDetail(conflict))
	}
	_, err := m.procs.Start(procmgr.Spec{
		ID:      p.ID,
		Dir:     p.Path,
		Command: p.StartCommand,
		Env:     p.Env,
	})
	if errors.Is(err, procmgr.ErrAlreadyRunning) {
		return nil
	}
	return err
}

func (m *Manager) stopProcess(ctx context.Context, p model.Project, grace time.Duration) error {
	if p.StopCommand != "" {
		// A custom stop command is the user's own supervision (docker stop,
		// make down, ...); run it and let the process watcher notice the exit.
		return m.runShell(ctx, p, p.StopCommand)
	}
	return m.procs.Stop(p.ID, grace)
}

func (m *Manager) conflictingPort(p model.Project) *model.Port {
	v, ok := m.View(p.ID)
	if !ok {
		return nil
	}
	return firstConflict(v.State.Ports)
}

func (m *Manager) runShell(ctx context.Context, p model.Project, command string) error {
	buf := m.logBuffer(p.ID)
	if buf != nil {
		buf.Append("agent", "$ "+command)
	}
	return procmgr.RunOnce(ctx, p.Path, command, p.Env, buf)
}

// Open launches a project URL in the browser.
func (m *Manager) Open(ctx context.Context, id, url string) (string, error) {
	v, ok := m.View(id)
	if !ok {
		return "", ErrUnknownProject
	}
	if url == "" {
		if len(v.URLs) == 0 {
			return "", errors.New("this project has no known URL; add one under `urls:` in the config file")
		}
		url = v.URLs[0]
	} else if !allowedURL(v, url) {
		// Only URLs the project actually declares may be opened, so this
		// endpoint cannot be turned into a generic "open anything" primitive.
		return "", fmt.Errorf("URL %q is not one of this project's URLs", url)
	}
	if err := openurl.Open(ctx, url); err != nil {
		return url, err
	}
	return url, nil
}

func allowedURL(v model.View, url string) bool {
	for _, u := range v.URLs {
		if u == url {
			return true
		}
	}
	for _, p := range v.State.Ports {
		if strings.HasPrefix(url, "http://localhost:"+strconv.Itoa(p.Port)) ||
			strings.HasPrefix(url, "http://127.0.0.1:"+strconv.Itoa(p.Port)) {
			return true
		}
	}
	return false
}

// KillPortOwner terminates whatever holds a conflicting port. This is the
// "Resolve" action, and it is deliberately explicit rather than automatic: the
// agent never reassigns ports or kills anything without the user asking.
func (m *Manager) KillPortOwner(ctx context.Context, id string, port int, force bool) (string, error) {
	v, ok := m.View(id)
	if !ok {
		return "", ErrUnknownProject
	}
	var target *model.Port
	for i := range v.State.Ports {
		if v.State.Ports[i].Port == port {
			target = &v.State.Ports[i]
		}
	}
	if target == nil {
		return "", fmt.Errorf("port %d is not one of this project's ports", port)
	}
	if target.State != model.PortConflict {
		return "", fmt.Errorf("port %d is not in conflict", port)
	}
	if target.Owner == nil {
		return "", fmt.Errorf("the owner of port %d could not be identified, so there is nothing safe to stop", port)
	}

	if target.Owner.ContainerID != "" {
		if err := procmgr.RunOnce(ctx, v.Path,
			fmt.Sprintf("docker stop %s", target.Owner.ContainerID), nil, m.logBuffer(id)); err != nil {
			return "", err
		}
		return fmt.Sprintf("stopped container %s", target.Owner.Container), nil
	}
	if target.Owner.PID <= 0 {
		return "", errors.New("no PID recorded for the port owner")
	}
	if err := procmgr.KillPID(target.Owner.PID, force); err != nil {
		return "", fmt.Errorf("signal pid %d: %w", target.Owner.PID, err)
	}
	verb := "sent SIGTERM to"
	if force {
		verb = "killed"
	}
	return fmt.Sprintf("%s %s (pid %d)", verb, target.Owner.Process, target.Owner.PID), nil
}

// ComposeLogs returns compose logs for a project, used by the logs endpoint.
func (m *Manager) ComposeLogs(ctx context.Context, id string, tail int) (string, error) {
	v, ok := m.View(id)
	if !ok {
		return "", ErrUnknownProject
	}
	if v.Runtime != model.RuntimeCompose {
		return "", errors.New("not a compose project")
	}
	return m.docker.Logs(ctx, v.Path, v.ComposeFile, tail)
}

// FollowComposeLogs streams compose logs into the project's buffer for as long
// as the context lives. Compose logs are pulled on demand rather than followed
// continuously, so the agent does not hold a `docker compose logs -f` process
// open for every project all day.
func (m *Manager) FollowComposeLogs(ctx context.Context, id string, tail int) error {
	v, ok := m.View(id)
	if !ok {
		return ErrUnknownProject
	}
	if v.Runtime != model.RuntimeCompose {
		return errors.New("not a compose project")
	}
	buf, err := m.Logs(id)
	if err != nil {
		return err
	}
	rc, err := m.docker.FollowLogs(ctx, v.Path, v.ComposeFile, tail)
	if err != nil {
		return err
	}
	go func() {
		defer func() { _ = rc.Close() }()
		buf.Pump("container", rc)
	}()
	return nil
}

// ProcessLogPath exposes the on-disk log for a supervised project.
func (m *Manager) ProcessLogPath(id string) string { return m.procs.LogPath(id) }

// Status describes the agent itself.
func (m *Manager) Status() AgentStatus {
	dockerOK, dockerVer, dockerErr := m.docker.Available()
	m.mu.RLock()
	count := len(m.order)
	scanErr := m.scanErr
	m.mu.RUnlock()

	st := AgentStatus{
		Version:        Version,
		DockerOK:       dockerOK,
		DockerVersion:  dockerVer,
		DaemonOK:       m.daemonOK.get(),
		CanOpenBrowser: openurl.Available(),
		PortOwnersOK:   !ports.Take().Degraded(),
		ConfigPath:     m.cfg.Path(),
		ScanRoots:      m.cfg.Scan.Roots,
		MissingRoots:   m.cfg.MissingRoots(),
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
		ProjectCount:   count,
	}
	if dockerErr != nil {
		st.DockerError = dockerErr.Error()
	}
	if scanErr != "" {
		st.DockerError = strings.TrimSpace(st.DockerError + " " + scanErr)
	}
	return st
}

// StopSupervised terminates all supervised processes, called on shutdown when
// the user passes --stop-on-exit.
func (m *Manager) StopSupervised() { m.procs.StopAll(5 * time.Second) }

// atomicBool is a tiny mutex-free flag used for daemon availability.
type atomicBool struct {
	mu sync.RWMutex
	v  bool
}

func (a *atomicBool) set(v bool) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicBool) get() bool  { a.mu.RLock(); defer a.mu.RUnlock(); return a.v }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
