// Package gitinfo reads the current branch of a working tree.
//
// It parses .git directly rather than shelling out to git: the dashboard
// refreshes every project every few seconds, and spawning a process per project
// per tick to read one line of a file would be the most expensive thing the
// agent does.
package gitinfo

import (
	"os"
	"path/filepath"
	"strings"
)

// Info describes a project's git state.
type Info struct {
	// Branch is the checked-out branch, or a short commit for a detached HEAD.
	Branch string
	// Detached is true when HEAD points at a commit rather than a branch.
	Detached bool
}

// Read returns the git state of dir, or a zero Info when it is not a work tree.
func Read(dir string) Info {
	gitDir, ok := resolveGitDir(dir)
	if !ok {
		return Info{}
	}
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return Info{}
	}
	head := strings.TrimSpace(string(raw))

	if ref, ok := strings.CutPrefix(head, "ref:"); ok {
		ref = strings.TrimSpace(ref)
		// refs/heads/feature/x -> feature/x
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			return Info{Branch: name}
		}
		return Info{Branch: filepath.Base(ref)}
	}

	// Detached HEAD: the file holds the commit SHA.
	if len(head) >= 7 && isHex(head) {
		return Info{Branch: head[:7], Detached: true}
	}
	return Info{}
}

// resolveGitDir finds the .git directory for dir, following the "gitdir:"
// pointer that worktrees and submodules use instead of a real directory.
func resolveGitDir(dir string) (string, bool) {
	path := filepath.Join(dir, ".git")
	st, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if st.IsDir() {
		return path, true
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		return "", false
	}
	return target, true
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
