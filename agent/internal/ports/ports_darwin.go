//go:build darwin

package ports

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/devcc/agent/internal/model"
)

// takePlatform shells out to lsof once for every listening TCP socket. macOS
// has no /proc, and the alternatives (libproc via cgo) are not worth the build
// complexity for a developer tool that already depends on the docker CLI.
func takePlatform() (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// -F pcn gives machine-readable pid / command / name records.
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pcn")
	out, err := cmd.Output()
	if err != nil {
		// lsof exits 1 when it finds nothing, which is not an error for us.
		var ee *exec.ExitError
		if !asExitError(err, &ee) || len(out) == 0 {
			return nil, err
		}
	}

	s := &Snapshot{listening: map[int]bool{}, owners: map[int]*model.PortOwner{}}
	var pid int
	var command string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			command = ""
		case 'c':
			command = line[1:]
		case 'n':
			port, ok := portFromLsofName(line[1:])
			if !ok {
				continue
			}
			s.listening[port] = true
			if _, dup := s.owners[port]; !dup && pid > 0 {
				s.owners[port] = &model.PortOwner{PID: pid, Process: command, Command: fullCommand(pid, command)}
			}
		}
	}
	return s, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// portFromLsofName parses lsof name records such as "*:3000",
// "127.0.0.1:3000", "[::1]:3000" and "localhost:3000".
func portFromLsofName(name string) (int, bool) {
	if i := strings.Index(name, "->"); i >= 0 {
		name = name[:i]
	}
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(name[i+1:]))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// fullCommand asks ps for the full argv, which lsof truncates.
func fullCommand(pid int, fallback string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return fallback
	}
	cmd := strings.TrimSpace(string(out))
	if cmd == "" {
		return fallback
	}
	if len(cmd) > 300 {
		cmd = cmd[:300] + "…"
	}
	return cmd
}
