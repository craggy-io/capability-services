package registry

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// A cloned schema shares nothing mutable with its source: rewriting any
// slice, map, pointer or nested value of the clone leaves the source as it
// was, and the clone's JSON form equals the source's.
func TestCloneSchemaSharesNothing(t *testing.T) {
	min := 1.0
	var c any = map[string]any{"k": []any{"v"}}
	src := &jsonschema.Schema{Type: "object", Required: []string{"a"}, Enum: []any{"x", map[string]any{"y": 1}},
		Const: &c, Minimum: &min, Default: json.RawMessage(`{"a":1}`), Extra: map[string]any{"x-k": []any{1}},
		Properties: map[string]*jsonschema.Schema{"a": {Type: "string", Enum: []any{"p"}}}}
	want, _ := json.Marshal(src)

	cp := cloneSchema(src)
	if got, _ := json.Marshal(cp); string(got) != string(want) {
		t.Fatalf("clone JSON = %s, want %s", got, want)
	}
	cp.Required[0] = "changed"
	cp.Enum[1].(map[string]any)["y"] = 2
	(*cp.Const).(map[string]any)["k"].([]any)[0] = "changed"
	*cp.Minimum = 9
	cp.Default[2] = 'b'
	cp.Extra["x-k"].([]any)[0] = 2
	cp.Properties["a"].Enum[0] = "changed"
	cp.Properties["b"] = &jsonschema.Schema{}
	if got, _ := json.Marshal(src); string(got) != string(want) {
		t.Fatalf("mutating the clone rewrote the source: %s, want %s", got, want)
	}
}

// A hand-built cyclic schema is copied as the same cycle, not aliased.
func TestCloneSchemaKeepsCycles(t *testing.T) {
	src := &jsonschema.Schema{Type: "object"}
	src.Properties = map[string]*jsonschema.Schema{"self": src}
	cp := cloneSchema(src)
	if cp == src || cp.Properties["self"] != cp {
		t.Fatalf("cycle not preserved in the copy: clone %p, clone.self %p, source %p", cp, cp.Properties["self"], src)
	}
}

func BenchmarkCloneSchema(b *testing.B) {
	s := &jsonschema.Schema{Type: "object", Required: []string{"a", "b"}, Properties: map[string]*jsonschema.Schema{
		"a": {Type: "string", Description: "a field"}, "b": {Type: "array", Items: &jsonschema.Schema{Type: "integer"}}}}
	for b.Loop() {
		cloneSchema(s)
	}
}
