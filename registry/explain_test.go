package registry_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/craggy-io/capability-services/registry"
)

const explainSchema = `{
  "type": "object",
  "properties": {
    "id":    {"type": "string"},
    "count": {"type": "integer", "minimum": 1, "maximum": 10},
    "mode":  {"type": "string", "enum": ["quick", "full"]},
    "opts":  {"type": "object", "properties": {"depth": {"type": "integer"}}, "additionalProperties": false},
    "tags":  {"type": "array", "items": {"type": "string"}}
  },
  "required": ["id", "mode"],
  "additionalProperties": false
}`

// explainReg registers demo.explain with the schema above and, when
// withExample, one example.
func explainReg(t *testing.T, withExample bool) *registry.Registry {
	t.Helper()
	s, err := registry.ParseSchema(json.RawMessage(explainSchema))
	if err != nil {
		t.Fatal(err)
	}
	opts := []registry.Option{registry.WithInput(s)}
	if withExample {
		opts = append(opts, registry.WithExample("quick scan", map[string]any{"id": "a1", "mode": "quick"}))
	}
	r := registry.New()
	t.Cleanup(r.Close)
	fn := func(context.Context, json.RawMessage) (any, error) { return map[string]any{}, nil }
	if err := r.Add(registry.Func("demo.explain", "Explain demo", fn, opts...)); err != nil {
		t.Fatal(err)
	}
	return r
}

func invalidMessage(t *testing.T, r *registry.Registry, in string) string {
	t.Helper()
	_, err := r.Invoke(context.Background(), "demo.explain", json.RawMessage(in))
	if registry.CodeOf(err) != registry.Invalid || registry.ReasonOf(err) != "" {
		t.Fatalf("Invoke(%s) = %v, want plain invalid", in, err)
	}
	return registry.Info(err).Message
}

func TestInvalidInputExplained(t *testing.T) {
	r := explainReg(t, false)
	cases := []struct{ name, in, want string }{
		{"missing required", `{"id":"x"}`, "- mode: missing required field"},
		{"wrong type", `{"id":7,"mode":"quick"}`, "- id: wrong type: expected string, got integer 7"},
		{"wrong type nested", `{"id":"x","mode":"full","tags":["a",3]}`, "- tags[1]: wrong type: expected string, got integer 3"},
		{"enum", `{"id":"x","mode":"fast"}`, `- mode: "fast" is not an allowed value; use one of: "quick", "full"`},
		{"unknown field", `{"id":"x","mode":"full","colour":"red"}`, "- colour: unknown field; accepted fields: count, id, mode, opts, tags"},
		{"unknown nested", `{"id":"x","mode":"full","opts":{"depht":2}}`, "- opts.depht: unknown field; accepted fields: depth"},
		{"out of range", `{"id":"x","mode":"full","count":11}`, "- count: out of range: 11 is above the maximum 10"},
		{"not an object", `[1]`, "- input: wrong type: expected object, got array"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := invalidMessage(t, r, c.in)
			if !strings.HasPrefix(msg, "demo.explain@1: invalid input:") {
				t.Errorf("message lacks header:\n%s", msg)
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("message lacks %q:\n%s", c.want, msg)
			}
			if !strings.Contains(msg, "\nRequired fields: id, mode.") {
				t.Errorf("message lacks required fields:\n%s", msg)
			}
			if strings.Contains(msg, "Example input") {
				t.Errorf("no example declared, but message has one:\n%s", msg)
			}
		})
	}
}

func TestInvalidInputListsEveryProblem(t *testing.T) {
	r := explainReg(t, false)
	msg := invalidMessage(t, r, `{"count":"3","extra":true}`)
	for _, want := range []string{
		"- id: missing required field",
		"- mode: missing required field",
		`- count: wrong type: expected integer, got string "3"`,
		"- extra: unknown field",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

func TestInvalidInputAppendsExample(t *testing.T) {
	r := explainReg(t, true)
	msg := invalidMessage(t, r, `{"id":"x"}`)
	if want := "\nExample input: {\"id\":\"a1\",\"mode\":\"quick\"}"; !strings.HasSuffix(msg, want) {
		t.Errorf("message does not end with %q:\n%s", want, msg)
	}
}

func TestInvalidInputBounded(t *testing.T) {
	r := explainReg(t, true)
	// Forty unknown fields with long names: far more than the bound.
	fields := map[string]any{"id": "x", "mode": "quick"}
	for i := range 40 {
		fields[fmt.Sprintf("field_%02d_%s", i, strings.Repeat("x", 80))] = i
	}
	in, _ := json.Marshal(fields)
	msg := invalidMessage(t, r, string(in))
	if len(msg) > 1500 {
		t.Errorf("message is %d bytes, want <= 1500", len(msg))
	}
	if !strings.Contains(msg, "more") {
		t.Errorf("message does not say problems were left out:\n%s", msg)
	}
	if !strings.Contains(msg, "Required fields: id, mode.") || !strings.Contains(msg, "Example input: ") {
		t.Errorf("bounding dropped the required fields or example:\n%s", msg)
	}
	if n := strings.Count(msg, "\n- field_"); n == 0 || n > 8 {
		t.Errorf("listed %d problems, want 1..8:\n%s", n, msg)
	}
}

// A Go-derived schema (struct tags, additionalProperties false) gets the
// same treatment as a hand-written one.
func TestInvalidInputDerivedSchema(t *testing.T) {
	r := registry.New()
	defer r.Close()
	fn := func(_ context.Context, _ descIn) (descOut, error) { return descOut{}, nil }
	if err := r.Add(registry.Func("demo.derived", "Derived", fn, registry.WithExample("", descIn{ID: "a1"}))); err != nil {
		t.Fatal(err)
	}
	_, err := r.Invoke(context.Background(), "demo.derived", json.RawMessage(`{"n":"2","idd":"a"}`))
	msg := registry.Info(err).Message
	for _, want := range []string{
		"- id: missing required field",
		`- n: wrong type: expected integer, got string "2"`,
		"- idd: unknown field; accepted fields: id, n",
		"Required fields: id.",
		`Example input: {"id":"a1"}`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}
