package jobs_test

// Regression test for the 2026-11-04 review: the hand-written JobStatus
// schema must name every field the JSON actually carries.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
	"github.com/google/jsonschema-go/jsonschema"
)

func jsonFieldNames(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}

func TestReviewJobStatusSchemaMatchesJSONTags(t *testing.T) {
	var statusCap registry.Capability
	for _, c := range jobs.Capabilities() {
		if c.Name == "jobs.get" {
			statusCap = c
		}
	}
	if statusCap.Output == nil {
		t.Fatal("jobs.get has no output schema")
	}
	schema := statusCap.Output
	for _, name := range jsonFieldNames(t, registry.JobStatus{}) {
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("job status schema missing JobStatus field %q", name)
		}
	}
	last := schema.Properties["last"]
	for _, name := range jsonFieldNames(t, registry.Update{}) {
		if _, ok := last.Properties[name]; !ok {
			t.Errorf("job status schema last missing Update field %q", name)
		}
	}
	errSchema := schema.Properties["error"]
	for _, name := range jsonFieldNames(t, registry.ErrorInfo{}) {
		if _, ok := errSchema.Properties[name]; !ok {
			t.Errorf("job status schema error missing ErrorInfo field %q", name)
		}
	}
	if raw, err := json.Marshal(schema); err != nil || !json.Valid(raw) {
		t.Fatalf("schema does not marshal: %v", err)
	}
	var _ *jsonschema.Schema = schema
}
