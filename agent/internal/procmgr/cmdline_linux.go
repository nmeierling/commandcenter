//go:build linux

package procmgr

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processCmdline(pid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	parts := strings.FieldsFunc(string(raw), func(r rune) bool { return r == 0 })
	return strings.Join(parts, " ")
}

// processStartToken returns the process's start time in clock ticks since boot,
// field 22 of /proc/<pid>/stat. Together with the PID this uniquely identifies a
// process for as long as it lives, which is exactly what we need to detect PID
// reuse across agent restarts.
//
// The parse has one trap: field 2 is the executable name in parentheses and may
// itself contain spaces and parentheses, so fields are counted from after the
// last ')'.
func processStartToken(pid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	close := strings.LastIndex(string(raw), ")")
	if close < 0 {
		return ""
	}
	// After the closing paren, the next field is state (field 3), so the start
	// time (field 22) is at index 19 of the remainder.
	fields := strings.Fields(string(raw)[close+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}
