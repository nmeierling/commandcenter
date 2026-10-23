package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/devcc/agent/internal/model"
)

// ErrUnknownUnit is returned for a unit ID that is not part of the project.
var ErrUnknownUnit = errors.New("unknown unit")

// UnitAction starts or kills a single container or supervised app.
//
// "kill" is always explicit — it is only ever reached because the user pressed
// the button on that specific row. Starting still refuses on a port conflict
// and hands the decision back, exactly as a project-level start does.
func (m *Manager) UnitAction(ctx context.Context, projectID, unitID, action string, opts ActionOptions) (model.View, error) {
	e, err := m.entry(projectID)
	if err != nil {
		return model.View{}, err
	}

	// Scoped to this unit: resetting one row's port must not undo a choice
	// made for a sibling service.
	if err := m.applyPortChoices(projectID, unitID, opts); err != nil {
		return model.View{}, err
	}
	if e, err = m.entry(projectID); err != nil {
		return model.View{}, err
	}

	view, ok := m.View(projectID)
	if !ok {
		return model.View{}, ErrUnknownProject
	}
	unit := findUnit(view.State.Units, unitID)
	if unit == nil {
		return model.View{}, fmt.Errorf("%w: %q", ErrUnknownUnit, unitID)
	}

	m.mu.Lock()
	if e.state.Busy != "" {
		m.mu.Unlock()
		return model.View{}, ErrBusy
	}
	p := e.project
	e.state.Busy = action
	e.state.Error = ""
	if action == "start" {
		e.state.Detail = "starting " + unit.Name + "…"
		e.startedAt = time.Now()
	} else {
		e.state.Detail = "stopping " + unit.Name + "…"
	}
	busyView := normalize(model.View{Project: p, State: e.state})
	m.mu.Unlock()
	m.hub.publish(Event{Type: EventProject, Project: &busyView})

	actionErr := m.runUnitAction(ctx, e, p, *unit, action, opts)

	m.mu.Lock()
	e.state.Busy = ""
	if actionErr != nil {
		e.state.Error = actionErr.Error()
	}
	m.mu.Unlock()

	m.refresh(ctx, projectID, m.snapshot(ctx))
	out, _ := m.View(projectID)
	return out, actionErr
}

func (m *Manager) runUnitAction(ctx context.Context, e *entry, p model.Project, unit model.Unit, action string, opts ActionOptions) error {
	e.logs.Append("agent", fmt.Sprintf("--- %s %s/%s ---", action, p.Name, unit.Name))

	if action == "start" {
		// Only the ports this unit uses can block it. A conflict on some other
		// service is not this row's problem.
		if conflict := m.checkUnitConflicts(ctx, p, unit, opts); conflict != nil {
			return conflict
		}
	}

	switch p.Runtime {
	case model.RuntimeCompose:
		return m.runComposeUnitAction(ctx, e, p, unit, action)
	case model.RuntimeProcess:
		if action == "start" {
			return m.startProcess(p)
		}
		return m.stopProcess(ctx, p, 10*time.Second)
	default:
		return ErrNotControllable
	}
}

func (m *Manager) runComposeUnitAction(ctx context.Context, e *entry, p model.Project, unit model.Unit, action string) error {
	if ok, _, err := m.docker.Available(); !ok {
		return fmt.Errorf("docker compose is not available: %w", err)
	}
	if !m.daemonOK.get() {
		return errors.New("the docker daemon is not running")
	}

	dopts, err := m.composeStartOptions(p)
	if err != nil {
		return err
	}

	var out string
	switch action {
	case "start":
		e.logs.Append("agent", "$ "+m.docker.Describe(p.Path, p.ComposeFile, dopts,
			"up", "--detach", "--no-deps", unit.ID))
		out, err = m.docker.UpService(ctx, p.Path, p.ComposeFile, unit.ID, dopts)
	case "kill":
		e.logs.Append("agent", "$ "+m.docker.Describe(p.Path, p.ComposeFile, dopts,
			"stop", "--timeout", "10", unit.ID))
		out, err = m.docker.StopService(ctx, p.Path, p.ComposeFile, unit.ID, 10, dopts)
	default:
		return fmt.Errorf("unknown unit action %q", action)
	}
	appendLines(e, out)
	if err != nil {
		// A failing compose command is usually a question about which files and
		// directories it was given; answer it in the log rather than leaving the
		// user to reconstruct the invocation.
		appendLines(e, m.docker.Diagnose(ctx, p.Path, p.ComposeFile, dopts))
	}
	return err
}

// checkUnitConflicts is the project conflict check narrowed to one unit's ports.
func (m *Manager) checkUnitConflicts(ctx context.Context, p model.Project, unit model.Unit, opts ActionOptions) *PortConflictError {
	conflict := m.checkConflicts(ctx, p, opts)
	if conflict == nil {
		return nil
	}
	mine := map[int]bool{}
	for _, up := range unit.Ports {
		for _, n := range []int{up.Configured, up.Declared, up.Actual} {
			if n > 0 {
				mine[n] = true
			}
		}
	}
	filtered := conflict.Ports[:0:0]
	for _, cp := range conflict.Ports {
		if mine[cp.Port] {
			filtered = append(filtered, cp)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	conflict.Ports = filtered
	conflict.UnitID = unit.ID
	conflict.UnitName = unit.Name
	conflict.Candidates = m.SubstituteCandidates(p.ID, unit.ID)
	conflict.Blocked = true
	return conflict
}

func findUnit(units []model.Unit, id string) *model.Unit {
	for i := range units {
		if units[i].ID == id {
			return &units[i]
		}
	}
	return nil
}

func appendLines(e *entry, out string) {
	for _, line := range splitNonEmptyLines(out) {
		e.logs.Append("agent", line)
	}
}

// UnitOptions describes how a unit could be run, whether or not anything is
// currently in the way.
//
// It is the same payload a refused start returns, so the dialog that asks
// "what should happen here?" works identically when opened after a failure and
// when opened deliberately to change a decision already made.
func (m *Manager) UnitOptions(ctx context.Context, projectID, unitID string) (*PortConflictError, error) {
	view, ok := m.View(projectID)
	if !ok {
		return nil, ErrUnknownProject
	}
	unit := findUnit(view.State.Units, unitID)
	if unit == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownUnit, unitID)
	}

	snap := m.snapshot(ctx)
	claimed := m.claimedPorts()
	p := view.Project
	resolved := m.resolvePorts(&p, &model.State{}, snap)

	byPort := map[int]model.Port{}
	for _, port := range resolved {
		byPort[port.Port] = port
	}

	out := &PortConflictError{
		ProjectID:   view.ID,
		ProjectName: view.Name,
		UnitID:      unit.ID,
		UnitName:    unit.Name,
		PortControl: view.PortControl,
		Candidates:  m.SubstituteCandidates(projectID, unitID),
	}

	for _, up := range unit.Ports {
		configured := up.Configured
		if configured == 0 {
			// A port we only observed has no configured value to move.
			continue
		}
		declared := up.Declared
		if declared == 0 {
			declared = configured
		}
		// Prefer the live resolution, except where the row already knows better:
		// a port covered by a satisfied substitution is being served by exactly
		// what the user asked for, and calling that a conflict would make the
		// dialog open with the wrong question.
		state := up.State
		owner := up.Owner
		substituted := unit.Substitute != nil && unit.Substitute.Satisfied
		if live, ok := byPort[configured]; ok && !substituted {
			state, owner = live.State, live.Owner
		}
		blocked := state == model.PortConflict
		if blocked {
			out.Blocked = true
		}
		out.Ports = append(out.Ports, ConflictingPort{
			Port:         configured,
			Declared:     declared,
			Owner:        owner,
			OwnerLabel:   ownerLabel(owner),
			CanStopOwner: blocked && owner != nil && (owner.ContainerID != "" || owner.PID > 0),
			Suggested:    suggestPort(configured, snap.tcp, claimed),
			Service:      unit.ID,
		})
	}
	if len(out.Ports) == 0 {
		return nil, fmt.Errorf("%s has no configured port to change", unit.Name)
	}
	return out, nil
}
