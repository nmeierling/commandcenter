package core

import (
	"reflect"
	"testing"
)

func TestNormalizeCategories(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"trims and drops empties", []string{"  api ", "", "   "}, []string{"api"}},
		{"de-duplicates case-insensitively", []string{"API", "api", "Api"}, []string{"API"}},
		{"sorts, so tabs do not reshuffle", []string{"web", "api", "Data"}, []string{"api", "Data", "web"}},
		// Categories are flat; a slash reads as a hierarchy that does not exist.
		{"collapses would-be nesting", []string{"team/api"}, []string{"team-api"}},
		{"nothing at all", nil, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeCategories(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestNormalizeCategoriesTruncatesAbsurdLabels(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	got := NormalizeCategories([]string{long})
	if len(got) != 1 || len(got[0]) > maxCategoryLength {
		t.Errorf("a tab label must stay readable, got %d chars", len(got[0]))
	}
}
