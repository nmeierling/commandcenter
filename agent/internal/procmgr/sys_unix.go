//go:build unix

package procmgr

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// shellArgv runs the user's command through a shell, because that is what they
// wrote it for: `npm run dev`, `make serve`, `go run ./cmd/api 2>&1`. We keep
// the shell as the process group leader and signal the group, so pipelines and
// child processes go down together.
func shellArgv(command string) []string {
	shell := os.Getenv("SHELL")
	if shell == "" || strings.Contains(shell, "fish") {
		// fish's -c semantics differ enough that we prefer a POSIX shell.
		shell = "/bin/sh"
	}
	return []string{shell, "-lc", command}
}

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func processGroup(pid int) int {
	if pgid, err := syscall.Getpgid(pid); err == nil {
		return pgid
	}
	// With Setpgid the child leads a group whose ID equals its PID.
	return pid
}

// pidAlive reports whether pid exists and is still the process we started.
//
// Identity is checked with the kernel's process start time, not the command
// line. A shell given `-c` with a single command execs it in place, which
// rewrites /proc/<pid>/cmdline — so comparing command text would decide that a
// perfectly healthy `npm run dev` is a stranger, and Stop would silently
// signal nothing. Start time is immutable for the life of a process, and the
// (pid, start time) pair is unique on the machine.
//
// An empty token skips the identity check.
func pidAlive(pid int, token string) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		// ESRCH: gone. EPERM: exists but owned by another user, which means the
		// PID was reused — either way it is not our process.
		_ = errors.Is(err, syscall.EPERM)
		return false
	}
	if token == "" {
		return true
	}
	actual := processStartToken(pid)
	if actual == "" {
		// Unreadable on this system; fall back to plain liveness.
		return true
	}
	return actual == token
}

func signalGroup(pid, pgid int, token string, sig syscall.Signal) error {
	// Re-verify identity immediately before signalling: a negative PID targets
	// a whole process group, and getting that wrong is expensive.
	if !pidAlive(pid, token) {
		return nil
	}
	if pgid > 0 {
		if err := syscall.Kill(-pgid, sig); err == nil {
			return nil
		} else if !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// inProcessGroup reports whether pid is a member of pgid. Dev servers fork
// children (vite, tsc, nodemon), and those children are the ones that actually
// bind the port, so group membership is how we attribute it back to a project.
func inProcessGroup(pid, pgid int) bool {
	if pgid <= 0 {
		return false
	}
	g, err := syscall.Getpgid(pid)
	return err == nil && g == pgid
}

func terminateGroup(pid, pgid int, token string) error {
	return signalGroup(pid, pgid, token, syscall.SIGTERM)
}

func killGroup(pid, pgid int, token string) error {
	return signalGroup(pid, pgid, token, syscall.SIGKILL)
}

// KillPID terminates an arbitrary process by PID, used by the port-conflict
// "resolve" action. It never targets a process group, because the port holder
// may be unrelated to anything we started.
func KillPID(pid int, force bool) error {
	if pid <= 1 {
		return errors.New("refusing to signal pid <= 1")
	}
	if pid == os.Getpid() {
		return errors.New("refusing to signal the agent itself")
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(pid, sig)
}
