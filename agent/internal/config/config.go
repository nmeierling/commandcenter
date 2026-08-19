// Package config loads the optional user configuration file that supplements
// auto-discovery. Everything here has a working default, so the agent runs with
// no config file at all.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk configuration.
type Config struct {
	// Listen is the address the local API binds to. Must be a loopback address.
	Listen string `yaml:"listen"`
	Scan   Scan   `yaml:"scan"`
	Poll   Poll   `yaml:"poll"`
	// Projects supplement or override discovered projects, matched by path.
	Projects []Project `yaml:"projects"`
	// AllowRepoConfig enables reading .devcc.yaml from inside project
	// directories. Off by default: a repo-local file can specify commands the
	// agent would execute, so cloning an untrusted repo would otherwise be
	// enough to get code execution.
	AllowRepoConfig bool `yaml:"allowRepoConfig"`

	// path is where this config was loaded from (empty if defaults).
	path string `yaml:"-"`
}

// Scan controls auto-discovery.
type Scan struct {
	Roots []string `yaml:"roots"`
	// Depth is how many directory levels below each root to search.
	Depth int `yaml:"depth"`
	// Ignore are directory names skipped entirely during the walk.
	Ignore []string `yaml:"ignore"`
	// Enabled turns auto-discovery off, leaving only configured projects.
	Enabled *bool `yaml:"enabled"`
}

// Poll controls background status refresh.
type Poll struct {
	Interval         time.Duration `yaml:"interval"`
	HealthTimeout    time.Duration `yaml:"healthTimeout"`
	DiscoverEvery    time.Duration `yaml:"discoverEvery"`
	MaxConcurrent    int           `yaml:"maxConcurrent"`
	StartupSettleFor time.Duration `yaml:"startupSettleFor"`
}

// Project is a configured project entry. Zero fields mean "leave the discovered
// value alone"; set fields override discovery.
type Project struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
	// Kind forces the detected technology ("compose", "node", "go", ...).
	Kind string `yaml:"kind"`
	// Start/Stop are shell commands run in the project directory. Setting Start
	// switches a project to process-supervision mode.
	Start string            `yaml:"start"`
	Stop  string            `yaml:"stop"`
	Env   map[string]string `yaml:"env"`
	// Ports are the expected ports. Overrides ports parsed from a compose file.
	Ports []int `yaml:"ports"`
	// URLs are opened by the Open action. Defaults to http://localhost:<firstPort>.
	URLs   []string `yaml:"urls"`
	Health *Health  `yaml:"health"`
	// ComposeFile overrides which compose file to use.
	ComposeFile string `yaml:"composeFile"`
	Disabled    bool   `yaml:"disabled"`
}

// Health configures the HTTP health probe.
type Health struct {
	URL          string        `yaml:"url"`
	ExpectStatus int           `yaml:"expectStatus"`
	Timeout      time.Duration `yaml:"timeout"`
}

// Defaults returns a config with every field populated.
func Defaults() Config {
	return Config{
		Listen: "127.0.0.1:47123",
		Scan: Scan{
			Roots: defaultRoots(),
			Depth: 3,
			Ignore: []string{
				"node_modules", ".git", "vendor", "dist", "build", "target",
				".next", ".nuxt", ".venv", "venv", "__pycache__", ".cache",
				"Library", "Applications", ".Trash", "coverage", ".terraform",
			},
		},
		Poll: Poll{
			Interval:         5 * time.Second,
			HealthTimeout:    2 * time.Second,
			DiscoverEvery:    2 * time.Minute,
			MaxConcurrent:    6,
			StartupSettleFor: 90 * time.Second,
		},
	}
}

func defaultRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var roots []string
	for _, name := range []string{"Projects", "projects", "dev", "Developer", "src", "code", "workspace"} {
		p := filepath.Join(home, name)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			roots = append(roots, p)
		}
	}
	return roots
}

// LocalNames are config files looked for in the working directory.
var LocalNames = []string{"devcc.yaml", "devcc.yml"}

// DefaultPath resolves which config file to load, in order:
//
//  1. $DEVCC_CONFIG
//  2. ./devcc.yaml or ./devcc.yml in the working directory
//  3. the per-user config file
//
// The working-directory entry is what makes a project-local config work: keep a
// devcc.yaml next to your repos, run `devcc` there, and that file wins without
// any flags. Note that it means running `devcc` inside a directory you did not
// write means trusting its devcc.yaml, the same way running `make` there means
// trusting its Makefile. The startup banner always prints which file was used.
func DefaultPath() string {
	if v := os.Getenv("DEVCC_CONFIG"); v != "" {
		return v
	}
	for _, name := range LocalNames {
		if st, err := os.Stat(name); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(name); err == nil {
				return abs
			}
			return name
		}
	}
	return UserPath()
}

// UserPath is the per-user config location:
// ~/.config/devcc/config.yaml on Linux, ~/Library/Application Support/devcc/
// on macOS.
func UserPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "devcc", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "devcc", "config.yaml")
}

// Load reads path, filling in defaults for anything unset. A missing file is
// not an error: the agent is expected to work with pure auto-discovery.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		path = DefaultPath()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	// Unmarshal onto a copy of the defaults so unset keys keep their default,
	// but note that YAML replaces slices wholesale rather than merging them.
	var file Config
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg = merge(cfg, file)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	cfg.path = path

	// Relative paths are resolved against the config file's own directory, not
	// the shell's working directory. A devcc.yaml that says `./projects` should
	// mean the same thing however you invoke the agent.
	if err := cfg.normalize(filepath.Dir(path)); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Path reports where the config was loaded from ("" when defaults were used).
func (c Config) Path() string { return c.path }

func merge(base, file Config) Config {
	out := base
	if file.Listen != "" {
		out.Listen = file.Listen
	}
	if len(file.Scan.Roots) > 0 {
		out.Scan.Roots = file.Scan.Roots
	}
	if file.Scan.Depth > 0 {
		out.Scan.Depth = file.Scan.Depth
	}
	if len(file.Scan.Ignore) > 0 {
		// Append rather than replace: users almost always mean "also ignore".
		out.Scan.Ignore = append(append([]string{}, base.Scan.Ignore...), file.Scan.Ignore...)
	}
	if file.Scan.Enabled != nil {
		out.Scan.Enabled = file.Scan.Enabled
	}
	if file.Poll.Interval > 0 {
		out.Poll.Interval = file.Poll.Interval
	}
	if file.Poll.HealthTimeout > 0 {
		out.Poll.HealthTimeout = file.Poll.HealthTimeout
	}
	if file.Poll.DiscoverEvery > 0 {
		out.Poll.DiscoverEvery = file.Poll.DiscoverEvery
	}
	if file.Poll.MaxConcurrent > 0 {
		out.Poll.MaxConcurrent = file.Poll.MaxConcurrent
	}
	if file.Poll.StartupSettleFor > 0 {
		out.Poll.StartupSettleFor = file.Poll.StartupSettleFor
	}
	out.AllowRepoConfig = file.AllowRepoConfig
	out.Projects = file.Projects
	return out
}

func (c *Config) normalize(baseDir string) error {
	for i, r := range c.Scan.Roots {
		c.Scan.Roots[i] = resolve(r, baseDir)
	}
	seen := map[string]int{}
	for i := range c.Projects {
		p := &c.Projects[i]
		if p.Path == "" {
			return fmt.Errorf("config project %q: path is required", p.Name)
		}
		p.Path = resolve(p.Path, baseDir)
		if prev, dup := seen[p.Path]; dup {
			return fmt.Errorf("config projects %d and %d both use path %s", prev, i, p.Path)
		}
		seen[p.Path] = i
		if p.ComposeFile != "" && !filepath.IsAbs(p.ComposeFile) {
			p.ComposeFile = filepath.Join(p.Path, p.ComposeFile)
		}
	}
	return nil
}

// ScanEnabled reports whether auto-discovery should run.
func (c Config) ScanEnabled() bool {
	return c.Scan.Enabled == nil || *c.Scan.Enabled
}

// MissingRoots lists configured scan roots that do not exist or are not
// directories. The scanner skips them silently, which otherwise leaves you
// staring at an empty dashboard after a typo in a path.
func (c Config) MissingRoots() []string {
	var missing []string
	for _, root := range c.Scan.Roots {
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			missing = append(missing, root)
		}
	}
	return missing
}

// resolve expands ~ and $VARS, then makes the path absolute relative to
// baseDir (the config file's directory) rather than the process's working
// directory.
func resolve(p, baseDir string) string {
	p = ExpandPath(p)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if baseDir == "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		return abs
	}
	return filepath.Clean(filepath.Join(baseDir, p))
}

// ExpandPath expands a leading ~ and environment variables in a path.
func ExpandPath(p string) string {
	if p == "" {
		return p
	}
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return filepath.Clean(p)
}

// Example is a commented sample config written by `devcc init`.
const Example = `# Development Command Center configuration.
# Every key is optional: with no config file the agent auto-discovers projects
# under ~/Projects, ~/dev, ~/src and similar.

# Local API bind address. Must be a loopback address.
listen: 127.0.0.1:47123

scan:
  # Directories to search for projects.
  roots:
    - ~/Projects
  # How many levels below each root to descend.
  depth: 3
  # Extra directory names to skip (added to the built-in list).
  ignore:
    - .archive

poll:
  # How often to refresh project status.
  interval: 5s
  healthTimeout: 2s
  # How often to re-scan roots for new projects.
  discoverEvery: 2m

# Reading .devcc.yaml from inside project directories is off by default,
# because such a file can specify commands this agent would execute.
allowRepoConfig: false

# Configured projects supplement discovery and override detected values.
projects:
  - name: Ecommerce
    path: ~/Projects/ecommerce
    health:
      url: http://localhost:3000/health

  - name: Dashboard
    path: ~/Projects/dashboard
    ports: [3001]
    urls:
      - http://localhost:3001/admin
    health:
      url: http://localhost:3001

  # A non-Docker project: setting "start" puts it under process supervision.
  - name: API
    path: ~/Projects/api
    start: npm run dev
    ports: [4000]
    env:
      NODE_ENV: development
`
