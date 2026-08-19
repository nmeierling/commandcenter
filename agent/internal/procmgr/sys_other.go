//go:build !unix

package procmgr

import (
	"errors"
	"os/exec"
)

var errUnsupported = errors.New("process supervision is not supported on this platform")

func shellArgv(command string) []string   { return []string{"cmd", "/c", command} }
func setProcessGroup(cmd *exec.Cmd)       {}
func processGroup(pid int) int            { return pid }
func processCmdline(pid int) string       { return "" }
func processStartToken(pid int) string    { return "" }
func pidAlive(pid int, token string) bool { return false }
func inProcessGroup(pid, pgid int) bool   { return false }

func terminateGroup(pid, pgid int, token string) error { return errUnsupported }
func killGroup(pid, pgid int, token string) error      { return errUnsupported }

// KillPID is unimplemented outside unix.
func KillPID(pid int, force bool) error { return errUnsupported }
