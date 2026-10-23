// Package ports inspects local TCP ports: which are listening, and which
// process owns them. Container-owned ports are attributed separately by the
// docker layer, because the host-side listener is usually docker-proxy (or
// nothing at all, when publishing goes through iptables).
package ports

import (
	"net"
	"strconv"
	"time"

	"github.com/devcc/agent/internal/model"
)

// Snapshot is a point-in-time view of listening TCP ports and their owners.
// Taking one snapshot per refresh cycle keeps the cost of inspecting N projects
// proportional to the machine, not to the number of projects.
type Snapshot struct {
	listening map[int]bool
	owners    map[int]*model.PortOwner
	// degraded is set when the platform lookup failed and we fell back to
	// dial probing, which cannot attribute ownership.
	degraded bool
}

// Take collects the current listening ports. Owner attribution is best-effort:
// on most systems we can only see processes owned by the same user.
func Take() *Snapshot {
	s, err := takePlatform()
	if err != nil || s == nil {
		return &Snapshot{listening: map[int]bool{}, owners: map[int]*model.PortOwner{}, degraded: true}
	}
	return s
}

// Synthetic builds a snapshot from known values instead of inspecting the
// machine. It exists so the status logic can be tested against port states that
// are impractical to arrange for real (a port held by another user, a container
// publishing through iptables with no visible host listener).
func Synthetic(listening []int, owners map[int]*model.PortOwner) *Snapshot {
	s := &Snapshot{listening: map[int]bool{}, owners: map[int]*model.PortOwner{}}
	for _, p := range listening {
		s.listening[p] = true
	}
	for port, owner := range owners {
		s.owners[port] = owner
		s.listening[port] = true
	}
	return s
}

// Listening reports whether anything is listening on the port. When the
// platform snapshot is unavailable it falls back to a connect probe.
func (s *Snapshot) Listening(port int) bool {
	if s == nil {
		return dialProbe(port)
	}
	if s.degraded {
		return dialProbe(port)
	}
	return s.listening[port]
}

// Owner returns the process holding the port, or nil if unknown.
func (s *Snapshot) Owner(port int) *model.PortOwner {
	if s == nil {
		return nil
	}
	return s.owners[port]
}

// Owners returns every attributed listening port. It is how we discover the
// port a dev server actually opened, as opposed to the one it was asked for.
func (s *Snapshot) Owners() map[int]*model.PortOwner {
	if s == nil {
		return nil
	}
	out := make(map[int]*model.PortOwner, len(s.owners))
	for port, owner := range s.owners {
		out[port] = owner
	}
	return out
}

// Degraded reports whether ownership information is unavailable on this system.
func (s *Snapshot) Degraded() bool { return s == nil || s.degraded }

// dialProbe is the portable fallback: if we can open a TCP connection to the
// loopback port, something is listening.
func dialProbe(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// IsFree reports whether a port can be bound right now. Used when explaining
// conflicts, not for status.
func IsFree(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
