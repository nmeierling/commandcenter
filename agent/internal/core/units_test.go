package core

import (
	"testing"

	"github.com/devcc/agent/internal/model"
)

func unitByName(units []model.Unit, name string) *model.Unit {
	for i := range units {
		if units[i].Name == name {
			return &units[i]
		}
	}
	return nil
}

// The point of listing declared services: a compose project that has never been
// started still shows what it is made of, rather than an empty card.
func TestComposeUnitsListDeclaredServicesBeforeAnythingRuns(t *testing.T) {
	p := model.Project{
		Runtime:  model.RuntimeCompose,
		Services: []string{"web", "db", "worker"},
		Ports: []model.Port{
			{Port: 3000, Service: "web", ContainerPort: 80},
			{Port: 5432, Service: "db", ContainerPort: 5432},
		},
	}
	st := model.State{Status: model.StatusStopped}
	units := buildUnits(&p, &st, p.Ports)

	if len(units) != 3 {
		t.Fatalf("got %d units, want 3: %+v", len(units), units)
	}
	for _, u := range units {
		if u.State != model.UnitNotCreated || u.Running {
			t.Errorf("%s: state = %q running = %v, want not-created", u.Name, u.State, u.Running)
		}
	}
	web := unitByName(units, "web")
	if len(web.Ports) != 1 || web.Ports[0].Configured != 3000 || web.Ports[0].Actual != 0 {
		t.Errorf("web ports = %+v, want configured 3000 with no actual", web.Ports)
	}
	// A service with no published ports is still a row.
	if w := unitByName(units, "worker"); w == nil || len(w.Ports) != 0 {
		t.Errorf("worker should be listed with no ports, got %+v", w)
	}
}

func TestComposeUnitsMergeRunningContainers(t *testing.T) {
	p := model.Project{
		Runtime:  model.RuntimeCompose,
		Services: []string{"web", "worker"},
		Ports:    []model.Port{{Port: 3000, Service: "web", ContainerPort: 80, State: model.PortOwned}},
	}
	st := model.State{
		Status: model.StatusRunning,
		Containers: []model.Container{
			{Name: "shop-web-1", Service: "web", State: "running", Status: "Up 2 minutes",
				Ports: []model.ContainerPort{{Host: 3000, Container: 80, Protocol: "tcp"}}},
			{Name: "shop-worker-1", Service: "worker", State: "exited", Status: "Exited (0)"},
		},
	}
	units := buildUnits(&p, &st, p.Ports)

	web := unitByName(units, "web")
	if web == nil || !web.Running || web.ContainerName != "shop-web-1" {
		t.Fatalf("web = %+v", web)
	}
	if web.Ports[0].Configured != 3000 || web.Ports[0].Actual != 3000 || web.Ports[0].Container != 80 {
		t.Errorf("web port = %+v, want 3000/3000 -> 80", web.Ports[0])
	}
	if web.URL != "http://localhost:3000" {
		t.Errorf("web URL = %q", web.URL)
	}

	// A stopped container is still listed, and offers no URL to open.
	worker := unitByName(units, "worker")
	if worker == nil || worker.Running || worker.State != model.UnitExited {
		t.Fatalf("worker = %+v", worker)
	}
	if worker.URL != "" {
		t.Errorf("a stopped unit should have no URL, got %q", worker.URL)
	}
}

func TestUnitPortShowsConfiguredAndActualSeparatelyAfterAMove(t *testing.T) {
	p := model.Project{
		Runtime:  model.RuntimeCompose,
		Services: []string{"web"},
		// The user moved this project: it now runs on 3100, declared as 3000.
		Ports: []model.Port{
			{Port: 3100, Declared: 3000, Service: "web", ContainerPort: 80, State: model.PortOwned},
		},
	}
	st := model.State{
		Status: model.StatusRunning,
		Containers: []model.Container{{
			Name: "shop-web-1", Service: "web", State: "running",
			Ports: []model.ContainerPort{{Host: 3100, Container: 80}},
		}},
	}
	units := buildUnits(&p, &st, p.Ports)

	port := unitByName(units, "web").Ports[0]
	// Configured is what devcc will use, Declared what the file says, Actual
	// what is listening. All three must survive or the row cannot say
	// "running on 3100, declared as 3000".
	if port.Configured != 3100 {
		t.Errorf("configured = %d, want the effective 3100", port.Configured)
	}
	if port.Declared != 3000 {
		t.Errorf("declared = %d, want 3000", port.Declared)
	}
	if port.Actual != 3100 {
		t.Errorf("actual = %d, want 3100", port.Actual)
	}
}

// The case that was actually broken: a port moved to 8081 but not yet started
// still showed 8080, because the new number was only ever surfaced through
// Actual and nothing was listening yet.
func TestMovedPortIsVisibleBeforeAnythingStarts(t *testing.T) {
	p := model.Project{
		Runtime:  model.RuntimeCompose,
		Services: []string{"api"},
		Ports: []model.Port{
			{Port: 8081, Declared: 8080, Service: "api", ContainerPort: 8080, State: model.PortFree},
		},
	}
	st := model.State{Status: model.StatusStopped}
	units := buildUnits(&p, &st, p.Ports)

	port := unitByName(units, "api").Ports[0]
	if port.Configured != 8081 {
		t.Errorf("configured = %d, want 8081 — the port devcc will actually use", port.Configured)
	}
	if port.Declared != 8080 {
		t.Errorf("declared = %d, want 8080", port.Declared)
	}
	if port.Actual != 0 {
		t.Errorf("actual = %d, want 0 — nothing is listening yet", port.Actual)
	}
}

func TestComposeUnitsIncludeContainersNotInTheFile(t *testing.T) {
	// A service removed from the compose file but still running is exactly the
	// sort of thing worth seeing rather than hiding.
	p := model.Project{Runtime: model.RuntimeCompose, Services: []string{"web"}}
	st := model.State{
		Status: model.StatusRunning,
		Containers: []model.Container{
			{Name: "shop-web-1", Service: "web", State: "running"},
			{Name: "shop-legacy-1", Service: "legacy", State: "running"},
		},
	}
	units := buildUnits(&p, &st, nil)
	if unitByName(units, "legacy") == nil {
		t.Errorf("an orphaned container should still be listed, got %+v", units)
	}
}

func TestProcessProjectHasExactlyOneUnit(t *testing.T) {
	p := model.Project{
		Runtime:      model.RuntimeProcess,
		Name:         "api",
		StartCommand: "npm run dev",
		Ports:        []model.Port{{Port: 4000, State: model.PortOwned}},
	}
	pid := 4242
	st := model.State{
		Status:  model.StatusRunning,
		Process: &model.ProcessInfo{PID: pid, Running: true},
	}
	units := buildUnits(&p, &st, p.Ports)
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	u := units[0]
	if u.ID != AppUnitID || !u.Running || u.PID != pid {
		t.Errorf("unit = %+v", u)
	}
	if u.Ports[0].Configured != 4000 || u.Ports[0].Actual != 4000 {
		t.Errorf("ports = %+v", u.Ports)
	}
}

func TestUncontrollableProjectUnitExplainsWhy(t *testing.T) {
	p := model.Project{Runtime: model.RuntimeNone, Name: "notes"}
	st := model.State{Status: model.StatusStopped}
	units := buildUnits(&p, &st, nil)
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	// The row still exists — it just cannot be started, and says so.
	if units[0].Blocked == "" {
		t.Error("an uncontrollable unit should carry a reason")
	}
}

func TestUnitURLSkipsConflictedPorts(t *testing.T) {
	// Opening a port another project is serving would show their app, not ours.
	ports := []model.UnitPort{
		{Configured: 3000, Actual: 3000, State: model.PortConflict},
		{Configured: 3001, Actual: 3001, State: model.PortOwned},
	}
	if got := firstOpenableURL(ports, true); got != "http://localhost:3001" {
		t.Errorf("URL = %q, want the owned port", got)
	}
	if got := firstOpenableURL(ports, false); got != "" {
		t.Errorf("a stopped unit should have no URL, got %q", got)
	}
}
