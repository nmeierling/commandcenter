// Package model defines the core domain types shared across the agent.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Status is the coarse lifecycle state shown in the dashboard.
type Status string

const (
	StatusRunning   Status = "running"
	StatusStopped   Status = "stopped"
	StatusStarting  Status = "starting"
	StatusStopping  Status = "stopping"
	StatusUnhealthy Status = "unhealthy"
	StatusConflict  Status = "conflict"
	StatusError     Status = "error"
	StatusUnknown   Status = "unknown"
)

// Kind is the detected project technology.
type Kind string

const (
	KindCompose Kind = "compose"
	KindNode    Kind = "node"
	KindGo      Kind = "go"
	KindRust    Kind = "rust"
	KindPython  Kind = "python"
	KindCustom  Kind = "custom"
)

// Runtime says how a project is started and stopped.
type Runtime string

const (
	// RuntimeCompose delegates to the docker compose CLI in the project directory.
	RuntimeCompose Runtime = "compose"
	// RuntimeProcess spawns a supervised child process group.
	RuntimeProcess Runtime = "process"
	// RuntimeNone means the project is observable but not controllable: we can
	// report status from ports and health checks but have no start command.
	RuntimeNone Runtime = "none"
)

// PortState describes what we found on an expected port.
type PortState string

const (
	// PortFree means nothing is listening.
	PortFree PortState = "free"
	// PortOwned means the port is listening and belongs to this project.
	PortOwned PortState = "owned"
	// PortConflict means something is listening that is not this project.
	PortConflict PortState = "conflict"
	// PortUnknownOwner means the port is listening but we could not attribute it.
	PortUnknownOwner PortState = "unknown-owner"
)

// PortOwner identifies whoever is holding a TCP port.
type PortOwner struct {
	PID         int    `json:"pid,omitempty"`
	Process     string `json:"process,omitempty"`
	Command     string `json:"command,omitempty"`
	ContainerID string `json:"containerId,omitempty"`
	Container   string `json:"container,omitempty"`
	// ComposeProject is the compose project label of the owning container, if any.
	// It is how we tell "my own container" apart from "someone else's".
	ComposeProject string `json:"composeProject,omitempty"`
}

// Port is first-class project metadata: an expected port plus what we observed.
type Port struct {
	Port int `json:"port"`
	// Source records where the expectation came from: "compose", "config" or "observed".
	Source string     `json:"source"`
	State  PortState  `json:"state"`
	Owner  *PortOwner `json:"owner,omitempty"`
	// Service is the compose service that declares the port, when known.
	Service string `json:"service,omitempty"`
}

// HealthCheck configures an HTTP probe.
type HealthCheck struct {
	URL            string        `json:"url"`
	ExpectStatus   int           `json:"expectStatus,omitempty"`
	Timeout        time.Duration `json:"-"`
	TimeoutSeconds float64       `json:"timeoutSeconds,omitempty"`
}

// HealthResult is the outcome of the most recent probe.
type HealthResult struct {
	Checked    bool      `json:"checked"`
	OK         bool      `json:"ok"`
	StatusCode int       `json:"statusCode,omitempty"`
	Error      string    `json:"error,omitempty"`
	LatencyMS  int64     `json:"latencyMs,omitempty"`
	At         time.Time `json:"at,omitempty"`
}

// Container is a compose container belonging to a project.
type Container struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
	Status  string `json:"status,omitempty"`
	Image   string `json:"image,omitempty"`
	ID      string `json:"id,omitempty"`
}

// ProcessInfo describes a supervised child process.
type ProcessInfo struct {
	PID       int       `json:"pid"`
	Command   string    `json:"command"`
	StartedAt time.Time `json:"startedAt"`
	Running   bool      `json:"running"`
	ExitCode  *int      `json:"exitCode,omitempty"`
	ExitError string    `json:"exitError,omitempty"`
	// StopRequested distinguishes "the user pressed Stop" from "it crashed".
	// Both leave a non-zero exit status — a signalled process reports -1 — so
	// without this flag every clean shutdown would be reported as an error.
	StopRequested bool `json:"stopRequested,omitempty"`
}

// Project is a discovered or configured local project.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	Kind Kind   `json:"kind"`
	// Markers are the files that caused this directory to be detected.
	Markers []string `json:"markers,omitempty"`
	Runtime Runtime  `json:"runtime"`
	// Source is "discovered", "configured" or "both".
	Source string `json:"source"`

	// StartCommand/StopCommand apply to RuntimeProcess projects.
	StartCommand string            `json:"startCommand,omitempty"`
	StopCommand  string            `json:"stopCommand,omitempty"`
	Env          map[string]string `json:"-"`

	// ComposeFile is the compose file path for RuntimeCompose projects.
	ComposeFile    string `json:"composeFile,omitempty"`
	ComposeProject string `json:"composeProject,omitempty"`

	Ports  []Port       `json:"ports"`
	URLs   []string     `json:"urls"`
	Health *HealthCheck `json:"health,omitempty"`

	Disabled bool `json:"disabled,omitempty"`
}

// State is the live, frequently-changing half of a project card. It is kept
// separate from Project so discovery and status refresh can update
// independently without racing on one struct.
type State struct {
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Ports and Containers are never omitempty: the UI iterates them, and an
	// absent key and an empty list are the same thing in Go but not in
	// JavaScript.
	Containers []Container  `json:"containers"`
	Ports      []Port       `json:"ports"`
	Process    *ProcessInfo `json:"process,omitempty"`
	Health     HealthResult `json:"health"`
	// Busy marks an in-flight lifecycle action ("start", "stop", "restart").
	Busy      string    `json:"busy,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// View is a project plus its current state, as sent to the UI.
type View struct {
	Project
	State State `json:"state"`
}

// ID derives a stable project identifier from its absolute path, so IDs survive
// agent restarts and directory reordering.
func ID(absPath string) string {
	sum := sha256.Sum256([]byte(absPath))
	return hex.EncodeToString(sum[:])[:12]
}
