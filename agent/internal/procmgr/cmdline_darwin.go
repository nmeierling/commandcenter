//go:build darwin

package procmgr

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func processCmdline(pid int) string {
	return psField(pid, "command=")
}

// processStartToken uses the process start time, which macOS exposes through
// ps. Combined with the PID it identifies a process uniquely, so an agent
// restart cannot mistake a recycled PID for the dev server it used to manage.
func processStartToken(pid int) string {
	return psField(pid, "lstart=")
}

func psField(pid int, format string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", format, "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}
