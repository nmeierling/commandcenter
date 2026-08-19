package procmgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devcc/agent/internal/logbuf"
)

func newManager(t *testing.T) (*Manager, *logbuf.Buffer) {
	t.Helper()
	buf := logbuf.New(500)
	m, err := New(t.TempDir(), func(string) *logbuf.Buffer { return buf })
	if err != nil {
		t.Fatal(err)
	}
	return m, buf
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func TestStartCapturesOutputAndReportsRunning(t *testing.T) {
	m, buf := newManager(t)
	dir := t.TempDir()

	if _, err := m.Start(Spec{ID: "p1", Dir: dir, Command: "echo hello-from-child; sleep 30"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Stop("p1", 2*time.Second) }()

	if !m.Running("p1") {
		t.Fatal("expected the process to be running")
	}
	if !waitFor(t, 5*time.Second, func() bool {
		for _, l := range buf.Tail(50) {
			if strings.Contains(l.Text, "hello-from-child") {
				return true
			}
		}
		return false
	}) {
		t.Errorf("child output never reached the log buffer: %+v", buf.Tail(20))
	}
}

// The reason for process groups: `sh -c` spawns the real server as a child, and
// signalling only the shell leaves that child alive holding the port.
func TestStopKillsTheWholeProcessGroup(t *testing.T) {
	m, _ := newManager(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "child.pid")

	// The shell backgrounds a grandchild, records its PID, then waits.
	cmd := "sh -c 'sleep 60 & echo $! > " + marker + "; wait' "
	if _, err := m.Start(Spec{ID: "p1", Dir: dir, Command: cmd}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}) {
		t.Fatal("grandchild never started")
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	grandchild := strings.TrimSpace(string(raw))

	if err := m.Stop("p1", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if m.Running("p1") {
		t.Error("supervised process still running after Stop")
	}
	if !waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join("/proc", grandchild))
		return os.IsNotExist(err)
	}) {
		t.Errorf("grandchild pid %s survived Stop: the process group was not signalled", grandchild)
	}
}

func TestStartTwiceIsIdempotent(t *testing.T) {
	m, _ := newManager(t)
	dir := t.TempDir()

	first, err := m.Start(Spec{ID: "p1", Dir: dir, Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Stop("p1", 2*time.Second) }()

	second, err := m.Start(Spec{ID: "p1", Dir: dir, Command: "sleep 30"})
	if err != ErrAlreadyRunning {
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}
	if first.PID != second.PID {
		t.Error("a second start should return the existing process, not spawn another")
	}
}

func TestStopReportsCleanlyAndDistinguishesCrashes(t *testing.T) {
	m, _ := newManager(t)
	dir := t.TempDir()

	if _, err := m.Start(Spec{ID: "stopme", Dir: dir, Command: "sleep 30"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop("stopme", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	info := m.Info("stopme")
	if info == nil || info.Running {
		t.Fatalf("expected a stopped process, got %+v", info)
	}
	// A requested stop must not look like a crash, or every Stop click would
	// paint the card red.
	if !info.StopRequested {
		t.Error("StopRequested should be set after Stop")
	}
	if info.ExitCode != nil {
		t.Errorf("no exit code should be reported for a requested stop, got %d", *info.ExitCode)
	}

	if _, err := m.Start(Spec{ID: "crash", Dir: dir, Command: "exit 3"}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		i := m.Info("crash")
		return i != nil && !i.Running
	}) {
		t.Fatal("crashing process never reported as exited")
	}
	info = m.Info("crash")
	if info.StopRequested {
		t.Error("a crash must not be marked as a requested stop")
	}
	if info.ExitCode == nil || *info.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", info.ExitCode)
	}
}

func TestAdoptReattachesSurvivingProcesses(t *testing.T) {
	stateDir := t.TempDir()
	buf := logbuf.New(200)
	first, err := New(stateDir, func(string) *logbuf.Buffer { return buf })
	if err != nil {
		t.Fatal(err)
	}
	proc, err := first.Start(Spec{ID: "svc", Dir: t.TempDir(), Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}

	// A second manager over the same state directory stands in for the agent
	// being restarted while the dev server keeps running.
	second, err := New(stateDir, func(string) *logbuf.Buffer { return buf })
	if err != nil {
		t.Fatal(err)
	}
	adopted, stale := second.Adopt()
	if adopted != 1 || stale != 0 {
		t.Fatalf("adopted=%d stale=%d, want 1/0", adopted, stale)
	}
	if !second.Running("svc") {
		t.Error("adopted process should report as running")
	}
	if info := second.Info("svc"); info == nil || info.PID != proc.PID {
		t.Errorf("adopted the wrong pid: %+v", info)
	}
	// And the new manager can still stop it.
	if err := second.Stop("svc", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if second.Running("svc") {
		t.Error("Stop failed on an adopted process")
	}
}

func TestAdoptDiscardsDeadAndReusedPIDs(t *testing.T) {
	stateDir := t.TempDir()
	buf := logbuf.New(50)
	m, err := New(stateDir, func(string) *logbuf.Buffer { return buf })
	if err != nil {
		t.Fatal(err)
	}

	// PID 1 certainly exists, but its start time cannot match the token we
	// recorded — that is exactly what PID reuse looks like. Adopting it would
	// mean signalling init's process group on the next Stop.
	state := `{"procs":[{"id":"ghost","pid":1,"pgid":1,"command":"sleep 30",` +
		`"dir":"/tmp","argv":["/bin/sh","-lc","sleep 30"],"startToken":"999999999999",` +
		`"logPath":"/tmp/x.log"}]}`
	if err := os.WriteFile(filepath.Join(stateDir, "processes.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	adopted, stale := m.Adopt()
	if adopted != 0 || stale != 1 {
		t.Errorf("adopted=%d stale=%d, want 0/1 — a reused PID must not be adopted", adopted, stale)
	}
}

func TestKillPIDRefusesDangerousTargets(t *testing.T) {
	if err := KillPID(1, false); err == nil {
		t.Error("KillPID(1) should be refused")
	}
	if err := KillPID(os.Getpid(), false); err == nil {
		t.Error("KillPID should refuse to signal the agent itself")
	}
}

func TestTailFileSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	var body strings.Builder
	for i := 0; i < 500; i++ {
		body.WriteString("line ")
		body.WriteString(strings.Repeat("x", 10))
		body.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := TailFileSnapshot(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 10 {
		t.Errorf("got %d lines, want 10", len(lines))
	}
}
