package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/devcc/agent/internal/config"
	"github.com/devcc/agent/internal/dockercli"
	"github.com/devcc/agent/internal/model"
	"github.com/devcc/agent/internal/ports"
)

// portOverridesFor returns the ports the user moved a project onto.
func (m *Manager) portOverridesFor(projectID string) map[int]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if src, ok := m.portOverrides[projectID]; ok && len(src) > 0 {
		out := make(map[int]int, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	return nil
}

// writer edits the config file lifecycle choices are persisted to.
func (m *Manager) writer() *config.Writer {
	path := m.configPath
	if path == "" {
		path = config.DefaultPath()
	}
	return &config.Writer{Path: path}
}

// reloadConfig re-reads the config file after the agent has written to it, so
// the in-memory view matches what is now on disk.
func (m *Manager) reloadConfig() error {
	path := m.configPath
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}
	m.mu.Lock()
	// Only the per-project entries can change under us; scan and poll settings
	// are the user's and are not rewritten by the agent.
	m.cfg.Projects = cfg.Projects
	m.mu.Unlock()
	m.Discover()
	return nil
}

// SetPortOverrides records chosen ports in the config file.
func (m *Manager) SetPortOverrides(projectPath, projectName string, overrides map[int]int) error {
	w := m.writer()
	declared := make([]int, 0, len(overrides))
	for d := range overrides {
		declared = append(declared, d)
	}
	sort.Ints(declared)
	for _, d := range declared {
		if err := w.SetPortOverride(projectPath, projectName, d, overrides[d]); err != nil {
			return err
		}
	}
	return m.reloadConfig()
}

// ClearPortOverrides removes port overrides, optionally only the listed ones.
func (m *Manager) ClearPortOverrides(projectPath, projectName string, only []int) error {
	if err := m.writer().ClearPortOverrides(projectPath, projectName, only); err != nil {
		return err
	}
	return m.reloadConfig()
}

// --- port control ----------------------------------------------------------

// derivePortControl decides whether a project's host ports can be moved, and by
// what mechanism. Being explicit about this matters: offering "start on another
// port" for a project where it silently would not take effect is worse than not
// offering it, because the user would be told the app is on 3100 while it is
// really on 3000 or not running at all.
func derivePortControl(p *model.Project, docker *dockercli.Client) model.PortControl {
	switch p.Runtime {
	case model.RuntimeProcess:
		if hasPortFlag(p.StartCommand) {
			return model.PortControl{CanChange: true, Mechanism: model.PortChangeCommand}
		}
		// Most dev servers read PORT, but not all: Vite needs --port, and a
		// hand-written Go server may hard-code its address. We can set PORT and
		// then check which port the process actually opened.
		return model.PortControl{CanChange: true, Mechanism: model.PortChangeEnv}

	case model.RuntimeCompose:
		for _, port := range p.Ports {
			if port.Variable != "" {
				return model.PortControl{CanChange: true, Mechanism: model.PortChangeComposeVar}
			}
		}
		// A hard-coded "3000:80" cannot be moved by setting anything. An extra
		// compose file would normally *append* a second published port rather
		// than replace the first, leaving the conflict in place — only the
		// `!override` tag (compose v2.24+) actually replaces it.
		if docker != nil && docker.SupportsOverrideTag() {
			return model.PortControl{CanChange: true, Mechanism: model.PortChangeComposeOverride}
		}
		_, version, _ := docker.Available()
		return model.PortControl{
			CanChange: false,
			Mechanism: model.PortChangeNone,
			Reason: fmt.Sprintf(
				"the port is hard-coded in the compose file and Docker Compose %s cannot replace it. "+
					"Either upgrade to Compose v2.24+, or declare the port as a variable, e.g. \"${PORT:-3000}:80\".",
				version),
		}
	}
	return model.PortControl{
		CanChange: false,
		Mechanism: model.PortChangeNone,
		Reason:    "this project has no start mechanism, so there is nothing to move",
	}
}

func hasPortFlag(command string) bool {
	_, ok := portFlagIn(command)
	return ok
}

// suggestPort finds a free port at or above base, skipping ports that any known
// project already expects. Handing out a port another project will want is just
// deferring the same conflict.
func suggestPort(base int, snap *ports.Snapshot, claimed map[int]bool) int {
	if base < 1024 {
		base = 1024
	}
	for candidate := base + 1; candidate <= 65535 && candidate < base+400; candidate++ {
		if claimed[candidate] {
			continue
		}
		if snap != nil && snap.Listening(candidate) {
			continue
		}
		if !ports.IsFree(candidate) {
			continue
		}
		return candidate
	}
	return 0
}

// claimedPorts collects every port any known project expects, so a suggestion
// does not collide with a project that simply is not running right now.
func (m *Manager) claimedPorts() map[int]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	claimed := map[int]bool{}
	for _, e := range m.byID {
		for _, p := range e.project.Ports {
			claimed[p.Port] = true
			if p.Declared > 0 {
				claimed[p.Declared] = true
			}
		}
	}
	return claimed
}

// --- compose override file -------------------------------------------------

// writeComposeOverride generates an agent-owned compose file that republishes
// the given services on new host ports.
//
// It lives in the agent's state directory, never inside the project: the whole
// point is to move a port without editing a file the user has in git. The
// `!override` tag is essential — without it compose concatenates the port lists
// and publishes both the old and the new port.
func (m *Manager) writeComposeOverride(p model.Project, mapping map[int]int) (string, error) {
	// Group the new mappings by service, keeping the container-side port.
	byService := map[string][]string{}
	for _, port := range p.Ports {
		chosen, ok := mapping[declaredPort(port)]
		if !ok {
			chosen = port.Port
		}
		svc := port.Service
		if svc == "" {
			return "", fmt.Errorf("cannot move port %d: the compose service that publishes it is unknown", port.Port)
		}
		byService[svc] = append(byService[svc], fmt.Sprintf("%d:%d", chosen, containerSideOf(p, port)))
	}
	if len(byService) == 0 {
		return "", fmt.Errorf("no published ports to override")
	}

	services := make([]string, 0, len(byService))
	for svc := range byService {
		services = append(services, svc)
	}
	sort.Strings(services)

	var b []byte
	b = append(b, "# Generated by devcc. Do not edit; it is rewritten on every start.\n"...)
	b = append(b, "# It republishes the ports you chose, using !override so compose replaces\n"...)
	b = append(b, "# the declared ports instead of appending to them.\n"...)
	b = append(b, "services:\n"...)
	for _, svc := range services {
		b = append(b, fmt.Sprintf("  %s:\n    ports: !override\n", svc)...)
		entries := byService[svc]
		sort.Strings(entries)
		for _, e := range entries {
			b = append(b, fmt.Sprintf("      - \"%s\"\n", e)...)
		}
	}

	dir := filepath.Join(m.stateDir, "compose-overrides")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, p.ID+".yaml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// containerSideOf finds the container port behind a published host port, from
// the running containers when we have them. Falling back to the host port is
// correct for the common "3000:3000" style and harmless otherwise, because the
// override is only ever written for ports we parsed out of the compose file.
func containerSideOf(p model.Project, port model.Port) int {
	if port.ContainerPort > 0 {
		return port.ContainerPort
	}
	return port.Port
}

func declaredPort(p model.Port) int {
	if p.Declared > 0 {
		return p.Declared
	}
	return p.Port
}

// applyPortChoices persists the port decisions carried by an action before it
// runs. unitID scopes a clear to one row's ports; empty means the whole project.
func (m *Manager) applyPortChoices(projectID, unitID string, opts ActionOptions) error {
	if !opts.ClearOverrides && len(opts.PortOverrides) == 0 {
		return nil
	}
	view, ok := m.View(projectID)
	if !ok {
		return ErrUnknownProject
	}
	if opts.ClearOverrides {
		var only []int
		if unitID != "" {
			only = declaredPortsOfUnit(view, unitID)
			if len(only) == 0 {
				return nil
			}
		}
		return m.ClearPortOverrides(view.Path, view.Name, only)
	}
	return m.SetPortOverrides(view.Path, view.Name, opts.PortOverrides)
}

// declaredPortsOfUnit lists the keys under which a unit's overrides are stored.
//
// Overrides are keyed by the port the *project* declares, so clearing one has
// to look up 8080 even when the unit is currently running on 8081.
func declaredPortsOfUnit(view model.View, unitID string) []int {
	unit := findUnit(view.State.Units, unitID)
	if unit == nil {
		return nil
	}
	var out []int
	for _, p := range unit.Ports {
		switch {
		case p.Declared > 0:
			out = append(out, p.Declared)
		case p.Configured > 0:
			out = append(out, p.Configured)
		}
	}
	return out
}
