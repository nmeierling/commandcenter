package core

import (
	"testing"

	"github.com/devcc/agent/internal/dockercli"
	"github.com/devcc/agent/internal/model"
	"github.com/devcc/agent/internal/ports"
)

// seed installs projects directly, so substitution logic can be tested without
// a docker daemon or a filesystem full of fixtures.
func seed(t *testing.T, m *Manager, views ...model.View) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID = map[string]*entry{}
	m.order = nil
	for _, v := range views {
		m.byID[v.ID] = &entry{project: v.Project, state: v.State}
		m.order = append(m.order, v.ID)
	}
}

func runningDB(projectID, name string, port int) model.View {
	return model.View{
		Project: model.Project{ID: projectID, Name: name, Runtime: model.RuntimeCompose,
			Services: []string{"db", "web"}},
		State: model.State{
			Status: model.StatusRunning,
			Units: []model.Unit{
				{ID: "db", Name: "db", Kind: "container", Running: true, State: model.UnitRunning,
					Image: "postgres:16",
					Ports: []model.UnitPort{{Configured: port, Actual: port, Container: 5432, State: model.PortOwned}}},
				{ID: "web", Name: "web", Kind: "container", Running: true, State: model.UnitRunning,
					Ports: []model.UnitPort{{Configured: 3000, Actual: 3000, Container: 80, State: model.PortOwned}}},
			},
		},
	}
}

func blockedBlog() model.View {
	return model.View{
		Project: model.Project{ID: "blog", Name: "blog", Runtime: model.RuntimeCompose,
			Services: []string{"db", "web"}},
		State: model.State{
			Units: []model.Unit{
				{ID: "db", Name: "db", Kind: "container",
					Ports: []model.UnitPort{{Configured: 5432, Container: 5432, State: model.PortConflict}}},
				{ID: "web", Name: "web", Kind: "container",
					Ports: []model.UnitPort{{Configured: 3100, Container: 80, State: model.PortFree}}},
			},
		},
	}
}

func TestCandidatesRankThePortMatchFirst(t *testing.T) {
	m := newManager(t)
	seed(t, m, runningDB("shop", "shop", 5432), blockedBlog())

	got := m.SubstituteCandidates("blog", "db")
	if len(got) < 2 {
		t.Fatalf("expected at least two candidates, got %+v", got)
	}
	// Whatever already serves the blocked port is almost always the answer.
	if got[0].Target != "shop/db" || !got[0].Matches {
		t.Errorf("first candidate = %+v, want shop/db with matches=true", got[0])
	}
	for _, c := range got {
		if c.ProjectID == "blog" {
			t.Error("a project cannot substitute for itself")
		}
	}
}

func TestCandidatesSkipUnitsWithNoPublishedPort(t *testing.T) {
	m := newManager(t)
	shop := runningDB("shop", "shop", 5432)
	shop.State.Units = append(shop.State.Units, model.Unit{
		ID: "worker", Name: "worker", Kind: "container", Running: true, State: model.UnitRunning,
	})
	seed(t, m, shop, blockedBlog())

	for _, c := range m.SubstituteCandidates("blog", "db") {
		if c.UnitID == "worker" {
			t.Error("a unit publishing nothing cannot stand in for a dependency")
		}
	}
}

func TestResolveSubstitutionReportsWhyItIsUnsatisfied(t *testing.T) {
	m := newManager(t)
	seed(t, m, runningDB("shop", "shop", 5432), blockedBlog())

	if got := m.resolveSubstitution("shop/db"); !got.Satisfied || got.ProjectName != "shop" {
		t.Errorf("valid target: %+v", got)
	}
	// Each failure mode says something the user can act on.
	for target, wantPart := range map[string]string{
		"nope/db":  "no project",
		"shop/xyz": "no unit called",
		"garbage":  "not a valid target",
	} {
		got := m.resolveSubstitution(target)
		if got.Satisfied {
			t.Errorf("%q should not be satisfied", target)
		}
		if got.Detail == "" {
			t.Errorf("%q: expected a reason mentioning %q", target, wantPart)
		}
	}

	// A target that exists but is stopped is not a substitute yet.
	stopped := runningDB("api", "api", 6000)
	stopped.State.Units[0].Running = false
	seed(t, m, stopped, blockedBlog())
	if got := m.resolveSubstitution("api/db"); got.Satisfied || got.ProjectID == "" {
		t.Errorf("stopped provider: %+v", got)
	}
}

func TestSubstitutedUnitIsNotStartedByStartAll(t *testing.T) {
	m := newManager(t)
	m.mu.Lock()
	m.subs = map[string]map[string]string{"blog": {"db": "shop/db"}}
	m.mu.Unlock()

	p := model.Project{ID: "blog", Runtime: model.RuntimeCompose, Services: []string{"db", "web"}}
	got := m.startableServices(p)
	if len(got) != 1 || got[0] != "web" {
		t.Errorf("startableServices = %v, want [web] — a project-wide start must not undo the choice", got)
	}

	// With nothing substituted, compose is left to start everything itself.
	m.mu.Lock()
	m.subs = map[string]map[string]string{}
	m.mu.Unlock()
	if got := m.startableServices(p); got != nil {
		t.Errorf("expected no restriction, got %v", got)
	}
}

func TestSatisfiedSubstitutionClearsTheConflict(t *testing.T) {
	st := model.State{
		Ports: []model.Port{
			{Port: 5432, State: model.PortConflict},
			{Port: 3100, State: model.PortFree},
		},
		Units: []model.Unit{{
			ID:         "db",
			Substitute: &model.Substitution{Target: "shop/db", Satisfied: true},
			Ports:      []model.UnitPort{{Configured: 5432, State: model.PortConflict}},
		}},
	}
	applySubstitutedPorts(&st)
	if st.Ports[0].State != model.PortSubstituted {
		t.Errorf("port 5432 = %q, want substituted", st.Ports[0].State)
	}
	if st.Ports[1].State != model.PortFree {
		t.Errorf("unrelated port changed: %q", st.Ports[1].State)
	}
}

func TestUnsatisfiedSubstitutionLeavesTheConflictAlone(t *testing.T) {
	// If the provider is not actually running, the port really is a problem.
	st := model.State{
		Ports: []model.Port{{Port: 5432, State: model.PortConflict}},
		Units: []model.Unit{{
			ID:         "db",
			Substitute: &model.Substitution{Target: "shop/db", Satisfied: false, Detail: "not running"},
			Ports:      []model.UnitPort{{Configured: 5432, State: model.PortConflict}},
		}},
	}
	applySubstitutedPorts(&st)
	if st.Ports[0].State != model.PortConflict {
		t.Errorf("port = %q, want conflict", st.Ports[0].State)
	}
}

func TestDecorateUnitsMarksTheRow(t *testing.T) {
	m := newManager(t)
	seed(t, m, runningDB("shop", "shop", 5432), blockedBlog())
	m.mu.Lock()
	m.subs = map[string]map[string]string{"blog": {"db": "shop/db"}}
	m.mu.Unlock()

	units := m.decorateUnits(blockedBlog().State.Units, "blog")
	db := findUnit(units, "db")
	if db.State != model.UnitSubstituted || db.Substitute == nil || !db.Substitute.Satisfied {
		t.Fatalf("db = %+v", db)
	}
	if db.Substitute.ProjectName != "shop" {
		t.Errorf("substitute should name the provider, got %+v", db.Substitute)
	}
	if db.Ports[0].State != model.PortSubstituted {
		t.Errorf("port state = %q, want substituted", db.Ports[0].State)
	}
	// An unrelated unit is untouched.
	if web := findUnit(units, "web"); web.Substitute != nil {
		t.Errorf("web = %+v", web)
	}
}

func withContainers(m *Manager, cs ...dockercli.RunningContainer) {
	m.setContainers(cs)
}

func rawContainer(name, image string, host, target int, project, service string) dockercli.RunningContainer {
	return dockercli.RunningContainer{
		ID: name + "id", Name: name, Image: image,
		Ports:          []model.ContainerPort{{Host: host, Container: target, Protocol: "tcp"}},
		ComposeProject: project, ComposeService: service,
	}
}

// A postgres someone started by hand is invisible to project discovery, but it
// is exactly the thing a shared dependency should be pointed at.
func TestCandidatesIncludeLooseDockerContainers(t *testing.T) {
	m := newManager(t)
	seed(t, m, blockedBlog())
	withContainers(m, rawContainer("local-pg", "postgres:16", 5432, 5432, "", ""))

	got := m.SubstituteCandidates("blog", "db")
	if len(got) != 1 {
		t.Fatalf("got %+v, want the loose container", got)
	}
	if got[0].Target != "docker/local-pg" || got[0].Source != "docker" {
		t.Errorf("candidate = %+v", got[0])
	}
	if !got[0].Matches {
		t.Error("a container serving the blocked port should be marked as matching")
	}
}

func TestDockerCandidatesDoNotDuplicateProjectUnits(t *testing.T) {
	m := newManager(t)
	shop := runningDB("shop", "shop", 5432)
	shop.Project.ComposeProject = "shop"
	seed(t, m, shop, blockedBlog())
	// The very same container docker reports is already offered as shop/db.
	withContainers(m, rawContainer("shop-db-1", "postgres:16", 5432, 5432, "shop", "db"))

	got := m.SubstituteCandidates("blog", "db")
	targets := map[string]int{}
	for _, c := range got {
		targets[c.Target]++
	}
	if targets["docker/shop-db-1"] != 0 {
		t.Errorf("the same database was offered twice: %+v", got)
	}
	if targets["shop/db"] != 1 {
		t.Errorf("expected the project unit to be offered once: %+v", got)
	}
}

func TestProjectCandidatesRankAboveLooseContainers(t *testing.T) {
	m := newManager(t)
	seed(t, m, runningDB("shop", "shop", 5432), blockedBlog())
	withContainers(m, rawContainer("stray-pg", "postgres:16", 5432, 5432, "", ""))

	got := m.SubstituteCandidates("blog", "db")
	if len(got) < 2 {
		t.Fatalf("expected both sources, got %+v", got)
	}
	// Both serve the blocked port; the managed one is the more meaningful pick.
	if got[0].Source != "project" {
		t.Errorf("first candidate = %+v, want the project unit", got[0])
	}
}

func TestOwnContainersAreNotOfferedAsSubstitutes(t *testing.T) {
	m := newManager(t)
	blog := blockedBlog()
	blog.Project.ComposeProject = "blog"
	seed(t, m, blog)
	// blog's own db container must never be offered as a substitute for itself.
	withContainers(m, rawContainer("blog-db-1", "postgres:16", 5432, 5432, "blog", "db"))

	if got := m.SubstituteCandidates("blog", "db"); len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

func TestResolveDockerSubstitution(t *testing.T) {
	m := newManager(t)
	withContainers(m, rawContainer("local-pg", "postgres:16", 5432, 5432, "", ""))

	got := m.resolveSubstitution("docker/local-pg")
	if !got.Satisfied || len(got.Ports) != 1 || got.Ports[0] != 5432 {
		t.Errorf("got %+v", got)
	}

	// A container that has gone away says so rather than silently succeeding.
	gone := m.resolveSubstitution("docker/vanished")
	if gone.Satisfied || gone.Detail == "" {
		t.Errorf("got %+v, want an unsatisfied result with a reason", gone)
	}
}

func TestDockerPrefixIsNotShadowedByAProjectNamedDocker(t *testing.T) {
	m := newManager(t)
	// A project actually called "docker" must not capture docker/<container>.
	confusing := runningDB("d1", "docker", 6000)
	seed(t, m, confusing)
	withContainers(m, rawContainer("local-pg", "postgres:16", 5432, 5432, "", ""))

	if got := m.resolveSubstitution("docker/local-pg"); !got.Satisfied {
		t.Errorf("docker/ prefix should win: %+v", got)
	}
}

func TestContainersWithNoPublishedPortAreNotOffered(t *testing.T) {
	m := newManager(t)
	seed(t, m, blockedBlog())
	withContainers(m, dockercli.RunningContainer{ID: "x", Name: "sidecar", Image: "alpine"})

	if got := m.SubstituteCandidates("blog", "db"); len(got) != 0 {
		t.Errorf("got %+v, want nothing reachable", got)
	}
}

func TestStoppedProviderIsStillAValidChoice(t *testing.T) {
	m := newManager(t)
	stopped := runningDB("shop", "shop", 5432)
	stopped.State.Units[0].Running = false
	seed(t, m, stopped, blockedBlog())

	got := m.resolveSubstitution("shop/db")
	// It exists, so recording it is allowed; it just is not satisfied yet.
	if !got.Known {
		t.Error("a stopped but existing provider should be a known target")
	}
	if got.Satisfied {
		t.Error("a stopped provider cannot satisfy anything")
	}

	// Nothing at all, on the other hand, is not recordable.
	if m.resolveSubstitution("nope/db").Known {
		t.Error("an unknown project should not be a known target")
	}
	if m.resolveSubstitution("shop/nosuchunit").Known {
		t.Error("an unknown unit should not be a known target")
	}
}

func TestUnitOptionsWorkWithoutAConflict(t *testing.T) {
	m := newManager(t)
	shop := runningDB("shop", "shop", 5432)
	seed(t, m, shop)
	m.testSnapshot = &hostSnapshot{
		tcp:             ports.Synthetic(nil, nil),
		containerOwners: map[int]*model.PortOwner{},
	}

	// Opened deliberately on a healthy unit: the same payload, but not blocked,
	// so the dialog can ask "how should this run?" instead of announcing a
	// conflict that does not exist.
	got, err := m.UnitOptions(t.Context(), "shop", "db")
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocked {
		t.Error("a unit with a free port should not be reported as blocked")
	}
	if len(got.Ports) != 1 || got.Ports[0].Port != 5432 {
		t.Errorf("ports = %+v", got.Ports)
	}
	if got.Ports[0].Suggested == 0 {
		t.Error("expected an alternative port to be suggested")
	}
	if got.UnitName != "db" {
		t.Errorf("unit = %q", got.UnitName)
	}
}

func TestUnitOptionsDoNotCallASatisfiedSubstitutionAConflict(t *testing.T) {
	m := newManager(t)
	blog := blockedBlog()
	// The db row is substituted and happy; the raw port resolution would still
	// say "conflict", and the dialog must not open with that question.
	blog.State.Units[0].Substitute = &model.Substitution{
		Target: "shop/db", Known: true, Satisfied: true,
	}
	blog.State.Units[0].State = model.UnitSubstituted
	blog.State.Units[0].Ports[0].State = model.PortSubstituted
	seed(t, m, runningDB("shop", "shop", 5432), blog)
	m.testSnapshot = &hostSnapshot{
		tcp: ports.Synthetic(nil, map[int]*model.PortOwner{
			5432: {ContainerID: "c2", Container: "shop-db-1", ComposeProject: "shop"},
		}),
		containerOwners: map[int]*model.PortOwner{
			5432: {ContainerID: "c2", Container: "shop-db-1", ComposeProject: "shop"},
		},
	}

	got, err := m.UnitOptions(t.Context(), "blog", "db")
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocked {
		t.Error("a satisfied substitution is the desired state, not a conflict")
	}
}

func TestUnitOptionsRejectAUnitWithNoConfiguredPort(t *testing.T) {
	m := newManager(t)
	shop := runningDB("shop", "shop", 5432)
	shop.State.Units = append(shop.State.Units, model.Unit{
		ID: "worker", Name: "worker", Kind: "container", Running: true,
	})
	seed(t, m, shop)

	// There is nothing to move and nothing to point elsewhere.
	if _, err := m.UnitOptions(t.Context(), "shop", "worker"); err == nil {
		t.Error("expected an error for a unit with no configured port")
	}
}
