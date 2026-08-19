package core

import (
	"sync"

	"github.com/devcc/agent/internal/model"
)

// EventType identifies what changed.
type EventType string

const (
	// EventProject carries one project's updated view.
	EventProject EventType = "project"
	// EventProjects carries the whole list, sent after discovery.
	EventProjects EventType = "projects"
	// EventAgent carries agent-level status (docker availability, errors).
	EventAgent EventType = "agent"
)

// Event is pushed to UI clients over SSE.
type Event struct {
	Type     EventType    `json:"type"`
	Project  *model.View  `json:"project,omitempty"`
	Projects []model.View `json:"projects,omitempty"`
	Agent    *AgentStatus `json:"agent,omitempty"`
}

// AgentStatus describes the agent's own capabilities, so the UI can explain
// missing features instead of showing broken buttons.
type AgentStatus struct {
	Version        string   `json:"version"`
	DockerOK       bool     `json:"dockerOk"`
	DockerVersion  string   `json:"dockerVersion,omitempty"`
	DockerError    string   `json:"dockerError,omitempty"`
	DaemonOK       bool     `json:"daemonOk"`
	CanOpenBrowser bool     `json:"canOpenBrowser"`
	PortOwnersOK   bool     `json:"portOwnersOk"`
	ConfigPath     string   `json:"configPath,omitempty"`
	ScanRoots      []string `json:"scanRoots,omitempty"`
	// MissingRoots are configured roots that do not exist on disk.
	MissingRoots []string `json:"missingRoots,omitempty"`
	Platform     string   `json:"platform"`
	ProjectCount int      `json:"projectCount"`
}

// hub fans events out to connected clients.
type hub struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Event
}

func newHub() *hub { return &hub{subs: map[int]chan Event{}} }

func (h *hub) subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
		h.mu.Unlock()
	}
}

func (h *hub) publish(ev Event) {
	h.mu.Lock()
	subs := make([]chan Event, 0, len(h.subs))
	for _, ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// A client that cannot keep up with status updates will resync on
			// its next full fetch; dropping is better than blocking the
			// reconciler.
		}
	}
}

func (h *hub) clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
