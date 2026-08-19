package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devcc/agent/internal/config"
	"github.com/devcc/agent/internal/model"
	"github.com/devcc/agent/internal/ports"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	cfg := config.Defaults()
	cfg.Scan.Roots = nil
	m, err := New(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func snapshot(listening []int, owners map[int]*model.PortOwner, containers map[int]*model.PortOwner) *hostSnapshot {
	if containers == nil {
		containers = map[int]*model.PortOwner{}
	}
	return &hostSnapshot{tcp: ports.Synthetic(listening, owners), containerOwners: containers}
}

func TestResolvePortsAttributesContainersToTheirOwnProject(t *testing.T) {
	m := newManager(t)
	p := model.Project{
		Runtime:        model.RuntimeCompose,
		ComposeProject: "shop",
		Ports:          []model.Port{{Port: 3000, Source: "compose"}, {Port: 3001, Source: "compose"}},
	}
	snap := snapshot(nil, nil, map[int]*model.PortOwner{
		3000: {ContainerID: "abc", Container: "shop-web-1", ComposeProject: "shop"},
		3001: {ContainerID: "def", Container: "other-web-1", ComposeProject: "other"},
	})

	got := m.resolvePorts(&p, &model.State{}, snap)
	if got[0].State != model.PortOwned {
		t.Errorf("own container should own its port, got %q", got[0].State)
	}
	// This is the distinction that makes the conflict message useful: the port
	// is busy *because another project has it*, not because of a stray process.
	if got[1].State != model.PortConflict {
		t.Errorf("another compose project's container is a conflict, got %q", got[1].State)
	}
	if got[1].Owner == nil || got[1].Owner.ComposeProject != "other" {
		t.Errorf("conflict should name the owning project, got %+v", got[1].Owner)
	}
}

func TestResolvePortsMarksForeignProcessesAsConflicts(t *testing.T) {
	m := newManager(t)
	p := model.Project{Runtime: model.RuntimeProcess, ID: "p1", Ports: []model.Port{{Port: 4000}}}
	snap := snapshot(nil, map[int]*model.PortOwner{
		4000: {PID: 999999, Process: "python3"},
	}, nil)

	got := m.resolvePorts(&p, &model.State{}, snap)
	// Nothing is supervised for p1, so pid 999999 cannot belong to it.
	if got[0].State != model.PortConflict {
		t.Errorf("got %q, want conflict", got[0].State)
	}
}

func TestResolvePortsFreeWhenNothingListens(t *testing.T) {
	m := newManager(t)
	p := model.Project{Runtime: model.RuntimeProcess, Ports: []model.Port{{Port: 4000}}}
	got := m.resolvePorts(&p, &model.State{}, snapshot(nil, nil, nil))
	if got[0].State != model.PortFree {
		t.Errorf("got %q, want free", got[0].State)
	}
}

func TestResolvePortsUnattributedListenerWhileRunning(t *testing.T) {
	m := newManager(t)
	p := model.Project{Runtime: model.RuntimeCompose, ComposeProject: "shop", Ports: []model.Port{{Port: 3000}}}
	// Listening, but with no owner we can see — Docker publishing through
	// iptables leaves no host-side process. If our containers are up, the port
	// is almost certainly ours.
	st := &model.State{Containers: []model.Container{{Service: "web", State: "running"}}}
	got := m.resolvePorts(&p, st, snapshot([]int{3000}, nil, nil))
	if got[0].State != model.PortOwned {
		t.Errorf("got %q, want owned", got[0].State)
	}

	// With nothing of ours running, we say so rather than guess.
	got = m.resolvePorts(&p, &model.State{}, snapshot([]int{3000}, nil, nil))
	if got[0].State != model.PortUnknownOwner {
		t.Errorf("got %q, want unknown-owner", got[0].State)
	}
}

func TestDeriveStatusCompose(t *testing.T) {
	m := newManager(t)
	p := model.Project{Runtime: model.RuntimeCompose}

	cases := []struct {
		name       string
		containers []model.Container
		want       model.Status
		detail     string
	}{
		{"never started", nil, model.StatusStopped, "no containers"},
		{"all up", []model.Container{
			{State: "running"}, {State: "running"},
		}, model.StatusRunning, "2 containers"},
		{"all down", []model.Container{
			{State: "exited"}, {State: "exited"},
		}, model.StatusStopped, "2 containers stopped"},
		{"partially up", []model.Container{
			{State: "running"}, {State: "exited"},
		}, model.StatusUnhealthy, "1 of 2 containers running"},
		{"container unhealthy", []model.Container{
			{State: "running", Health: "unhealthy", Service: "db"},
		}, model.StatusUnhealthy, "db unhealthy"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := model.State{Containers: tc.containers}
			m.deriveStatus(context.Background(), &p, &st, time.Time{})
			if st.Status != tc.want {
				t.Errorf("status = %q, want %q", st.Status, tc.want)
			}
			if tc.detail != "" && st.Detail != tc.detail {
				t.Errorf("detail = %q, want %q", st.Detail, tc.detail)
			}
		})
	}
}

func TestDeriveStatusBusyActionWins(t *testing.T) {
	m := newManager(t)
	p := model.Project{Runtime: model.RuntimeCompose}
	// The user just clicked Start; showing "stopped" until docker finishes
	// would look broken.
	st := model.State{Busy: "start", Containers: nil}
	m.deriveStatus(context.Background(), &p, &st, time.Now())
	if st.Status != model.StatusStarting {
		t.Errorf("status = %q, want starting", st.Status)
	}
}

func TestDeriveStatusConflictOnlyWhenNotRunning(t *testing.T) {
	m := newManager(t)
	p := model.Project{Runtime: model.RuntimeProcess}

	// Not running and the port is taken: starting would fail, so say why.
	st := model.State{Ports: []model.Port{{
		Port: 4000, State: model.PortConflict, Owner: &model.PortOwner{PID: 42, Process: "python3"},
	}}}
	m.deriveStatus(context.Background(), &p, &st, time.Time{})
	if st.Status != model.StatusConflict {
		t.Fatalf("status = %q, want conflict", st.Status)
	}
	if !strings.Contains(st.Detail, "python3") || !strings.Contains(st.Detail, "4000") {
		t.Errorf("detail should identify the owner, got %q", st.Detail)
	}
}

func TestDeriveStatusCrashOutranksConflict(t *testing.T) {
	m := newManager(t)
	p := model.Project{Runtime: model.RuntimeProcess}
	code := 1
	st := model.State{
		Process: &model.ProcessInfo{Running: false, ExitCode: &code},
		Ports: []model.Port{{
			Port: 4000, State: model.PortConflict, Owner: &model.PortOwner{PID: 42, Process: "ghost"},
		}},
	}
	m.deriveStatus(context.Background(), &p, &st, time.Time{})
	// The crash is the cause; the orphan holding the port is the symptom. Both
	// belong in the message, but the status should point at the crash.
	if st.Status != model.StatusError {
		t.Errorf("status = %q, want error", st.Status)
	}
	if !strings.Contains(st.Detail, "exited with code 1") || !strings.Contains(st.Detail, "4000") {
		t.Errorf("detail should mention both, got %q", st.Detail)
	}
}

func TestDeriveStatusRequestedStopIsNotAnError(t *testing.T) {
	m := newManager(t)
	p := model.Project{Runtime: model.RuntimeProcess}
	st := model.State{Process: &model.ProcessInfo{Running: false, StopRequested: true}}
	m.deriveStatus(context.Background(), &p, &st, time.Time{})
	if st.Status != model.StatusStopped {
		t.Errorf("status = %q, want stopped", st.Status)
	}
}

func TestHealthFailureIsStartingInsideTheSettleWindow(t *testing.T) {
	m := newManager(t)
	m.cfg.Poll.StartupSettleFor = time.Minute
	p := model.Project{
		Runtime: model.RuntimeProcess,
		// Port 1 is never listening, so the probe fails fast.
		Health: &model.HealthCheck{URL: "http://127.0.0.1:1/health"},
	}
	st := model.State{Process: &model.ProcessInfo{Running: true, PID: 1}}

	m.deriveStatus(context.Background(), &p, &st, time.Now())
	if st.Status != model.StatusStarting {
		t.Errorf("just started: status = %q, want starting", st.Status)
	}

	st = model.State{Process: &model.ProcessInfo{Running: true, PID: 1}}
	m.deriveStatus(context.Background(), &p, &st, time.Now().Add(-10*time.Minute))
	if st.Status != model.StatusUnhealthy {
		t.Errorf("after the settle window: status = %q, want unhealthy", st.Status)
	}
}

func TestApplyOverridesIsAPatchNotAReplacement(t *testing.T) {
	base := model.Project{
		Name:         "shop",
		Kind:         model.KindCompose,
		Runtime:      model.RuntimeCompose,
		Ports:        []model.Port{{Port: 3000, Source: "compose"}},
		URLs:         []string{"http://localhost:3000"},
		StartCommand: "",
	}
	// Setting only a health URL must not wipe the discovered ports or URLs.
	applyOverrides(&base, config.Project{Health: &config.Health{URL: "http://localhost:3000/health"}})
	if len(base.Ports) != 1 || base.Ports[0].Port != 3000 {
		t.Errorf("ports were lost: %+v", base.Ports)
	}
	if base.Health == nil || base.Health.URL == "" {
		t.Error("health check was not applied")
	}

	// An explicit start command switches the project to supervision.
	applyOverrides(&base, config.Project{Start: "make dev"})
	if base.Runtime != model.RuntimeProcess || base.StartCommand != "make dev" {
		t.Errorf("start override: runtime=%q start=%q", base.Runtime, base.StartCommand)
	}

	// Explicit ports replace detected ones outright.
	applyOverrides(&base, config.Project{Ports: []int{8080}})
	if len(base.Ports) != 1 || base.Ports[0].Port != 8080 || base.Ports[0].Source != "config" {
		t.Errorf("port override: %+v", base.Ports)
	}
}

func TestNormalizeReplacesNilSlices(t *testing.T) {
	// JSON null and [] are the same to Go and different to JavaScript; the UI
	// iterates these lists, so they must always be arrays.
	v := normalize(model.View{})
	if v.Ports == nil || v.URLs == nil || v.State.Ports == nil || v.State.Containers == nil {
		t.Errorf("nil slice survived normalize: %+v", v)
	}
}

func TestAllowedURLRejectsForeignTargets(t *testing.T) {
	v := model.View{
		Project: model.Project{URLs: []string{"http://localhost:3000"}},
		State:   model.State{Ports: []model.Port{{Port: 3000}}},
	}
	if !allowedURL(v, "http://localhost:3000") {
		t.Error("a declared URL should be allowed")
	}
	if !allowedURL(v, "http://127.0.0.1:3000/admin") {
		t.Error("a path on a known port should be allowed")
	}
	// Otherwise POST /open becomes a general-purpose "open anything" endpoint.
	if allowedURL(v, "https://evil.example") {
		t.Error("a foreign URL must be rejected")
	}
	if allowedURL(v, "http://localhost:9999") {
		t.Error("an unrelated port must be rejected")
	}
}
