package procmgr

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/devcc/agent/internal/logbuf"
)

// RunOnce executes a short-lived command (a custom stop command, `docker stop`)
// and waits for it, streaming output into the project's log buffer.
func RunOnce(ctx context.Context, dir, command string, env map[string]string, buf *logbuf.Buffer) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	argv := shellArgv(command)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = buildEnv(env)
	setProcessGroup(cmd)

	out, err := cmd.CombinedOutput()
	if buf != nil {
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if line != "" {
				buf.Append("agent", line)
			}
		}
	}
	if err != nil {
		msg := strings.TrimSpace(lastLines(string(out), 3))
		if msg != "" {
			return fmt.Errorf("%s: %s", err, msg)
		}
		return err
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// OwnsPID reports whether pid belongs to the process group we started for id.
// This is what lets the dashboard say "port 3000 is yours" instead of just
// "port 3000 is busy".
func (m *Manager) OwnsPID(id string, pid int) bool {
	if pid <= 0 {
		return false
	}
	m.mu.Lock()
	p, ok := m.procs[id]
	m.mu.Unlock()
	if !ok || !p.alive() {
		return false
	}
	if pid == p.PID {
		return true
	}
	return inProcessGroup(pid, p.PGID)
}
