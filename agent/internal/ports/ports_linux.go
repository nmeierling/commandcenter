//go:build linux

package ports

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/devcc/agent/internal/model"
)

// takePlatform reads /proc/net/tcp{,6} for listening sockets, then maps the
// socket inodes back to PIDs by walking /proc/<pid>/fd. Sockets owned by other
// users are visible in /proc/net/tcp but their /proc/<pid>/fd is unreadable, so
// those ports are reported as listening with an unknown owner.
func takePlatform() (*Snapshot, error) {
	s := &Snapshot{listening: map[int]bool{}, owners: map[int]*model.PortOwner{}}

	inodeToPort := map[uint64]int{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		parseProcNet(raw, s.listening, inodeToPort)
	}
	if len(s.listening) == 0 {
		// /proc unreadable (unusual). Let the caller fall back to dialing.
		if _, err := os.Stat("/proc/net/tcp"); err != nil {
			return nil, err
		}
	}

	if len(inodeToPort) > 0 {
		attributeOwners(inodeToPort, s.owners)
	}
	return s, nil
}

// parseProcNet extracts listening sockets. Lines look like:
//
//	sl local_address rem_address st tx_queue:rx_queue tr:when retrnsmt uid timeout inode
//	0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00 1000 0 45231 1 ...
//
// st == 0A is TCP_LISTEN.
func parseProcNet(raw []byte, listening map[int]bool, inodeToPort map[uint64]int) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		_, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port64, err := strconv.ParseUint(portHex, 16, 32)
		if err != nil || port64 == 0 || port64 > 65535 {
			continue
		}
		port := int(port64)
		listening[port] = true
		if inode, err := strconv.ParseUint(fields[9], 10, 64); err == nil && inode != 0 {
			// Keep the first inode seen for a port; a dual-stack listener has
			// one per address family and either resolves to the same process.
			if _, dup := inodeToPort[inode]; !dup {
				inodeToPort[inode] = port
			}
		}
	}
}

// attributeOwners walks /proc once, matching socket inodes to processes.
func attributeOwners(inodeToPort map[uint64]int, owners map[int]*model.PortOwner) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	want := len(inodeToPort)
	found := 0
	for _, e := range entries {
		if found >= want {
			return
		}
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // another user's process
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 64)
			if err != nil {
				continue
			}
			port, ok := inodeToPort[inode]
			if !ok {
				continue
			}
			if _, dup := owners[port]; dup {
				continue
			}
			owners[port] = &model.PortOwner{
				PID:     pid,
				Process: procComm(pid),
				Command: procCmdline(pid),
			}
			found++
		}
	}
}

func procComm(pid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func procCmdline(pid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	parts := strings.FieldsFunc(string(raw), func(r rune) bool { return r == 0 })
	cmd := strings.Join(parts, " ")
	if len(cmd) > 300 {
		cmd = cmd[:300] + "…"
	}
	return cmd
}
