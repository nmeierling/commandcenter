package core

import (
	"sort"
	"strconv"

	"github.com/devcc/agent/internal/model"
)

// AppUnitID is the unit ID for a supervised project's single process.
const AppUnitID = "app"

// buildUnits produces one row per container or supervised app.
//
// The list is driven by what the project *declares*, not by what happens to be
// running: a compose project shows all four of its services before it has ever
// been started, each with the port it is configured to use. Runtime facts are
// then layered on top, so a row can say "configured 3000, actually on 3100".
func buildUnits(p *model.Project, st *model.State, resolved []model.Port) []model.Unit {
	switch p.Runtime {
	case model.RuntimeCompose:
		return composeUnits(p, st, resolved)
	case model.RuntimeProcess:
		return []model.Unit{processUnit(p, st, resolved)}
	default:
		return []model.Unit{observedUnit(p, st, resolved)}
	}
}

func composeUnits(p *model.Project, st *model.State, resolved []model.Port) []model.Unit {
	// Containers grouped by the service they belong to. A scaled service has
	// several, and each gets its own row.
	byService := map[string][]model.Container{}
	for _, c := range st.Containers {
		key := c.Service
		if key == "" {
			key = c.Name
		}
		byService[key] = append(byService[key], c)
	}

	// Every declared service, plus any container whose service is not in the
	// file (left over from an older revision — worth seeing, not hiding).
	names := append([]string{}, p.Services...)
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for svc := range byService {
		if !seen[svc] {
			names = append(names, svc)
			seen[svc] = true
		}
	}
	sort.Strings(names)

	var units []model.Unit
	for _, svc := range names {
		containers := byService[svc]
		if len(containers) == 0 {
			// Declared but not created yet: show it with its configured ports.
			units = append(units, model.Unit{
				ID:      svc,
				Name:    svc,
				Kind:    "container",
				State:   notCreatedState(st),
				Ports:   configuredPortsFor(svc, resolved),
				Blocked: composeBlockedReason(st),
			})
			continue
		}
		for _, c := range containers {
			units = append(units, containerUnit(svc, c, resolved))
		}
	}
	return units
}

// notCreatedState distinguishes "declared but never started" from "we have no
// idea", which is what a stopped Docker daemon leaves us with.
func notCreatedState(st *model.State) model.UnitState {
	if st.Status == model.StatusUnknown {
		return model.UnitUnknown
	}
	return model.UnitNotCreated
}

func composeBlockedReason(st *model.State) string {
	if st.Status == model.StatusUnknown && st.Detail != "" {
		return st.Detail
	}
	return ""
}

func containerUnit(service string, c model.Container, resolved []model.Port) model.Unit {
	running := c.State == "running"
	u := model.Unit{
		ID:            service,
		Name:          service,
		Kind:          "container",
		State:         unitStateFrom(c.State),
		Status:        c.Status,
		Health:        c.Health,
		Image:         c.Image,
		ContainerName: c.Name,
		ContainerID:   c.ID,
		Running:       running,
	}

	// Actual mappings from the live container, matched back to the configured
	// port so the row can show both numbers.
	actualByContainerPort := map[int]int{}
	for _, cp := range c.Ports {
		actualByContainerPort[cp.Container] = cp.Host
	}

	configured := configuredPortsFor(service, resolved)
	used := map[int]bool{}
	for i := range configured {
		if actual, ok := actualByContainerPort[configured[i].Container]; ok {
			configured[i].Actual = actual
			used[actual] = true
		} else if running {
			// Running but not publishing what we expected.
			configured[i].Actual = 0
		}
	}
	// Ports the container publishes that the file never declared.
	for _, cp := range c.Ports {
		if used[cp.Host] || hasConfigured(configured, cp.Host) {
			continue
		}
		configured = append(configured, model.UnitPort{
			Actual:    cp.Host,
			Container: cp.Container,
			State:     portStateFor(cp.Host, resolved),
			Owner:     portOwnerFor(cp.Host, resolved),
		})
	}
	sort.Slice(configured, func(i, j int) bool {
		return unitPortKey(configured[i]) < unitPortKey(configured[j])
	})
	u.Ports = configured
	u.URL = firstOpenableURL(u.Ports, running)
	return u
}

func processUnit(p *model.Project, st *model.State, resolved []model.Port) model.Unit {
	running := st.Process != nil && st.Process.Running
	u := model.Unit{
		ID:      AppUnitID,
		Name:    p.Name,
		Kind:    "process",
		State:   processUnitState(st),
		Running: running,
		Ports:   allConfiguredPorts(resolved),
	}
	if st.Process != nil {
		u.PID = st.Process.PID
		u.Image = st.Process.Command
		if running {
			u.Status = "pid " + strconv.Itoa(st.Process.PID)
		} else if st.Process.ExitCode != nil {
			u.Status = "exited with code " + strconv.Itoa(*st.Process.ExitCode)
		}
	}
	if p.StartCommand == "" {
		u.Blocked = "no start command configured"
	}
	u.URL = firstOpenableURL(u.Ports, running)
	return u
}

func processUnitState(st *model.State) model.UnitState {
	switch {
	case st.Process == nil:
		return model.UnitNotCreated
	case st.Process.Running:
		return model.UnitRunning
	default:
		return model.UnitExited
	}
}

// observedUnit represents a project we can watch but not control.
func observedUnit(p *model.Project, st *model.State, resolved []model.Port) model.Unit {
	ports := allConfiguredPorts(resolved)
	running := false
	for _, up := range ports {
		if up.State == model.PortOwned || up.State == model.PortUnknownOwner {
			running = true
		}
	}
	return model.Unit{
		ID:      AppUnitID,
		Name:    p.Name,
		Kind:    "process",
		State:   observedState(running),
		Running: running,
		Ports:   ports,
		Blocked: "no start command configured for this project",
		URL:     firstOpenableURL(ports, running),
	}
}

func observedState(running bool) model.UnitState {
	if running {
		return model.UnitRunning
	}
	return model.UnitNotCreated
}

func unitStateFrom(dockerState string) model.UnitState {
	switch dockerState {
	case "running", "restarting":
		return model.UnitRunning
	case "created", "paused":
		return model.UnitCreated
	case "":
		return model.UnitUnknown
	default:
		return model.UnitExited
	}
}

func configuredPortsFor(service string, resolved []model.Port) []model.UnitPort {
	out := []model.UnitPort{}
	for _, p := range resolved {
		if p.Service != service {
			continue
		}
		out = append(out, unitPortFrom(p))
	}
	return out
}

func allConfiguredPorts(resolved []model.Port) []model.UnitPort {
	out := []model.UnitPort{}
	for _, p := range resolved {
		out = append(out, unitPortFrom(p))
	}
	return out
}

func unitPortFrom(p model.Port) model.UnitPort {
	// p.Port is already the effective port: discovery applies any override
	// before anything reads the list, and p.Declared holds the original.
	up := model.UnitPort{
		Configured: p.Port,
		Declared:   p.Declared,
		Container:  p.ContainerPort,
		State:      p.State,
		Owner:      p.Owner,
	}
	// A port we only observed at runtime was never configured.
	if p.Source == "observed" {
		up.Configured = 0
		up.Actual = p.Port
	}
	if p.State == model.PortOwned {
		up.Actual = p.Port
	}
	return up
}

func hasConfigured(ports []model.UnitPort, host int) bool {
	for _, p := range ports {
		if p.Configured == host || p.Actual == host {
			return true
		}
	}
	return false
}

func portStateFor(host int, resolved []model.Port) model.PortState {
	for _, p := range resolved {
		if p.Port == host {
			return p.State
		}
	}
	return model.PortOwned
}

func portOwnerFor(host int, resolved []model.Port) *model.PortOwner {
	for _, p := range resolved {
		if p.Port == host {
			return p.Owner
		}
	}
	return nil
}

func unitPortKey(p model.UnitPort) int {
	if p.Configured > 0 {
		return p.Configured
	}
	return p.Actual
}

// firstOpenableURL picks the address the Open control uses for a unit: the
// lowest port it is actually serving on.
func firstOpenableURL(ports []model.UnitPort, running bool) string {
	if !running {
		return ""
	}
	best := 0
	for _, p := range ports {
		port := p.Actual
		if port == 0 || p.State == model.PortConflict {
			continue
		}
		if best == 0 || port < best {
			best = port
		}
	}
	if best == 0 {
		return ""
	}
	return "http://localhost:" + strconv.Itoa(best)
}
