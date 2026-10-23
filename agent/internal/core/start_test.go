package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/devcc/agent/internal/config"
	"github.com/devcc/agent/internal/model"
	"github.com/devcc/agent/internal/ports"
)

func TestPortFlagIn(t *testing.T) {
	cases := map[string]int{
		"vite --port 5173":           5173,
		"next dev -p 4200":           4200,
		"node server.js --port=8080": 8080,
		"npm run dev":                0,
		"go run .":                   0,
	}
	for command, want := range cases {
		got, ok := portFlagIn(command)
		if want == 0 && ok {
			t.Errorf("%q: expected no port, got %d", command, got)
		}
		if want != 0 && got != want {
			t.Errorf("%q: got %d, want %d", command, got, want)
		}
	}
}

func TestProcessStartSpecRewritesFlagAndSetsPORT(t *testing.T) {
	m := newManager(t)
	p := model.Project{
		ID:           "p1",
		Path:         t.TempDir(),
		StartCommand: "node server.js --port 4001",
		Ports:        []model.Port{{Port: 4003, Declared: 4001}},
	}
	m.mu.Lock()
	m.portOverrides["p1"] = map[int]int{4001: 4003}
	m.mu.Unlock()

	spec := m.processStartSpec(p)
	if spec.Command != "node server.js --port 4003" {
		t.Errorf("command = %q, want the flag rewritten", spec.Command)
	}
	// Both levers, because neither alone is reliable across dev servers.
	if spec.Env["PORT"] != "4003" {
		t.Errorf("PORT = %q, want 4003", spec.Env["PORT"])
	}
}

func TestProcessStartSpecWithoutOverridesIsUntouched(t *testing.T) {
	m := newManager(t)
	p := model.Project{ID: "p1", Path: t.TempDir(), StartCommand: "npm run dev"}
	spec := m.processStartSpec(p)
	if spec.Command != "npm run dev" || spec.Env["PORT"] != "" {
		t.Errorf("unexpected spec: %+v", spec)
	}
}

func TestDerivePortControl(t *testing.T) {
	m := newManager(t)

	// A compose file that declares its port through a variable is movable by
	// setting that variable, which is the mechanism the author opted into.
	withVar := &model.Project{
		Runtime: model.RuntimeCompose,
		Ports:   []model.Port{{Port: 3000, Variable: "WEB_PORT"}},
	}
	if pc := derivePortControl(withVar, m.docker); pc.Mechanism != model.PortChangeComposeVar || !pc.CanChange {
		t.Errorf("variable-declared port: %+v", pc)
	}

	// An explicit --port flag is the most reliable lever for a process.
	withFlag := &model.Project{Runtime: model.RuntimeProcess, StartCommand: "vite --port 5173"}
	if pc := derivePortControl(withFlag, m.docker); pc.Mechanism != model.PortChangeCommand {
		t.Errorf("flagged command: %+v", pc)
	}

	// Otherwise we fall back to PORT, which is honest but not guaranteed.
	noFlag := &model.Project{Runtime: model.RuntimeProcess, StartCommand: "npm run dev"}
	if pc := derivePortControl(noFlag, m.docker); pc.Mechanism != model.PortChangeEnv {
		t.Errorf("unflagged command: %+v", pc)
	}

	// Nothing to move when there is no way to start it.
	none := &model.Project{Runtime: model.RuntimeNone}
	if pc := derivePortControl(none, m.docker); pc.CanChange {
		t.Errorf("uncontrollable project should not offer a port change: %+v", pc)
	}
}

func TestApplyOverridesToPortsKeepsTheDeclaredValue(t *testing.T) {
	p := &model.Project{
		Ports: []model.Port{{Port: 3000, Source: "compose"}, {Port: 5432, Source: "compose"}},
		URLs:  []string{"http://localhost:3000"},
	}
	applyOverridesToPorts(p, map[int]int{3000: 3100})

	if p.Ports[0].Port != 3100 || p.Ports[0].Declared != 3000 {
		t.Errorf("moved port = %+v, want 3100 declared 3000", p.Ports[0])
	}
	// An untouched port keeps no Declared value, so the UI does not claim it moved.
	for _, port := range p.Ports {
		if port.Port == 5432 && port.Declared != 0 {
			t.Errorf("untouched port should have no declared value: %+v", port)
		}
	}
	// A URL pointing at the old port would now go nowhere.
	if p.URLs[0] != "http://localhost:3100" {
		t.Errorf("URL = %q, want the moved port", p.URLs[0])
	}
}

func TestSuggestPortAvoidsOtherProjectsPorts(t *testing.T) {
	snap := ports.Synthetic([]int{3001}, nil)
	// 3001 is listening and 3002 belongs to another project: handing out either
	// would just move the conflict rather than resolve it.
	claimed := map[int]bool{3002: true}
	got := suggestPort(3000, snap, claimed)
	if got == 3001 || got == 3002 || got <= 3000 {
		t.Errorf("suggestPort = %d, want a free unclaimed port above 3000", got)
	}
	if !ports.IsFree(got) {
		t.Errorf("suggested port %d is not actually free", got)
	}
}

func TestPortOverridesComeFromTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "devcc.yaml")
	project := filepath.Join(dir, "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "projects:\n  - name: App\n    path: " + project + "\n    portOverrides:\n      3000: 3100\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.Discover()

	// The override survives a restart because it lives in the config file the
	// user can also edit by hand.
	if got := m.portOverridesFor(model.ID(project)); got[3000] != 3100 {
		t.Errorf("overrides = %v, want 3000->3100", got)
	}
}

func TestUnhonouredPortIsReported(t *testing.T) {
	// The user asked for 4003; the process ignored it and opened 4001.
	missed, actual := unhonouredPort([]model.Port{
		{Port: 4003, Declared: 4001, State: model.PortFree},
		{Port: 4001, Source: "observed", State: model.PortOwned},
	})
	if missed != 4003 || actual != 4001 {
		t.Errorf("got missed=%d actual=%d, want 4003/4001", missed, actual)
	}

	// When the move took effect there is nothing to report.
	missed, _ = unhonouredPort([]model.Port{{Port: 4003, Declared: 4001, State: model.PortOwned}})
	if missed != 0 {
		t.Errorf("honoured move should report nothing, got %d", missed)
	}
}

func TestOwnerLabel(t *testing.T) {
	cases := []struct {
		owner *model.PortOwner
		want  string
	}{
		{nil, "an unidentified process"},
		{&model.PortOwner{Container: "shop-web-1", ComposeProject: "shop"}, `container shop-web-1 (compose project "shop")`},
		{&model.PortOwner{Container: "redis"}, "container redis"},
		{&model.PortOwner{Process: "node", PID: 42}, "node (pid 42)"},
		{&model.PortOwner{PID: 42}, "pid 42"},
	}
	for _, c := range cases {
		if got := ownerLabel(c.owner); got != c.want {
			t.Errorf("ownerLabel = %q, want %q", got, c.want)
		}
	}
}

// Stopping something is never inferred: without StopOwners the conflict is
// returned to the caller untouched.
func TestConflictIsReturnedRatherThanResolved(t *testing.T) {
	m := newManager(t)
	p := model.Project{
		ID:      "p1",
		Name:    "svc",
		Runtime: model.RuntimeProcess,
		Ports:   []model.Port{{Port: 4999}},
	}
	m.mu.Lock()
	m.byID["p1"] = &entry{project: p}
	m.order = []string{"p1"}
	m.mu.Unlock()

	snapOwner := map[int]*model.PortOwner{4999: {PID: 999999, Process: "someone-else"}}
	m.testSnapshot = &hostSnapshot{
		tcp:             ports.Synthetic(nil, snapOwner),
		containerOwners: map[int]*model.PortOwner{},
	}

	conflict := m.checkConflicts(t.Context(), p, ActionOptions{})
	if conflict == nil {
		t.Fatal("expected a conflict")
	}
	if len(conflict.Ports) != 1 || conflict.Ports[0].Port != 4999 {
		t.Fatalf("unexpected conflict: %+v", conflict.Ports)
	}
	if !conflict.Ports[0].CanStopOwner {
		t.Error("an identified PID should be stoppable")
	}
	if conflict.Ports[0].Suggested == 0 {
		t.Error("expected a suggested alternative port")
	}
}

// The bug this guards: passing -f turns off Compose's automatic loading of the
// override sibling, so a project whose build context lives there fails with
// "failed to read dockerfile". Every compose invocation must carry the chain,
// with or without a port override, and for reads as well as writes.
func TestComposeOptionsAlwaysCarryTheProjectsOwnFiles(t *testing.T) {
	m := newManager(t)
	p := model.Project{
		ID:                "p1",
		Runtime:           model.RuntimeCompose,
		ComposeFile:       "/proj/docker-compose.yml",
		ComposeExtraFiles: []string{"/proj/docker-compose.override.yml"},
		Ports:             []model.Port{{Port: 8080, Service: "backend", ContainerPort: 8080}},
	}

	// No port override at all.
	opts, err := m.composeOptions(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.ExtraFiles) != 1 || opts.ExtraFiles[0] != "/proj/docker-compose.override.yml" {
		t.Errorf("without an override: ExtraFiles = %v", opts.ExtraFiles)
	}

	// Read-only commands (ps, logs) must see the same project definition.
	if got := m.composeReadOptions(p); len(got.ExtraFiles) != 1 {
		t.Errorf("read options dropped the chain: %v", got.ExtraFiles)
	}

	// With a generated port override, the project's own file still comes first
	// so the agent's !override wins.
	m.mu.Lock()
	m.portOverrides["p1"] = map[int]int{8080: 8082}
	m.mu.Unlock()
	opts, err = m.composeOptions(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.ExtraFiles) != 2 {
		t.Fatalf("ExtraFiles = %v, want the sibling then the generated file", opts.ExtraFiles)
	}
	if opts.ExtraFiles[0] != "/proj/docker-compose.override.yml" {
		t.Errorf("the project's own override must come first, got %v", opts.ExtraFiles)
	}
}

func TestExplicitComposeFileDoesNotPullInASibling(t *testing.T) {
	// Naming a file in the config is the equivalent of `docker compose -f x`,
	// which does not load an override sibling either.
	p := model.Project{Runtime: model.RuntimeCompose, ComposeExtraFiles: []string{"/proj/override.yml"}}
	applyOverrides(&p, config.Project{ComposeFile: "/proj/other.yml"})
	if len(p.ComposeExtraFiles) != 0 {
		t.Errorf("ComposeExtraFiles = %v, want none", p.ComposeExtraFiles)
	}
}
