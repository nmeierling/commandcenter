package gitinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBranchFromHead(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	if got := Read(dir); got.Branch != "main" || got.Detached {
		t.Errorf("got %+v, want main", got)
	}
}

func TestBranchWithSlashes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/feature/nested/name\n")
	// The full name after refs/heads/ is the branch; taking the basename would
	// report "name" for feature/nested/name.
	if got := Read(dir); got.Branch != "feature/nested/name" {
		t.Errorf("got %q", got.Branch)
	}
}

func TestDetachedHead(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".git", "HEAD"), "9fceb02d0ae598e95dc970b74767f19372d61af8\n")
	got := Read(dir)
	if got.Branch != "9fceb02" || !got.Detached {
		t.Errorf("got %+v, want a short sha marked detached", got)
	}
}

func TestWorktreeGitFile(t *testing.T) {
	// A linked worktree has a .git *file* pointing at the real git directory.
	base := t.TempDir()
	realGit := filepath.Join(base, "store", "worktrees", "wt")
	write(t, filepath.Join(realGit, "HEAD"), "ref: refs/heads/experiment\n")

	tree := filepath.Join(base, "tree")
	write(t, filepath.Join(tree, ".git"), "gitdir: "+realGit+"\n")

	if got := Read(tree); got.Branch != "experiment" {
		t.Errorf("got %+v, want experiment", got)
	}
}

func TestNotAGitRepo(t *testing.T) {
	if got := Read(t.TempDir()); got.Branch != "" {
		t.Errorf("got %+v, want empty", got)
	}
}

func TestUnreadableHeadIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// .git exists but has no HEAD; the dashboard just shows no branch.
	if got := Read(dir); got.Branch != "" {
		t.Errorf("got %+v", got)
	}
}
