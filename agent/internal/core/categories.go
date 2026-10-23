package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/devcc/agent/internal/model"
)

// maxCategoryLength keeps a tab label readable; anything longer is a sentence,
// not a category.
const maxCategoryLength = 40

// NormalizeCategories cleans a category list: trimmed, de-duplicated
// case-insensitively, empties dropped, sorted.
//
// Sorting matters because the result is written to the config file and read
// back as tab order; an unsorted list would reshuffle the tabs whenever
// something was added.
func NormalizeCategories(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		// Categories are flat. A slash reads as a hierarchy that does not
		// exist, so collapse it rather than pretend.
		c = strings.ReplaceAll(c, "/", "-")
		if c == "" {
			continue
		}
		if len(c) > maxCategoryLength {
			c = strings.TrimSpace(c[:maxCategoryLength])
		}
		key := strings.ToLower(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

// Categories lists every category in use, for the dashboard's tab bar.
func (m *Manager) Categories() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]string{}
	for _, e := range m.byID {
		for _, c := range e.project.Categories {
			if _, ok := seen[strings.ToLower(c)]; !ok {
				seen[strings.ToLower(c)] = c
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// SetCategories replaces a project's categories and persists them to the config
// file, so tabs survive a restart and can be edited by hand or shared.
func (m *Manager) SetCategories(ctx context.Context, projectID string, categories []string) (model.View, error) {
	view, ok := m.View(projectID)
	if !ok {
		return model.View{}, ErrUnknownProject
	}
	clean := NormalizeCategories(categories)
	if err := m.writer().SetCategories(view.Path, view.Name, clean); err != nil {
		return model.View{}, fmt.Errorf("save to config: %w", err)
	}
	if err := m.reloadConfig(); err != nil {
		return model.View{}, err
	}
	m.refresh(ctx, projectID, m.snapshot(ctx))
	out, _ := m.View(projectID)
	return out, nil
}
