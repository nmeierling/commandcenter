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
	// PortSubstituted means something else is serving this port on purpose,
	// because the user chose it as a stand-in.
	PortSubstituted PortState = "substituted"
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
	// ContainerPort is the port inside the container that Port maps to. Moving
	// a published port means changing only the host side of that pair.
	ContainerPort int `json:"containerPort,omitempty"`
	// Variable is the environment variable a compose file uses to declare this
	// port, as in "${WEB_PORT:-3000}:80". Its presence is what makes the port
	// changeable without editing the compose file.
	Variable string `json:"variable,omitempty"`
	// Declared is the port as written in the project, when an override is in
	// effect and Port is the overridden value.
	Declared int `json:"declared,omitempty"`
}

// PortChangeMechanism is how (or whether) a project's host port can be moved.
type PortChangeMechanism string

const (
	// PortChangeNone means we have no safe way to move the port.
	PortChangeNone PortChangeMechanism = "none"
	// PortChangeEnv sets PORT in a supervised process's environment.
	PortChangeEnv PortChangeMechanism = "env"
	// PortChangeCommand rewrites an explicit --port flag in the start command.
	PortChangeCommand PortChangeMechanism = "command"
	// PortChangeComposeVar sets the variable the compose file already uses.
	PortChangeComposeVar PortChangeMechanism = "compose-var"
	// PortChangeComposeOverride writes an agent-owned compose override file
	// using the `!override` tag. Never touches files inside the project.
	PortChangeComposeOverride PortChangeMechanism = "compose-override"
)

// PortControl describes whether this project's ports can be moved, and how.
// The dashboard uses it to decide whether offering "start on another port" is
// honest, rather than promising something that silently will not take effect.
type PortControl struct {
	CanChange bool                `json:"canChange"`
	Mechanism PortChangeMechanism `json:"mechanism"`
	// Reason explains why CanChange is false, in terms the user can act on.
	Reason string `json:"reason,omitempty"`
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

// UnitState is the lifecycle state of a single container or supervised app.
type UnitState string

const (
	// UnitRunning is up and serving.
	UnitRunning UnitState = "running"
	// UnitExited ran and stopped.
	UnitExited UnitState = "exited"
	// UnitCreated exists but was never started.
	UnitCreated UnitState = "created"
	// UnitNotCreated is declared in the compose file but has no container yet.
	// Showing these is the difference between "this project has four services"
	// and "this project has nothing", which is only true until you start it.
	UnitNotCreated UnitState = "not-created"
	// UnitUnknown covers a project we cannot inspect (docker down, no runtime).
	UnitUnknown UnitState = "unknown"
	// UnitSubstituted means this unit is deliberately not running because
	// something else is providing it.
	UnitSubstituted UnitState = "substituted"
)

// Substitution records that a unit is provided by something already running.
type Substitution struct {
	Target      string `json:"target"`
	ProjectID   string `json:"projectId,omitempty"`
	ProjectName string `json:"projectName,omitempty"`
	UnitID      string `json:"unitId,omitempty"`
	// Known means the target names something identifiable; Satisfied means it
	// is also running right now.
	Known     bool   `json:"known"`
	Satisfied bool   `json:"satisfied"`
	Detail    string `json:"detail,omitempty"`
	Ports     []int  `json:"ports,omitempty"`
}

// UnitPort pairs what a project asks for with what is actually published.
//
// Three numbers, because after a port override they genuinely differ:
// the compose file declares 8080, devcc is configured to use 8081, and until
// the container starts nothing is listening at all.
type UnitPort struct {
	// Configured is the port devcc will use — the override if one is set,
	// otherwise the declared value. Zero for a port we only observed.
	Configured int `json:"configured,omitempty"`
	// Declared is the project's own value, set only when it differs from
	// Configured because the user moved this port.
	Declared int `json:"declared,omitempty"`
	// Actual is the port in use right now. Zero when nothing is listening.
	Actual int `json:"actual,omitempty"`
	// Container is the port inside the container, for compose units.
	Container int        `json:"container,omitempty"`
	State     PortState  `json:"state"`
	Owner     *PortOwner `json:"owner,omitempty"`
}

// Unit is one runnable thing inside a project: a compose service (or one of its
// containers) or the single supervised process of a non-Docker project. The
// dashboard shows one row per unit whatever its state.
type Unit struct {
	// ID addresses this unit in the API: the compose service name, or "app".
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Kind  string    `json:"kind"` // "container" or "process"
	State UnitState `json:"state"`
	// Status is docker's human phrase ("Up 2 minutes"), when there is one.
	Status        string     `json:"status,omitempty"`
	Health        string     `json:"health,omitempty"`
	Image         string     `json:"image,omitempty"`
	ContainerName string     `json:"containerName,omitempty"`
	ContainerID   string     `json:"containerId,omitempty"`
	PID           int        `json:"pid,omitempty"`
	Running       bool       `json:"running"`
	Ports         []UnitPort `json:"ports"`
	// URL is what the Open control opens for this unit.
	URL string `json:"url,omitempty"`
	// Blocked explains why this unit cannot be started right now.
	Blocked string `json:"blocked,omitempty"`
	// Substitute is set when the user chose something else to provide this
	// unit, so it is intentionally not started.
	Substitute *Substitution `json:"substitute,omitempty"`
}

// ContainerPort is a single published port mapping on a container.
type ContainerPort struct {
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol,omitempty"`
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
	// Ports are this container's published host ports, so the dashboard can
	// show which container actually answers on which port.
	Ports []ContainerPort `json:"ports"`
	// URL is the address to open for this container: the first published TCP
	// port, or empty when it publishes nothing.
	URL string `json:"url,omitempty"`
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
	ComposeFile string `json:"composeFile,omitempty"`
	// ComposeExtraFiles are the files Compose would have loaded on its own —
	// the override sibling. They have to be passed explicitly because passing
	// any -f turns Compose's automatic discovery off.
	ComposeExtraFiles []string `json:"composeExtraFiles,omitempty"`
	ComposeProject    string   `json:"composeProject,omitempty"`

	Ports  []Port       `json:"ports"`
	URLs   []string     `json:"urls"`
	Health *HealthCheck `json:"health,omitempty"`
	// PortControl says whether this project's ports can be moved on start.
	PortControl PortControl `json:"portControl"`
	// Services are the compose services declared in the file, so the dashboard
	// can list them before the project has ever been started.
	Services []string `json:"services,omitempty"`
	// Branch is the checked-out git branch, or a short commit when detached.
	Branch string `json:"branch,omitempty"`
	// Categories are the dashboard tabs this project appears under.
	Categories []string `json:"categories"`

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
	Containers []Container `json:"containers"`
	// Units is one row per container or supervised app, listed whatever their
	// state.
	Units   []Unit       `json:"units"`
	Ports   []Port       `json:"ports"`
	Process *ProcessInfo `json:"process,omitempty"`
	Health  HealthResult `json:"health"`
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
