package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/devcc/agent/internal/dockercli"
	"github.com/devcc/agent/internal/model"
)

// Substitution records that a unit is provided by something already running
// elsewhere, so this project should not start its own copy.
//
// The motivating case is a shared database: three projects each declare a
// postgres on 5432, but you only ever want one. Without this, two of them are
// permanently in conflict and the only offers are "kill the database the other
// project is using" or "start a second one on another port" — and a second
// database on another port is not the same database, so the app that connects
// to it is broken in a way the dashboard would report as healthy.
type Substitution struct {
	// Target is the raw "<project>/<unit>" string from the config.
	Target string `json:"target"`
	// Resolved fields are filled when the target is found.
	ProjectID   string `json:"projectId,omitempty"`
	ProjectName string `json:"projectName,omitempty"`
	UnitID      string `json:"unitId,omitempty"`
	// Known is true when the target names something we could identify at all.
	// Satisfied additionally requires it to be running right now — a provider
	// that is merely stopped is still a legitimate thing to have chosen.
	Known bool `json:"known"`
	// Satisfied is true when the target exists and is running right now.
	Satisfied bool `json:"satisfied"`
	// Detail explains an unsatisfied substitution.
	Detail string `json:"detail,omitempty"`
	// Ports are the host ports the provider is actually serving on.
	Ports []int `json:"ports,omitempty"`
}

// SubstituteCandidate is a running unit that could stand in for another.
type SubstituteCandidate struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	UnitID      string `json:"unitId"`
	UnitName    string `json:"unitName"`
	// Target is the value to send back, and what gets written to the config.
	Target string `json:"target"`
	Ports  []int  `json:"ports"`
	Image  string `json:"image,omitempty"`
	// Matches is true when this candidate serves the exact port that is blocked,
	// which makes it the likely answer rather than merely a possible one.
	Matches bool `json:"matches"`
	// Source is "project" for a unit devcc manages, or "docker" for a container
	// it merely observed.
	Source string `json:"source"`
	// Container is the container name, for docker-sourced candidates.
	Container string `json:"container,omitempty"`
}

// substitutesFor returns the configured substitutions for a project, keyed by
// unit ID.
func (m *Manager) substitutesFor(projectID string) map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]string{}
	for unit, target := range m.subs[projectID] {
		out[unit] = target
	}
	return out
}

// resolveSubstitution looks up a "<project>/<unit>" target and reports whether
// it is currently running.
func (m *Manager) resolveSubstitution(target string) Substitution {
	if strings.HasPrefix(target, DockerPrefix) {
		return m.resolveDockerSubstitution(target)
	}
	sub := Substitution{Target: target}
	projectRef, unitID, ok := strings.Cut(target, "/")
	if !ok || projectRef == "" || unitID == "" {
		sub.Detail = fmt.Sprintf("%q is not a valid target; use \"<project>/<unit>\"", target)
		return sub
	}

	view, found := m.findProjectByRef(projectRef)
	if !found {
		sub.Detail = fmt.Sprintf("no project called %q", projectRef)
		return sub
	}
	sub.ProjectID = view.ID
	sub.ProjectName = view.Name
	sub.UnitID = unitID

	unit := findUnit(view.State.Units, unitID)
	if unit == nil {
		sub.Detail = fmt.Sprintf("%s has no unit called %q", view.Name, unitID)
		return sub
	}
	sub.Known = true
	if !unit.Running {
		sub.Detail = fmt.Sprintf("%s/%s is not running", view.Name, unitID)
		return sub
	}
	for _, p := range unit.Ports {
		if p.Actual > 0 {
			sub.Ports = append(sub.Ports, p.Actual)
		}
	}
	sub.Satisfied = true
	return sub
}

// findProjectByRef matches a project by name first, because that is what a
// human writes in the config file, then by ID for stability.
func (m *Manager) findProjectByRef(ref string) (model.View, bool) {
	views := m.Views()
	for _, v := range views {
		if strings.EqualFold(v.Name, ref) {
			return v, true
		}
	}
	for _, v := range views {
		if v.ID == ref {
			return v, true
		}
	}
	return model.View{}, false
}

// SubstituteCandidates lists running units elsewhere that could satisfy a unit.
//
// Ordering matters more than completeness here: whatever is already serving the
// blocked port is almost always the intended answer, so it comes first.
func (m *Manager) SubstituteCandidates(projectID, unitID string) []SubstituteCandidate {
	view, ok := m.View(projectID)
	if !ok {
		return nil
	}
	unit := findUnit(view.State.Units, unitID)
	if unit == nil {
		return nil
	}

	wantedHost := map[int]bool{}
	wantedContainer := map[int]bool{}
	for _, p := range unit.Ports {
		if p.Configured > 0 {
			wantedHost[p.Configured] = true
		}
		if p.Container > 0 {
			wantedContainer[p.Container] = true
		}
	}

	var out []SubstituteCandidate
	claimed := map[string]bool{}
	// Our own containers are never substitutes for ourselves.
	if view.ComposeProject != "" {
		for _, u := range view.State.Units {
			claimed[view.ComposeProject+"/"+u.ID] = true
		}
	}
	for _, other := range m.Views() {
		if other.ID == projectID {
			continue
		}
		for _, ou := range other.State.Units {
			if !ou.Running {
				continue
			}
			var serving []int
			matches := false
			for _, p := range ou.Ports {
				if p.Actual == 0 {
					continue
				}
				serving = append(serving, p.Actual)
				if wantedHost[p.Actual] || (p.Container > 0 && wantedContainer[p.Container]) {
					matches = true
				}
			}
			if len(serving) == 0 {
				// Something with no published port cannot stand in for a
				// dependency another project needs to reach.
				continue
			}
			out = append(out, SubstituteCandidate{
				ProjectID:   other.ID,
				ProjectName: other.Name,
				UnitID:      ou.ID,
				UnitName:    ou.Name,
				Target:      other.Name + "/" + ou.ID,
				Ports:       serving,
				Image:       ou.Image,
				Matches:     matches,
				Source:      "project",
			})
			// Remember which compose service this unit is, so the same
			// container is not offered again as a raw docker candidate.
			if other.ComposeProject != "" {
				claimed[other.ComposeProject+"/"+ou.ID] = true
			}
		}
	}
	out = append(out, m.dockerCandidates(wantedHost, wantedContainer, claimed)...)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Matches != out[j].Matches {
			return out[i].Matches
		}
		// Projects before loose containers: a unit devcc manages is the more
		// meaningful choice when both would do.
		if out[i].Source != out[j].Source {
			return out[i].Source == "project"
		}
		if out[i].ProjectName != out[j].ProjectName {
			return out[i].ProjectName < out[j].ProjectName
		}
		return out[i].UnitName < out[j].UnitName
	})
	return out
}

// SetSubstitute records or clears a substitution and persists it to the config
// file, so the choice survives a restart and is visible to whoever reads the
// repository.
func (m *Manager) SetSubstitute(ctx context.Context, projectID, unitID, target string) (model.View, error) {
	view, ok := m.View(projectID)
	if !ok {
		return model.View{}, ErrUnknownProject
	}
	if findUnit(view.State.Units, unitID) == nil {
		return model.View{}, fmt.Errorf("%w: %q", ErrUnknownUnit, unitID)
	}
	if target != "" {
		// Reject only what we cannot identify at all. A provider that exists but
		// happens to be stopped is a perfectly reasonable thing to record; the
		// row will say it is unavailable until it comes back.
		if sub := m.resolveSubstitution(target); !sub.Known {
			detail := sub.Detail
			if detail == "" {
				detail = "not found"
			}
			return model.View{}, fmt.Errorf("cannot use %q: %s", target, detail)
		}
	}

	if err := m.writer().SetSubstitute(view.Path, view.Name, unitID, target); err != nil {
		return model.View{}, fmt.Errorf("save to config: %w", err)
	}
	if err := m.reloadConfig(); err != nil {
		return model.View{}, err
	}
	m.refresh(ctx, projectID, m.snapshot(ctx))
	out, _ := m.View(projectID)
	return out, nil
}

// substitutedUnits lists the unit IDs a project should not start itself.
func (m *Manager) substitutedUnits(projectID string) map[string]bool {
	out := map[string]bool{}
	for unit := range m.substitutesFor(projectID) {
		out[unit] = true
	}
	return out
}

// decorateUnits marks the units a substitution covers, and folds the provider's
// state into the row so it reads as "handled" rather than "broken".
func (m *Manager) decorateUnits(units []model.Unit, projectID string) []model.Unit {
	subs := m.substitutesFor(projectID)
	if len(subs) == 0 {
		return units
	}
	for i := range units {
		target, ok := subs[units[i].ID]
		if !ok {
			continue
		}
		resolved := m.resolveSubstitution(target)
		units[i].Substitute = &model.Substitution{
			Target:      resolved.Target,
			ProjectID:   resolved.ProjectID,
			ProjectName: resolved.ProjectName,
			UnitID:      resolved.UnitID,
			Known:       resolved.Known,
			Satisfied:   resolved.Satisfied,
			Detail:      resolved.Detail,
			Ports:       resolved.Ports,
		}
		if resolved.Satisfied {
			// A satisfied unit is deliberately not running, which is a
			// different thing from failing to run.
			units[i].State = model.UnitSubstituted
			// Its own ports are not in conflict; something we chose is serving
			// them, and that is the desired state.
			for j := range units[i].Ports {
				if units[i].Ports[j].State == model.PortConflict {
					units[i].Ports[j].State = model.PortSubstituted
				}
			}
		}
	}
	return units
}

// substitutedPorts lists the ports covered by a satisfied substitution.
func (m *Manager) substitutedPorts(projectID string) map[int]bool {
	out := map[int]bool{}
	view, ok := m.View(projectID)
	if !ok {
		return out
	}
	for _, u := range view.State.Units {
		if u.Substitute == nil || !u.Substitute.Satisfied {
			continue
		}
		for _, p := range u.Ports {
			if p.Configured > 0 {
				out[p.Configured] = true
			}
		}
	}
	return out
}

// startableServices lists the compose services a project-wide start should
// bring up: everything except the ones something else already provides.
//
// An empty result means "no restriction", which is what compose does with no
// service arguments — correct when nothing is substituted.
func (m *Manager) startableServices(p model.Project) []string {
	subs := m.substitutedUnits(p.ID)
	if len(subs) == 0 {
		return nil
	}
	var out []string
	for _, svc := range p.Services {
		if !subs[svc] {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out
}

// applySubstitutedPorts folds unit-level substitutions back into the project's
// port list. Without this the card would still read "conflict" for a port that
// is being served by exactly the thing the user chose.
func applySubstitutedPorts(st *model.State) {
	covered := map[int]bool{}
	for _, u := range st.Units {
		if u.Substitute == nil || !u.Substitute.Satisfied {
			continue
		}
		for _, p := range u.Ports {
			if p.Configured > 0 {
				covered[p.Configured] = true
			}
		}
	}
	if len(covered) == 0 {
		return
	}
	for i := range st.Ports {
		if covered[st.Ports[i].Port] && st.Ports[i].State == model.PortConflict {
			st.Ports[i].State = model.PortSubstituted
		}
	}
}

// DockerPrefix namespaces substitute targets that name a raw container rather
// than a project devcc manages. A project called "docker" cannot shadow it,
// because this prefix is checked first.
const DockerPrefix = "docker/"

func (m *Manager) setContainers(cs []dockercli.RunningContainer) {
	m.containersMu.Lock()
	m.containers = cs
	m.containersMu.Unlock()
}

func (m *Manager) knownContainers() []dockercli.RunningContainer {
	m.containersMu.RLock()
	defer m.containersMu.RUnlock()
	return append([]dockercli.RunningContainer(nil), m.containers...)
}

// resolveDockerSubstitution looks up a bare container by name.
func (m *Manager) resolveDockerSubstitution(target string) Substitution {
	name := strings.TrimPrefix(target, DockerPrefix)
	sub := Substitution{Target: target, ProjectName: "docker", UnitID: name}
	if name == "" {
		sub.Detail = "no container name given"
		return sub
	}
	for _, rc := range m.knownContainers() {
		if rc.Name != name {
			continue
		}
		for _, p := range rc.Ports {
			sub.Ports = append(sub.Ports, p.Host)
		}
		sub.Known = true
		sub.Satisfied = true
		return sub
	}
	// Containers keep their name across stop/start, so a missing one really is
	// gone (or was recreated with a generated name).
	sub.Detail = fmt.Sprintf("no running container called %q", name)
	return sub
}

// dockerCandidates offers running containers that no known project accounts
// for: a hand-started `docker run postgres`, or a compose stack outside the
// scanned roots.
func (m *Manager) dockerCandidates(wantedHost, wantedContainer map[int]bool, claimed map[string]bool) []SubstituteCandidate {
	var out []SubstituteCandidate
	for _, rc := range m.knownContainers() {
		if rc.Name == "" || len(rc.Ports) == 0 {
			continue
		}
		// Skip anything already offered as a project unit, so the same database
		// does not appear twice under two different names.
		if claimed[rc.ComposeProject+"/"+rc.ComposeService] {
			continue
		}
		var serving []int
		matches := false
		for _, p := range rc.Ports {
			serving = append(serving, p.Host)
			if wantedHost[p.Host] || wantedContainer[p.Container] {
				matches = true
			}
		}
		name := rc.ComposeService
		if name == "" {
			name = rc.Name
		}
		out = append(out, SubstituteCandidate{
			ProjectName: "docker",
			UnitID:      rc.Name,
			UnitName:    name,
			Target:      DockerPrefix + rc.Name,
			Ports:       serving,
			Image:       rc.Image,
			Matches:     matches,
			Source:      "docker",
			Container:   rc.Name,
		})
	}
	return out
}
