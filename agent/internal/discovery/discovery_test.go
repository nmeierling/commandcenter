package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/devcc/agent/internal/config"
	"github.com/devcc/agent/internal/model"
)

func mkdirp(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanDetectsKindsAndStopsAtProjects(t *testing.T) {
	root := t.TempDir()

	shop := mkdirp(t, root, "shop")
	write(t, shop, "compose.yaml", "services:\n  web:\n    ports: [\"3000:80\"]\n")
	// A package.json inside a compose project must not become a second card.
	write(t, shop, "package.json", `{"name":"shop-web","scripts":{"dev":"vite"}}`)

	svc := mkdirp(t, root, "svc")
	write(t, svc, "go.mod", "module svc\n")
	write(t, svc, "main.go", "package main\n")

	// Nested project directories below a detected project are not scanned.
	nested := mkdirp(t, root, "svc", "internal", "tool")
	write(t, nested, "package.json", `{"name":"tool","scripts":{"dev":"x"}}`)

	s := NewScanner(config.Scan{Roots: []string{root}, Depth: 3})
	found := s.Scan()

	byName := map[string]model.Project{}
	for _, p := range found {
		byName[p.Name] = p
	}
	if len(found) != 2 {
		t.Fatalf("expected 2 projects, got %d: %v", len(found), byName)
	}
	if p := byName["shop"]; p.Kind != model.KindCompose || p.Runtime != model.RuntimeCompose {
		t.Errorf("shop = %+v, want a compose project", p)
	} else if len(p.Ports) != 1 || p.Ports[0].Port != 3000 {
		t.Errorf("shop ports = %v, want [3000]", p.Ports)
	}
	if p := byName["svc"]; p.Kind != model.KindGo || p.StartCommand != "go run ." {
		t.Errorf("svc = %+v, want a go project started with `go run .`", p)
	}
}

// depth is counted from the root itself: depth 0 is the root directory, depth 1
// its children, and so on. The README documents it as "levels below each root",
// so depth: 2 must reach <root>/a/b.
func TestScanDepthIsCountedFromTheRoot(t *testing.T) {
	root := t.TempDir()
	// One project at each level: <root>/l1, <root>/l1/l2, <root>/l1/l2/l3.
	// Each needs a marker that does not stop the walk, so use bare directories
	// with a compose file only at the leaf of each branch.
	for _, rel := range []string{"a", "a/b", "a/b/c"} {
		d := mkdirp(t, root, rel)
		write(t, d, "go.mod", "module "+filepath.Base(rel)+"\n")
	}

	// Nested projects never multiply: the walk stops at `a`, whatever the
	// depth. (Depth 0 is not expressible — an unset depth means the default.)
	for _, depth := range []int{1, 2, 3} {
		got := len(NewScanner(config.Scan{Roots: []string{root}, Depth: depth}).Scan())
		if got != 1 {
			t.Errorf("depth %d found %d projects, want 1", depth, got)
		}
	}

	// Only one project is ever found here because the walk stops at `a`, which
	// is itself a project. Siblings at deeper levels are what depth governs.
	wide := t.TempDir()
	for _, rel := range []string{"one", "mid/two", "mid/deeper/three"} {
		d := mkdirp(t, wide, rel)
		write(t, d, "go.mod", "module x\n")
	}
	counts := map[int]int{1: 1, 2: 2, 3: 3}
	for depth, want := range counts {
		got := len(NewScanner(config.Scan{Roots: []string{wide}, Depth: depth}).Scan())
		if got != want {
			t.Errorf("wide tree at depth %d found %d projects, want %d", depth, got, want)
		}
	}
}

func TestScanDescendsIntoWorkspaceRoots(t *testing.T) {
	root := t.TempDir()
	mono := mkdirp(t, root, "mono")
	write(t, mono, "package.json", `{"name":"mono","workspaces":["apps/*"]}`)
	web := mkdirp(t, mono, "apps", "web")
	write(t, web, "package.json", `{"name":"web","scripts":{"dev":"vite --port 5100"}}`)

	found := NewScanner(config.Scan{Roots: []string{root}, Depth: 4}).Scan()
	names := map[string]bool{}
	for _, p := range found {
		names[p.Name] = true
	}
	// The workspace root is a project, and so are its members: that is the
	// whole point of a monorepo.
	if !names["mono"] || !names["web"] {
		t.Errorf("expected both mono and web, got %v", names)
	}
}

func TestScanIgnoresConfiguredDirectories(t *testing.T) {
	root := t.TempDir()
	buried := mkdirp(t, root, "app", "node_modules", "dep")
	write(t, buried, "package.json", `{"name":"dep","scripts":{"dev":"x"}}`)
	app := mkdirp(t, root, "app")
	write(t, app, "package.json", `{"name":"app","scripts":{"dev":"x"}}`)

	s := NewScanner(config.Scan{Roots: []string{root}, Depth: 4, Ignore: []string{"node_modules"}})
	for _, p := range s.Scan() {
		if p.Name == "dep" {
			t.Fatal("a project inside node_modules should never be discovered")
		}
	}
}

func TestNodeStartCommandFollowsLockfile(t *testing.T) {
	for lock, want := range map[string]string{
		"pnpm-lock.yaml":    "pnpm run dev",
		"yarn.lock":         "yarn run dev",
		"bun.lockb":         "bun run dev",
		"package-lock.json": "npm run dev",
	} {
		dir := t.TempDir()
		write(t, dir, "package.json", `{"name":"x","scripts":{"dev":"vite"}}`)
		write(t, dir, lock, "")
		p, ok := Inspect(dir, nil)
		if !ok {
			t.Fatalf("%s: not detected", lock)
		}
		if p.StartCommand != want {
			t.Errorf("%s: start = %q, want %q", lock, p.StartCommand, want)
		}
	}
}

func TestNodeProjectWithoutDevScriptIsNotControllable(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"lib","scripts":{"build":"tsc"}}`)
	p, ok := Inspect(dir, nil)
	if !ok {
		t.Fatal("not detected")
	}
	// Guessing `npm run build` would be actively wrong: it is not a server.
	if p.Runtime != model.RuntimeNone || p.StartCommand != "" {
		t.Errorf("got runtime %q start %q, want no start mechanism", p.Runtime, p.StartCommand)
	}
}

func TestGoProjectWithAmbiguousCommandsIsNotControllable(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n")
	mkdirp(t, dir, "cmd", "api")
	mkdirp(t, dir, "cmd", "worker")

	p, ok := Inspect(dir, nil)
	if !ok {
		t.Fatal("not detected")
	}
	if p.Runtime != model.RuntimeNone {
		t.Errorf("two cmd/ binaries is ambiguous; got start %q", p.StartCommand)
	}

	single := t.TempDir()
	write(t, single, "go.mod", "module y\n")
	mkdirp(t, single, "cmd", "api")
	if p, _ := Inspect(single, nil); p.StartCommand != "go run ./cmd/api" {
		t.Errorf("single cmd/ dir: start = %q", p.StartCommand)
	}
}

func TestPortDetectionOnlyTrustsExplicitSources(t *testing.T) {
	if p, ok := portFromScript("vite --port 5173"); !ok || p != 5173 {
		t.Errorf("--port: got %d %v", p, ok)
	}
	if p, ok := portFromScript("next dev -p 4200"); !ok || p != 4200 {
		t.Errorf("-p: got %d %v", p, ok)
	}
	// No flag means no port. Assuming a framework default would show a number
	// that is wrong the moment PORT is set.
	if _, ok := portFromScript("next dev"); ok {
		t.Error("a bare dev script should not yield a port")
	}
	if _, ok := portFromScript("node -p 1"); !ok {
		t.Log("note: -p is ambiguous across tools; explicit config always wins")
	}
}

func TestPortFromDotEnv(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"x","scripts":{"dev":"node ."}}`)
	write(t, dir, ".env", "# comment\nNODE_ENV=development\nexport PORT=\"4321\"\n")

	p, ok := Inspect(dir, nil)
	if !ok {
		t.Fatal("not detected")
	}
	if len(p.Ports) != 1 || p.Ports[0].Port != 4321 || p.Ports[0].Source != "env" {
		t.Errorf("ports = %+v, want 4321 from env", p.Ports)
	}
}

func TestStableIDsAcrossScans(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		d := mkdirp(t, root, name)
		write(t, d, "compose.yaml", "services: {}")
	}
	s := NewScanner(config.Scan{Roots: []string{root}, Depth: 2})
	first := s.Scan()
	second := s.Scan()
	if len(first) != 3 {
		t.Fatalf("got %d projects", len(first))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("IDs must be stable across scans: %s != %s", first[i].ID, second[i].ID)
		}
		if first[i].ID != model.ID(first[i].Path) {
			t.Errorf("ID should derive from the path")
		}
	}
}

func TestComposeProjectNameMatchesDockerDefault(t *testing.T) {
	cases := map[string]string{
		"My Shop":     "myshop",
		"api_v2":      "api_v2",
		"web-ui":      "web-ui",
		"Project.Two": "projecttwo",
	}
	for in, want := range cases {
		if got := composeProjectName(filepath.Join("/tmp", in)); got != want {
			t.Errorf("composeProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}
