package tools_test

// Regression tests for the 2026-11-04 review findings in tools.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/tools"
)

// Defs are sorted by tool name (after '.'->'_' mapping), not by the
// capability name order List returns.
func TestReviewDefsSortedByToolName(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	mk := func(name string) registry.Capability {
		return registry.Func(name, name, func(ctx context.Context, i in) (in, error) { return i, nil })
	}
	if err := r.Add(mk("a.z"), mk("a1.x")); err != nil {
		t.Fatal(err)
	}
	defs := tools.Defs(r.View(registry.All))
	if len(defs) != 2 || defs[0].Name != "a1_x" || defs[1].Name != "a_z" {
		t.Fatalf("def order = %v, want [a1_x a_z]", []string{defs[0].Name, defs[1].Name})
	}
	for _, d := range defs {
		if d.InputSchema.Type != "object" {
			t.Fatalf("%s input schema type = %q, want object", d.Name, d.InputSchema.Type)
		}
	}
}

// A capability the registry accepts with a non-object input schema still
// yields an object tool schema (the MCP SDK panics otherwise).
func TestReviewNonObjectInputGetsObjectToolSchema(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	raw := registry.Func("raw.echo", "raw",
		func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) { return in, nil })
	str := registry.Func("str.echo", "str",
		func(ctx context.Context, s string) (string, error) { return s, nil })
	if err := r.Add(raw, str); err != nil {
		t.Fatal(err)
	}
	for _, d := range tools.Defs(r.View(registry.All)) {
		if d.InputSchema == nil || d.InputSchema.Type != "object" {
			t.Fatalf("%s: input schema %+v", d.Name, d.InputSchema)
		}
	}
}

// A non-object output with an advertised output schema still comes back
// as structured content.
func TestReviewStructuredOutputNonObject(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	c := registry.Func("str.out", "str",
		func(ctx context.Context, i in) (string, error) { return "hello", nil })
	if err := r.Add(c); err != nil {
		t.Fatal(err)
	}
	e, ok := r.Get("str.out")
	if !ok {
		t.Fatal("Get failed")
	}
	if got := tools.StructuredOutput(e, []byte(`"hello"`)); string(got) != `"hello"` {
		t.Fatalf("StructuredOutput = %s", got)
	}
	res := tools.Do(context.Background(), r.View(registry.All), "str_out", json.RawMessage(`{"text":"x"}`))
	if res.IsError || string(res.Structured) != `"hello"` {
		t.Fatalf("Do = %+v", res)
	}
}
