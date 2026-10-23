// Package composefile extracts the metadata we need from a Docker Compose file
// without depending on the compose libraries: which host ports it publishes and
// which services declare them.
package composefile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Names are the compose file names we recognise, in precedence order.
var Names = []string{
	"compose.yaml", "compose.yml",
	"docker-compose.yaml", "docker-compose.yml",
}

// OverrideNames are the files Compose loads automatically alongside the base
// file, in precedence order.
var OverrideNames = []string{
	"compose.override.yaml", "compose.override.yml",
	"docker-compose.override.yaml", "docker-compose.override.yml",
}

// Find returns the compose file in dir, or "" if there is none.
func Find(dir string) string {
	return firstExisting(dir, Names)
}

// FindOverride returns the conventional override file in dir, or "".
//
// This matters more than it looks. Running `docker compose up` with no -f loads
// the base file *and* its override sibling; passing -f suppresses that. Since
// the agent always passes -f — it has to, to add a port override file — it must
// re-supply the sibling explicitly, or a project that keeps its build context
// in docker-compose.override.yml silently builds the wrong thing.
func FindOverride(dir string) string {
	return firstExisting(dir, OverrideNames)
}

func firstExisting(dir string, names []string) string {
	for _, name := range names {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// Service is one service declared in a compose file, whether or not it has
// ever been created. Listing declared services is what lets the dashboard show
// a project's shape before it has been started for the first time.
type Service struct {
	Name  string
	Ports []PublishedPort
}

// AllPorts flattens the ports of every service, in host-port order.
func AllPorts(services []Service) []PublishedPort {
	var out []PublishedPort
	for _, svc := range services {
		out = append(out, svc.Ports...)
	}
	sortPorts(out)
	return out
}

// PublishedPort is a host port published by a compose service.
type PublishedPort struct {
	Host    int
	Service string
	// Variable is the environment variable the compose file uses to declare
	// this port, as in "${WEB_PORT:-3000}:80". When set, the port can be moved
	// by setting that variable — no file edits, no override files. When empty
	// the port is hard-coded and there is no clean way to change it.
	Variable string
	// Container is the port inside the container this maps to. Moving a
	// published port changes only the host side of that pair.
	Container int
}

type composeDoc struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Ports []yaml.Node `yaml:"ports"`
	} `yaml:"services"`
}

type longPort struct {
	Target    any    `yaml:"target"`
	Published any    `yaml:"published"`
	Protocol  string `yaml:"protocol"`
	Mode      string `yaml:"mode"`
}

// Parse reads a compose file and returns the project name override (the
// top-level `name:` key, if any) and the host ports it publishes.
//
// Ports are best-effort: compose supports ranges, variable interpolation and
// host-IP prefixes. Anything we cannot resolve statically is skipped rather
// than guessed, because a wrong port number in the UI is worse than a missing
// one — we also learn the real ports from `docker compose ps` once running.
func Parse(path string) (name string, services []Service, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	var doc composeDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", nil, fmt.Errorf("parse %s: %w", path, err)
	}

	env := dotEnv(filepath.Dir(path))
	seen := map[int]bool{}

	names := make([]string, 0, len(doc.Services))
	for svc := range doc.Services {
		names = append(names, svc)
	}
	// Map iteration order is random; sort so the UI does not reshuffle.
	sort.Strings(names)

	for _, svc := range names {
		s := doc.Services[svc]
		service := Service{Name: svc}
		for i := range s.Ports {
			resolved, target, variable := hostPorts(&s.Ports[i], env)
			for _, hp := range resolved {
				if hp <= 0 || hp > 65535 || seen[hp] {
					continue
				}
				seen[hp] = true
				service.Ports = append(service.Ports, PublishedPort{
					Host: hp, Service: svc, Variable: variable, Container: target,
				})
			}
		}
		sortPorts(service.Ports)
		// A service with no published ports still belongs on the dashboard;
		// "worker has no ports" is information, not a reason to hide it.
		services = append(services, service)
	}
	return doc.Name, services, nil
}

func sortPorts(ps []PublishedPort) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Host < ps[j-1].Host; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

// hostPorts resolves an entry to its host ports and the container port behind
// them, and reports the environment variable that produced the host side if the
// entry was written with one.
func hostPorts(n *yaml.Node, env map[string]string) (hosts []int, target int, variable string) {
	switch n.Kind {
	case yaml.ScalarNode:
		resolved, v := interpolate(n.Value, env)
		hosts, target = parseShort(resolved)
		return hosts, target, v
	case yaml.MappingNode:
		var lp longPort
		if err := n.Decode(&lp); err != nil {
			return nil, 0, ""
		}
		// A long-form entry without `published` publishes nothing on the host.
		if lp.Published == nil {
			return nil, 0, ""
		}
		resolved, v := interpolate(scalarString(lp.Published), env)
		targetResolved, _ := interpolate(scalarString(lp.Target), env)
		t := 0
		if ts := parseRange(targetResolved); len(ts) > 0 {
			t = ts[0]
		}
		return parseRange(resolved), t, v
	}
	return nil, 0, ""
}

func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// parseShort handles the short syntax:
//
//	"3000"                  -> container port only, nothing published
//	"3000:3000"             -> host 3000
//	"127.0.0.1:8001:8001"   -> host 8001
//	"8000-8005:80"          -> hosts 8000..8005
//	"6060:6060/udp"         -> host 6060
func parseShort(s string) (hosts []int, target int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, 0
	}
	if i := strings.Index(s, "/"); i >= 0 { // strip /tcp, /udp
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		// Container port only: docker picks a random host port, so there is no
		// stable host port to report.
		return nil, 0
	case 2:
		return parseRange(parts[0]), firstOf(parseRange(parts[1]))
	case 3:
		return parseRange(parts[1]), firstOf(parseRange(parts[2]))
	default:
		// IPv6 host addresses use brackets, e.g. "[::1]:8080:80".
		if i := strings.LastIndex(s, "]"); i >= 0 && i+1 < len(s) {
			return parseShort(s[i+2:])
		}
		return nil, 0
	}
}

func firstOf(ps []int) int {
	if len(ps) == 0 {
		return 0
	}
	return ps[0]
}

func parseRange(s string) []int {
	s = strings.TrimSpace(strings.Trim(s, `"'`))
	if s == "" {
		return nil
	}
	if lo, hi, ok := strings.Cut(s, "-"); ok {
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 != nil || err2 != nil || b < a || b-a > 64 {
			return nil
		}
		out := make([]int, 0, b-a+1)
		for p := a; p <= b; p++ {
			out = append(out, p)
		}
		return out
	}
	p, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return []int{p}
}

var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// interpolate resolves ${VAR}, ${VAR:-default} and $VAR using the project's
// .env file and the agent's own environment, matching compose's precedence
// (shell environment wins over .env). It also returns the first variable name
// it substituted, which is what tells us the port is movable.
func interpolate(s string, env map[string]string) (string, string) {
	if !strings.Contains(s, "$") {
		return s, ""
	}
	var firstVar string
	out := varRe.ReplaceAllStringFunc(s, func(m string) string {
		g := varRe.FindStringSubmatch(m)
		name, def := g[1], g[2]
		if name == "" {
			name = g[3]
		}
		if firstVar == "" {
			firstVar = name
		}
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		if v, ok := env[name]; ok && v != "" {
			return v
		}
		return def
	})
	return out, firstVar
}

// dotEnv reads a project's .env file. Errors are ignored: a missing or
// malformed .env just means fewer variables resolve.
func dotEnv(dir string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}
