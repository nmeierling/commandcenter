package core

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/devcc/agent/internal/model"
)

// nullArray matches any JSON key whose value is null. Every array-valued field
// the dashboard iterates must serialise as [] instead.
//
// This guard exists because the same bug shipped twice: a nil Go slice becomes
// JSON null, the UI calls .length on it, and the whole page fails to render
// with a message that points at minified React rather than at the field.
var nullArray = regexp.MustCompile(`"(ports|urls|containers|markers|scanRoots|missingRoots)":null`)

func TestZeroValueViewHasNoNullArrays(t *testing.T) {
	// A project with nothing set at all: no ports, no URLs, no containers.
	v := normalize(model.View{
		Project: model.Project{ID: "x", Name: "empty"},
		State:   model.State{Status: model.StatusStopped},
	})
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if m := nullArray.FindString(string(raw)); m != "" {
		t.Errorf("null array in payload: %s\nfull: %s", m, raw)
	}
}

func TestContainerWithoutPublishedPortsSerialisesAsEmptyArray(t *testing.T) {
	// A worker container publishes nothing; the UI still reads container.ports.
	v := normalize(model.View{
		Project: model.Project{ID: "x", Name: "svc"},
		State: model.State{
			Status:     model.StatusRunning,
			Containers: []model.Container{{Name: "svc-worker-1", Service: "worker", State: "running"}},
		},
	})
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if m := nullArray.FindString(string(raw)); m != "" {
		t.Errorf("null array in payload: %s\nfull: %s", m, raw)
	}
}
