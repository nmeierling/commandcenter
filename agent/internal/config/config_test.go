package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingConfigIsNotAnError(t *testing.T) {
	// The agent is expected to be useful with zero configuration.
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("missing config should fall back to defaults, got %v", err)
	}
	if cfg.Listen != "127.0.0.1:47123" || cfg.Poll.Interval == 0 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.Path() != "" {
		t.Errorf("Path() should be empty when defaults were used, got %q", cfg.Path())
	}
}

func TestPartialConfigKeepsDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "poll:\n  interval: 10s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Poll.Interval != 10*time.Second {
		t.Errorf("interval = %v", cfg.Poll.Interval)
	}
	// Setting one poll field must not zero the others.
	if cfg.Poll.HealthTimeout == 0 || cfg.Poll.MaxConcurrent == 0 {
		t.Errorf("sibling defaults were lost: %+v", cfg.Poll)
	}
	if cfg.Listen == "" {
		t.Error("listen default was lost")
	}
}

func TestIgnoreListIsAdditive(t *testing.T) {
	cfg, err := Load(writeConfig(t, "scan:\n  ignore:\n    - .archive\n"))
	if err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, name := range cfg.Scan.Ignore {
		has[name] = true
	}
	// "also ignore this" is what people mean; replacing the list would start
	// walking node_modules.
	if !has[".archive"] || !has["node_modules"] {
		t.Errorf("ignore list = %v", cfg.Scan.Ignore)
	}
}

func TestDefaultPathPrefersWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(dir)

	// With nothing anywhere, the answer is the file to create.
	if got := DefaultPath(); got != HomePath() {
		t.Errorf("DefaultPath = %q, want %q", got, HomePath())
	}

	// An existing ~/.devcc/devcc.yaml is the personal config.
	if err := os.MkdirAll(filepath.Join(home, HomeDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HomePath(), []byte("scan:\n  depth: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultPath(); got != HomePath() {
		t.Errorf("DefaultPath = %q, want the home config %q", got, HomePath())
	}

	// A devcc.yaml in the working directory wins over it, so
	// `cd myrepo && devcc` needs no flags.
	local := filepath.Join(dir, "devcc.yaml")
	if err := os.WriteFile(local, []byte("scan:\n  depth: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultPath(); got != local {
		t.Errorf("DefaultPath = %q, want %q", got, local)
	}

	// And an explicit environment variable beats everything.
	t.Setenv("DEVCC_CONFIG", "/somewhere/else.yaml")
	if got := DefaultPath(); got != "/somewhere/else.yaml" {
		t.Errorf("DEVCC_CONFIG should win, got %q", got)
	}
}

func TestDevccYmlAlsoRecognised(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	local := filepath.Join(dir, "devcc.yml")
	if err := os.WriteFile(local, []byte("scan:\n  depth: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultPath(); got != local {
		t.Errorf("DefaultPath = %q, want %q", got, local)
	}
}

func TestRelativePathsResolveAgainstTheConfigFile(t *testing.T) {
	// The config lives in <tmp>/conf and points at ./projects. Running the
	// agent from an unrelated directory must not change what that means.
	base := t.TempDir()
	confDir := filepath.Join(base, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(confDir, "devcc.yaml")
	body := "scan:\n  roots:\n    - ./projects\nprojects:\n  - name: App\n    path: ./projects/app\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // run from somewhere else entirely

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(confDir, "projects")
	if len(cfg.Scan.Roots) != 1 || cfg.Scan.Roots[0] != wantRoot {
		t.Errorf("scan root = %v, want [%s]", cfg.Scan.Roots, wantRoot)
	}
	wantProject := filepath.Join(confDir, "projects", "app")
	if cfg.Projects[0].Path != wantProject {
		t.Errorf("project path = %q, want %q", cfg.Projects[0].Path, wantProject)
	}
}

func TestAbsoluteAndTildePathsAreLeftAlone(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	dir := t.TempDir()
	path := filepath.Join(dir, "devcc.yaml")
	body := "scan:\n  roots:\n    - /var/code\n    - ~/Projects\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scan.Roots[0] != "/var/code" {
		t.Errorf("absolute root = %q", cfg.Scan.Roots[0])
	}
	if cfg.Scan.Roots[1] != "/home/testuser/Projects" {
		t.Errorf("~ root = %q", cfg.Scan.Roots[1])
	}
}

func TestRepoDevccYamlParses(t *testing.T) {
	// The devcc.yaml committed at the repository root must actually load.
	t.Setenv("HOME", t.TempDir())
	if _, err := Load(filepath.Join("..", "..", "..", "devcc.yaml")); err != nil {
		t.Errorf("repository devcc.yaml does not parse: %v", err)
	}
}

func TestProjectPathsAreExpandedAndValidated(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	cfg, err := Load(writeConfig(t, "projects:\n  - name: App\n    path: ~/Projects/app\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Projects[0].Path != "/home/testuser/Projects/app" {
		t.Errorf("path = %q", cfg.Projects[0].Path)
	}

	if _, err := Load(writeConfig(t, "projects:\n  - name: NoPath\n")); err == nil {
		t.Error("a project without a path should be rejected")
	}

	dup := "projects:\n  - name: A\n    path: /tmp/x\n  - name: B\n    path: /tmp/x\n"
	if _, err := Load(writeConfig(t, dup)); err == nil {
		t.Error("two projects on the same path should be rejected")
	}
}

func TestRelativeComposeFileIsResolvedAgainstTheProject(t *testing.T) {
	cfg, err := Load(writeConfig(t,
		"projects:\n  - name: A\n    path: /tmp/app\n    composeFile: deploy/compose.yaml\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Projects[0].ComposeFile != "/tmp/app/deploy/compose.yaml" {
		t.Errorf("composeFile = %q", cfg.Projects[0].ComposeFile)
	}
}

func TestScanCanBeDisabled(t *testing.T) {
	cfg, err := Load(writeConfig(t, "scan:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanEnabled() {
		t.Error("scan.enabled: false should turn discovery off")
	}
	// Absent means on, which is what makes zero-config work.
	def, _ := Load(writeConfig(t, "listen: 127.0.0.1:1\n"))
	if !def.ScanEnabled() {
		t.Error("discovery should default to on")
	}
}

func TestBadYAMLIsReported(t *testing.T) {
	if _, err := Load(writeConfig(t, "scan: [this is not a mapping\n")); err == nil {
		t.Error("expected a parse error")
	}
}

func TestExampleConfigParses(t *testing.T) {
	// The file `devcc init` writes must actually load.
	t.Setenv("HOME", t.TempDir())
	if _, err := Load(writeConfig(t, Example)); err != nil {
		t.Errorf("the example config does not parse: %v", err)
	}
}
