package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devcc/agent/internal/dockercli"
	"github.com/devcc/agent/internal/model"
	"github.com/devcc/agent/internal/ports"
	"github.com/devcc/agent/internal/procmgr"
)

// ActionOptions carry the decisions a user made in response to a conflict.
//
// Both fields default to false/nil, which is the point: without an explicit
// instruction the agent will not stop anything and will not move any ports. It
// reports the conflict and waits.
type ActionOptions struct {
	// StopOwners authorises stopping the specific processes or containers named
	// in the conflict the user was shown. It is never inferred.
	StopOwners bool
	// PortOverrides maps a declared host port to the one to use instead.
	PortOverrides map[int]int
	// ClearOverrides returns the project to the ports it declares.
	ClearOverrides bool
}

// ConflictingPort describes one blocked port and what can be done about it.
type ConflictingPort struct {
	Port int `json:"port"`
	// Declared is the key an override is stored under — the port the project
	// itself declares. Moving an already-moved port has to update that entry
	// rather than add a second one keyed by the current value.
	Declared int              `json:"declared"`
	Owner    *model.PortOwner `json:"owner,omitempty"`
	// OwnerLabel is a human phrase for the owner, e.g. `container shop-web-1`.
	OwnerLabel string `json:"ownerLabel"`
	// CanStopOwner is false when we could not identify the owner well enough to
	// stop precisely the right thing.
	CanStopOwner bool `json:"canStopOwner"`
	// Suggested is a free port we propose instead, or 0 if none was found.
	Suggested int `json:"suggested,omitempty"`
	// Service is the compose service publishing this port, when known.
	Service string `json:"service,omitempty"`
}

// PortConflictError is returned instead of starting when ports are taken. The
// API turns it into a 409 the dashboard renders as a dialog.
type PortConflictError struct {
	ProjectID   string            `json:"projectId"`
	ProjectName string            `json:"projectName"`
	Ports       []ConflictingPort `json:"ports"`
	PortControl model.PortControl `json:"portControl"`
	// UnitID/UnitName are set when the conflict blocked a single row rather
	// than the whole project.
	UnitID   string `json:"unitId,omitempty"`
	UnitName string `json:"unitName,omitempty"`
	// Candidates are running units elsewhere that could provide this one, so
	// the dialog can offer "use the database that is already up" without a
	// second round trip.
	Candidates []SubstituteCandidate `json:"candidates,omitempty"`
	// Blocked distinguishes "this start was refused" from "here are the options
	// for a unit that is running fine" — the same payload drives both, and the
	// dialog has to say different things.
	Blocked bool `json:"blocked"`
}

func (e *PortConflictError) Error() string {
	parts := make([]string, 0, len(e.Ports))
	for _, p := range e.Ports {
		parts = append(parts, fmt.Sprintf("port %d is in use by %s", p.Port, p.OwnerLabel))
	}
	return strings.Join(parts, "; ")
}

// checkConflicts returns a conflict error if any of the project's ports are
// held by something else, having first applied any requested overrides.
func (m *Manager) checkConflicts(ctx context.Context, p model.Project, opts ActionOptions) *PortConflictError {
	// Resolve against a snapshot taken right now rather than the last cached
	// refresh. The cached state can be seconds old, and it is definitely stale
	// immediately after a port override changed which ports we even care about.
	snap := m.snapshot(ctx)
	claimed := m.claimedPorts()
	resolved := m.resolvePorts(&p, &model.State{}, snap)
	// Ports belonging to a satisfied substitution are being served by exactly
	// what the user asked for, so they are not in the way of anything.
	substituted := m.substitutedPorts(p.ID)

	var conflicts []ConflictingPort
	for _, port := range resolved {
		if port.State != model.PortConflict || substituted[port.Port] {
			continue
		}
		// A port the user just chose to move is no longer blocking.
		if _, moved := opts.PortOverrides[declaredPort(port)]; moved {
			continue
		}
		owner := port.Owner
		conflicts = append(conflicts, ConflictingPort{
			Port:         port.Port,
			Declared:     declaredPort(port),
			Owner:        owner,
			OwnerLabel:   ownerLabel(owner),
			CanStopOwner: owner != nil && (owner.ContainerID != "" || owner.PID > 0),
			Suggested:    suggestPort(port.Port, snap.tcp, claimed),
			Service:      port.Service,
		})
	}
	if len(conflicts) == 0 {
		return nil
	}
	return &PortConflictError{
		ProjectID:   p.ID,
		ProjectName: p.Name,
		Ports:       conflicts,
		PortControl: p.PortControl,
	}
}

func ownerLabel(o *model.PortOwner) string {
	switch {
	case o == nil:
		return "an unidentified process"
	case o.ComposeProject != "":
		return fmt.Sprintf("container %s (compose project %q)", o.Container, o.ComposeProject)
	case o.Container != "":
		return fmt.Sprintf("container %s", o.Container)
	case o.Process != "":
		return fmt.Sprintf("%s (pid %d)", o.Process, o.PID)
	default:
		return fmt.Sprintf("pid %d", o.PID)
	}
}

// stopConflictOwners stops exactly the owners the user was shown and approved.
// It is only ever reached when ActionOptions.StopOwners was explicitly set.
func (m *Manager) stopConflictOwners(ctx context.Context, p model.Project, conflict *PortConflictError) error {
	for _, c := range conflict.Ports {
		if !c.CanStopOwner {
			return fmt.Errorf(
				"cannot free port %d: its owner could not be identified, so there is nothing safe to stop", c.Port)
		}
		msg, err := m.KillPortOwner(ctx, p.ID, c.Port, false)
		if err != nil {
			return fmt.Errorf("freeing port %d: %w", c.Port, err)
		}
		if buf := m.logBuffer(p.ID); buf != nil {
			buf.Append("agent", "[user approved] "+msg)
		}
	}
	// Give the OS a moment to release the sockets before we bind them.
	waitForPortsFree(conflict.Ports, 5)
	return nil
}

// waitForPortsFree gives the kernel a moment to release sockets after the
// owners were stopped; binding immediately usually fails with EADDRINUSE.
func waitForPortsFree(conflicts []ConflictingPort, attempts int) {
	for i := 0; i < attempts; i++ {
		allFree := true
		for _, c := range conflicts {
			if !ports.IsFree(c.Port) {
				allFree = false
			}
		}
		if allFree {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// --- applying overrides ----------------------------------------------------

// composeStartOptions turns a project's port overrides into the environment and
// extra compose files that will actually move the ports.
func (m *Manager) composeStartOptions(p model.Project) (dockercli.Options, error) {
	return m.composeOptions(p, true)
}

// composeReadOptions builds the same options for read-only commands (ps, logs)
// without generating anything. Every compose invocation for a project has to
// resolve the same configuration, or `ps` would report the port the file
// declares while the containers actually run on the one the user chose.
func (m *Manager) composeReadOptions(p model.Project) dockercli.Options {
	opts, err := m.composeOptions(p, false)
	if err != nil {
		return dockercli.Options{}
	}
	return opts
}

func (m *Manager) composeOptions(p model.Project, generate bool) (dockercli.Options, error) {
	// The project's own override sibling comes first, so anything the agent
	// generates still wins, and every command — ps and logs included — sees the
	// same project definition the developer would get by hand.
	base := append([]string{}, p.ComposeExtraFiles...)

	overrides := m.portOverridesFor(p.ID)
	if len(overrides) == 0 {
		return dockercli.Options{ExtraFiles: base}, nil
	}

	// Preferred mechanism: the compose file already declares the port through a
	// variable, so setting it is exactly what the author intended.
	var env []string
	remaining := map[int]int{}
	for declared, chosen := range overrides {
		variable := ""
		for _, port := range p.Ports {
			if declaredPort(port) == declared && port.Variable != "" {
				variable = port.Variable
				break
			}
		}
		if variable != "" {
			env = append(env, fmt.Sprintf("%s=%d", variable, chosen))
			continue
		}
		remaining[declared] = chosen
	}
	sort.Strings(env)

	if len(remaining) == 0 {
		return dockercli.Options{Env: env, ExtraFiles: base}, nil
	}

	// Fallback: an agent-owned override file using `!override`. A plain
	// override would append a second published port and leave the original
	// conflict untouched, so refuse when the tag is unsupported.
	if !generate {
		// Reuse a previously generated override file if there is one; never
		// write during a status refresh.
		path := filepath.Join(m.stateDir, "compose-overrides", p.ID+".yaml")
		if _, err := os.Stat(path); err == nil {
			return dockercli.Options{Env: env, ExtraFiles: append(base, path)}, nil
		}
		return dockercli.Options{Env: env, ExtraFiles: base}, nil
	}
	if !m.docker.SupportsOverrideTag() {
		_, version, _ := m.docker.Available()
		return dockercli.Options{}, fmt.Errorf(
			"cannot move a hard-coded compose port with Docker Compose %s: it needs the `!override` tag from v2.24. "+
				"Declare the port as a variable instead, e.g. \"${PORT:-%d}:%d\"",
			version, firstKey(remaining), firstKey(remaining))
	}
	path, err := m.writeComposeOverride(p, overrides)
	if err != nil {
		return dockercli.Options{}, err
	}
	return dockercli.Options{Env: env, ExtraFiles: append(base, path)}, nil
}

func firstKey(m map[int]int) int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	if len(keys) == 0 {
		return 0
	}
	return keys[0]
}

var portFlagRe = regexp.MustCompile(`(--port[= ]|(?:^|\s)-p[= ])(\d{2,5})\b`)

// portFlagIn reports an explicit --port/-p value in a start command.
func portFlagIn(command string) (int, bool) {
	m := portFlagRe.FindStringSubmatch(command)
	if m == nil {
		return 0, false
	}
	p, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, false
	}
	return p, true
}

// processStartSpec applies port overrides to a supervised project's command and
// environment.
//
// Both levers are used together on purpose. PORT is what most Node and Python
// frameworks read, but Vite and friends only honour --port, and a hand-written
// server may ignore both. Setting the flag when one is already there is the
// reliable path; PORT is the best-effort complement.
func (m *Manager) processStartSpec(p model.Project) procmgr.Spec {
	spec := procmgr.Spec{ID: p.ID, Dir: p.Path, Command: p.StartCommand, Env: p.Env}

	overrides := m.portOverridesFor(p.ID)
	if len(overrides) == 0 {
		return spec
	}
	// A supervised project has one primary port; take the lowest declared one.
	declared := make([]int, 0, len(overrides))
	for d := range overrides {
		declared = append(declared, d)
	}
	sort.Ints(declared)
	chosen := overrides[declared[0]]

	env := map[string]string{}
	for k, v := range p.Env {
		env[k] = v
	}
	env["PORT"] = strconv.Itoa(chosen)
	spec.Env = env

	if _, ok := portFlagIn(p.StartCommand); ok {
		spec.Command = portFlagRe.ReplaceAllString(p.StartCommand, "${1}"+strconv.Itoa(chosen))
	}
	return spec
}

// applyOverridesToPorts rewrites a project's expected ports to the ones the user
// chose, keeping the declared value so the UI can show "3100 (was 3000)".
func applyOverridesToPorts(p *model.Project, overrides map[int]int) {
	if len(overrides) == 0 {
		return
	}
	for i := range p.Ports {
		declared := p.Ports[i].Port
		chosen, ok := overrides[declared]
		if !ok || chosen == declared {
			continue
		}
		p.Ports[i].Declared = declared
		p.Ports[i].Port = chosen
	}
	sort.Slice(p.Ports, func(i, j int) bool { return p.Ports[i].Port < p.Ports[j].Port })

	// URLs derived from the old port would now point at nothing.
	for i, u := range p.URLs {
		for declared, chosen := range overrides {
			old := ":" + strconv.Itoa(declared)
			if strings.Contains(u, old) {
				p.URLs[i] = strings.Replace(u, old, ":"+strconv.Itoa(chosen), 1)
			}
		}
	}
}
