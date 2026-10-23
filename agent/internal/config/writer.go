package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"gopkg.in/yaml.v3"
)

// writeMu serialises config writes within the process. Two dashboard tabs
// resolving conflicts at the same moment would otherwise race read-modify-write
// and one choice would silently vanish.
var writeMu sync.Mutex

// Writer edits the config file in place.
//
// It round-trips through yaml.Node rather than marshalling the Config struct,
// which matters: the file is hand-written and usually checked in, and
// re-marshalling a struct would strip every comment, drop unknown keys and
// reorder everything. Node editing touches only the keys we set.
type Writer struct {
	// Path is the file to edit. When it does not exist it is created.
	Path string
}

// NewWriter targets the file a Config was loaded from, falling back to the
// per-user config when the agent is running on defaults.
func NewWriter(cfg Config) *Writer {
	path := cfg.Path()
	if path == "" {
		path = DefaultPath()
	}
	return &Writer{Path: path}
}

// SetPortOverride records that a project should use `chosen` instead of the
// port it declares. A chosen value equal to declared removes the entry.
func (w *Writer) SetPortOverride(projectPath, projectName string, declared, chosen int) error {
	return w.edit(projectPath, projectName, func(entry *yaml.Node) {
		key := strconv.Itoa(declared)
		if chosen == declared || chosen == 0 {
			deleteMapEntry(mapValue(entry, "portOverrides"), key)
			pruneEmptyMap(entry, "portOverrides")
			return
		}
		m := ensureMap(entry, "portOverrides",
			"Written by devcc when you start a project on a different port.")
		setMapEntry(m, key, strconv.Itoa(chosen), "!!int")
	})
}

// ClearPortOverrides removes every port override for a project.
func (w *Writer) ClearPortOverrides(projectPath, projectName string, only []int) error {
	return w.edit(projectPath, projectName, func(entry *yaml.Node) {
		m := mapValue(entry, "portOverrides")
		if m == nil {
			return
		}
		if len(only) == 0 {
			removeKey(entry, "portOverrides")
			return
		}
		for _, declared := range only {
			deleteMapEntry(m, strconv.Itoa(declared))
		}
		pruneEmptyMap(entry, "portOverrides")
	})
}

// SetSubstitute records that a unit is already provided by something else.
// An empty target removes the entry.
func (w *Writer) SetSubstitute(projectPath, projectName, unit, target string) error {
	return w.edit(projectPath, projectName, func(entry *yaml.Node) {
		if target == "" {
			deleteMapEntry(mapValue(entry, "substitutes"), unit)
			pruneEmptyMap(entry, "substitutes")
			return
		}
		m := ensureMap(entry, "substitutes",
			"Written by devcc: these units are provided by something already running.")
		setMapEntry(m, unit, target, "!!str")
	})
}

// edit loads the file, hands the project's entry to fn, and writes it back
// atomically.
func (w *Writer) edit(projectPath, projectName string, fn func(entry *yaml.Node)) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	root, err := w.load()
	if err != nil {
		return err
	}
	doc := root.Content[0]
	projects := ensureSequence(doc, "projects")
	entry := findProject(projects, projectPath)
	if entry == nil {
		entry = newProjectEntry(projectName, projectPath)
		// `projects: []` is flow style; appending to it as-is would render the
		// whole list on one line. Block style is what a human would have typed.
		if len(projects.Content) == 0 {
			projects.Style = 0
		}
		projects.Content = append(projects.Content, entry)
	}
	fn(entry)
	return w.save(root)
}

func (w *Writer) load() (*yaml.Node, error) {
	raw, err := os.ReadFile(w.Path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", w.Path, err)
		}
		raw = []byte("# Written by devcc. Edit freely; the agent preserves your comments.\n")
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", w.Path, err)
	}
	if len(root.Content) == 0 {
		// An empty or comment-only file has no document node yet.
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: expected a mapping at the top level", w.Path)
	}
	return &root, nil
}

func (w *Writer) save(root *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	// The default of 4 would reindent the whole file on the first write.
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}

	if dir := filepath.Dir(w.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	// Write to a temporary file and rename, so a crash mid-write cannot leave
	// the user with a truncated config.
	tmp := w.Path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.Path)
}

// --- yaml.Node helpers ------------------------------------------------------

func mapValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func removeKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

func ensureMap(parent *yaml.Node, key, comment string) *yaml.Node {
	if existing := mapValue(parent, key); existing != nil {
		return existing
	}
	value := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, HeadComment: comment},
		value)
	return value
}

func ensureSequence(doc *yaml.Node, key string) *yaml.Node {
	if existing := mapValue(doc, key); existing != nil && existing.Kind == yaml.SequenceNode {
		return existing
	}
	if existing := mapValue(doc, key); existing != nil {
		// `projects: []` parses as a sequence already; anything else (null from
		// an empty key) gets replaced with one.
		existing.Kind = yaml.SequenceNode
		existing.Tag = "!!seq"
		existing.Value = ""
		return existing
	}
	value := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	doc.Content = append(doc.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	return value
}

// setMapEntry writes key: value, using the given YAML tag so port numbers come
// out as plain integers rather than quoted strings.
func setMapEntry(mapping *yaml.Node, key, value, tag string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = value
			mapping.Content[i+1].Tag = tag
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
	sortMapKeys(mapping)
}

func deleteMapEntry(mapping *yaml.Node, key string) {
	if mapping == nil {
		return
	}
	removeKey(mapping, key)
}

// pruneEmptyMap drops a key whose map has no entries left, so removing the last
// override does not leave `portOverrides: {}` behind.
func pruneEmptyMap(parent *yaml.Node, key string) {
	if m := mapValue(parent, key); m != nil && len(m.Content) == 0 {
		removeKey(parent, key)
	}
}

// sortMapKeys keeps generated maps in a stable order so the file does not
// churn in version control.
func sortMapKeys(mapping *yaml.Node) {
	type pair struct{ k, v *yaml.Node }
	pairs := make([]pair, 0, len(mapping.Content)/2)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		pairs = append(pairs, pair{mapping.Content[i], mapping.Content[i+1]})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		a, errA := strconv.Atoi(pairs[i].k.Value)
		b, errB := strconv.Atoi(pairs[j].k.Value)
		if errA == nil && errB == nil {
			return a < b
		}
		return pairs[i].k.Value < pairs[j].k.Value
	})
	mapping.Content = mapping.Content[:0]
	for _, p := range pairs {
		mapping.Content = append(mapping.Content, p.k, p.v)
	}
}

// findProject locates the entry whose path resolves to projectPath. Paths in
// the file may be relative or use ~, so they are expanded before comparing.
func findProject(projects *yaml.Node, projectPath string) *yaml.Node {
	base := ""
	for _, entry := range projects.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		pathNode := mapValue(entry, "path")
		if pathNode == nil {
			continue
		}
		if samePath(pathNode.Value, projectPath, base) {
			return entry
		}
	}
	return nil
}

func samePath(a, b, base string) bool {
	return resolve(a, base) == resolve(b, base)
}

func newProjectEntry(name, path string) *yaml.Node {
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if name != "" {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
	}
	entry.Content = append(entry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "path"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: path})
	return entry
}
