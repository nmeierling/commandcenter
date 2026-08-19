// Package composefile extracts the metadata we need from a Docker Compose file
// without depending on the compose libraries: which host ports it publishes and
// which services declare them.
package composefile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Names are the compose file names we recognise, in precedence order.
var Names = []string{
	"compose.yaml", "compose.yml",
	"docker-compose.yaml", "docker-compose.yml",
}

// Find returns the compose file in dir, or "" if there is none.
func Find(dir string) string {
	for _, name := range Names {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// PublishedPort is a host port published by a compose service.
type PublishedPort struct {
	Host    int
	Service string
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
func Parse(path string) (name string, ports []PublishedPort, err error) {
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
	for svc, s := range doc.Services {
		for i := range s.Ports {
			for _, hp := range hostPorts(&s.Ports[i], env) {
				if hp <= 0 || hp > 65535 || seen[hp] {
					continue
				}
				seen[hp] = true
				ports = append(ports, PublishedPort{Host: hp, Service: svc})
			}
		}
	}
	// Map iteration order is random; sort so the UI does not reshuffle.
	sortPorts(ports)
	return doc.Name, ports, nil
}

func sortPorts(ps []PublishedPort) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Host < ps[j-1].Host; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

func hostPorts(n *yaml.Node, env map[string]string) []int {
	switch n.Kind {
	case yaml.ScalarNode:
		return parseShort(interpolate(n.Value, env))
	case yaml.MappingNode:
		var lp longPort
		if err := n.Decode(&lp); err != nil {
			return nil
		}
		// A long-form entry without `published` publishes nothing on the host.
		if lp.Published == nil {
			return nil
		}
		return parseRange(interpolate(scalarString(lp.Published), env))
	}
	return nil
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
func parseShort(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if i := strings.Index(s, "/"); i >= 0 { // strip /tcp, /udp
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		// Container port only: docker picks a random host port, so there is no
		// stable host port to report.
		return nil
	case 2:
		return parseRange(parts[0])
	case 3:
		return parseRange(parts[1])
	default:
		// IPv6 host addresses use brackets, e.g. "[::1]:8080:80".
		if i := strings.LastIndex(s, "]"); i >= 0 && i+1 < len(s) {
			return parseShort(s[i+2:])
		}
		return nil
	}
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
// (shell environment wins over .env).
func interpolate(s string, env map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		g := varRe.FindStringSubmatch(m)
		name, def := g[1], g[2]
		if name == "" {
			name = g[3]
		}
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		if v, ok := env[name]; ok && v != "" {
			return v
		}
		return def
	})
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
