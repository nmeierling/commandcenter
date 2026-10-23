package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const handWritten = `# My dev setup.
scan:
  roots:
    - ~/Projects   # where everything lives
  depth: 3

projects:
  - name: Dashboard
    path: /work/dashboard
    health:
      url: http://localhost:3001   # readiness
`

func writerFor(t *testing.T, body string) (*Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devcc.yaml")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &Writer{Path: path}, path
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The config file is hand-written and usually checked in. Editing it must not
// cost the user their comments or their formatting.
func TestWriterPreservesCommentsAndLayout(t *testing.T) {
	w, path := writerFor(t, handWritten)
	if err := w.SetSubstitute("/work/dashboard", "Dashboard", "db", "ecommerce/db"); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)

	for _, want := range []string{
		"# My dev setup.",
		"# where everything lives",
		"# readiness",
		"substitutes:",
		"db: ecommerce/db",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Two-space indentation, not the encoder's default of four.
	if !strings.Contains(out, "  - name: Dashboard") {
		t.Errorf("indentation changed:\n%s", out)
	}
	// And the file still loads.
	if _, err := Load(path); err != nil {
		t.Errorf("rewritten config no longer parses: %v", err)
	}
}

func TestWriterAddsProjectEntryWhenMissing(t *testing.T) {
	w, path := writerFor(t, handWritten)
	if err := w.SetPortOverride("/work/api", "API", 4000, 4100); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range cfg.Projects {
		if p.Path == "/work/api" {
			found = true
			if p.PortOverrides[4000] != 4100 {
				t.Errorf("overrides = %v", p.PortOverrides)
			}
		}
	}
	if !found {
		t.Errorf("new project entry not written:\n%s", read(t, path))
	}
	// The pre-existing project survived.
	if len(cfg.Projects) != 2 {
		t.Errorf("got %d projects, want 2", len(cfg.Projects))
	}
}

func TestWriterCreatesFileWhenAbsent(t *testing.T) {
	w, path := writerFor(t, "")
	if err := w.SetSubstitute("/work/api", "API", "db", "shop/db"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].Substitutes["db"] != "shop/db" {
		t.Errorf("got %+v", cfg.Projects)
	}
}

func TestWriterRemovesEntriesCleanly(t *testing.T) {
	w, path := writerFor(t, handWritten)
	if err := w.SetPortOverride("/work/dashboard", "Dashboard", 3001, 3101); err != nil {
		t.Fatal(err)
	}
	if err := w.SetSubstitute("/work/dashboard", "Dashboard", "db", "shop/db"); err != nil {
		t.Fatal(err)
	}
	// Setting a port back to its declared value removes it, and the empty map
	// goes with it rather than leaving `portOverrides: {}` behind.
	if err := w.SetPortOverride("/work/dashboard", "Dashboard", 3001, 3001); err != nil {
		t.Fatal(err)
	}
	if err := w.SetSubstitute("/work/dashboard", "Dashboard", "db", ""); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)
	if strings.Contains(out, "portOverrides") || strings.Contains(out, "substitutes") {
		t.Errorf("empty maps left behind:\n%s", out)
	}
	if !strings.Contains(out, "# My dev setup.") {
		t.Errorf("comments lost during removal:\n%s", out)
	}
}

func TestWriterMatchesProjectsByResolvedPath(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	body := "projects:\n  - name: App\n    path: ~/code/app\n"
	w, path := writerFor(t, body)
	// The caller has an absolute path; the file has a ~ path. They are the same
	// project and must not produce a duplicate entry.
	if err := w.SetSubstitute("/home/tester/code/app", "App", "db", "shop/db"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("got %d projects, want 1:\n%s", len(cfg.Projects), read(t, path))
	}
	if cfg.Projects[0].Substitutes["db"] != "shop/db" {
		t.Errorf("substitute not applied: %+v", cfg.Projects[0])
	}
}

func TestWriterKeysAreSorted(t *testing.T) {
	w, path := writerFor(t, handWritten)
	for _, p := range [][2]int{{9000, 9100}, {3000, 3100}, {5000, 5100}} {
		if err := w.SetPortOverride("/work/dashboard", "Dashboard", p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	out := read(t, path)
	// Stable order keeps the file from churning in version control.
	i3, i5, i9 := strings.Index(out, "3000:"), strings.Index(out, "5000:"), strings.Index(out, "9000:")
	if !(i3 < i5 && i5 < i9) {
		t.Errorf("keys not sorted numerically:\n%s", out)
	}
}

func TestWriterExpandsAnEmptyFlowSequence(t *testing.T) {
	// The shipped template writes `projects: []`. Appending to a flow-style
	// sequence without switching to block style would put every project on one
	// unreadable line.
	w, path := writerFor(t, "scan:\n  depth: 2\nprojects: []\n")
	if err := w.SetSubstitute("/work/api", "API", "db", "shop/db"); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)
	if !strings.Contains(out, "\n  - name: API") {
		t.Errorf("expected block style:\n%s", out)
	}
	if strings.Contains(out, "[{") {
		t.Errorf("flow style leaked:\n%s", out)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("result does not parse: %v", err)
	}
}

func TestWriterHandlesTheShippedTemplate(t *testing.T) {
	// The real devcc.yaml at the repository root must survive being written to.
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "devcc.yaml"))
	if err != nil {
		t.Skip("no repository devcc.yaml")
	}
	w, path := writerFor(t, string(raw))
	if err := w.SetPortOverride("/work/api", "API", 3000, 3100); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("template no longer parses after a write: %v", err)
	}
	out := read(t, path)
	if !strings.Contains(out, "# Which directories to scan") {
		t.Errorf("template comments lost:\n%s", out[:400])
	}
}

func TestWriterRoundTripsCategories(t *testing.T) {
	w, path := writerFor(t, handWritten)
	if err := w.SetCategories("/work/dashboard", "Dashboard", []string{"frontend", "team-a"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range cfg.Projects {
		if p.Path == "/work/dashboard" {
			got = p.Categories
		}
	}
	if len(got) != 2 || got[0] != "frontend" || got[1] != "team-a" {
		t.Errorf("categories = %v", got)
	}
	if !strings.Contains(read(t, path), "# My dev setup.") {
		t.Error("comments lost")
	}

	// Replacing rather than appending, and an empty list removes the key so the
	// file does not accumulate `categories: []`.
	if err := w.SetCategories("/work/dashboard", "Dashboard", []string{"backend"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(path)
	for _, p := range cfg.Projects {
		if p.Path == "/work/dashboard" && (len(p.Categories) != 1 || p.Categories[0] != "backend") {
			t.Errorf("categories not replaced: %v", p.Categories)
		}
	}
	if err := w.SetCategories("/work/dashboard", "Dashboard", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, path), "categories") {
		t.Errorf("empty list left behind:\n%s", read(t, path))
	}
}
