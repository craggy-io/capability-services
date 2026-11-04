package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/registry"
)

type descIn struct {
	ID string `json:"id" jsonschema:"the id"`
	N  int    `json:"n,omitempty"`
}

type descOut struct {
	OK bool `json:"ok"`
}

func TestDescriptorIsFullProjection(t *testing.T) {
	r := registry.New()
	defer r.Close()
	err := r.Add(
		registry.Func("demo.full", "Full demo",
			func(_ context.Context, _ descIn) (descOut, error) { return descOut{OK: true}, nil },
			registry.Describe("Does everything, descriptively."),
			registry.WithVersion(2),
			registry.WithEffects(registry.ReadOnly|registry.Idempotent),
			registry.WithTags("demo", "agent"),
			registry.WithSupersedes("demo.old"),
			registry.WithAccepts(registry.Subject{Type: "text.csv", Level: "raw"}),
			registry.WithEmits(registry.Subject{Type: "table"}),
			registry.AsLong(90*time.Second),
			registry.WithCheck(func(context.Context) error { return errors.New("not warmed up") }),
			registry.WithOrigin(registry.Origin{Kind: "script", Ref: "demo.star", Digest: "sha256:abc"}),
		),
		registry.Func("demo.old", "Old demo",
			func(_ context.Context, _ descIn) (descOut, error) { return descOut{}, nil },
		),
	)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	all := r.Descriptors(true)
	if len(all) != 2 {
		t.Fatalf("Descriptors(all) = %d, want 2", len(all))
	}
	var full, old registry.Descriptor
	for _, d := range all {
		switch d.Name {
		case "demo.full":
			full = d
		case "demo.old":
			old = d
		}
	}
	if full.Ref != "demo.full@2" || full.Version != 2 || full.Title != "Full demo" ||
		full.Description != "Does everything, descriptively." {
		t.Errorf("identity fields wrong: %+v", full)
	}
	if !strings.Contains(string(full.Input), `"id"`) || !strings.Contains(string(full.Output), `"ok"`) {
		t.Errorf("schemas missing: in=%s out=%s", full.Input, full.Output)
	}
	if got := strings.Join(full.Effects, ","); got != "read_only,idempotent" {
		t.Errorf("effects = %q", got)
	}
	if got := strings.Join(full.Tags, ","); got != "demo,agent" {
		t.Errorf("tags = %q", got)
	}
	if len(full.Supersedes) != 1 || full.Supersedes[0] != "demo.old" {
		t.Errorf("supersedes = %v", full.Supersedes)
	}
	if len(full.Accepts) != 1 || full.Accepts[0] != (registry.Subject{Type: "text.csv", Level: "raw"}) {
		t.Errorf("accepts = %v", full.Accepts)
	}
	if len(full.Emits) != 1 || full.Emits[0].Type != "table" {
		t.Errorf("emits = %v", full.Emits)
	}
	if !full.Long || full.HeartbeatMs != 90000 {
		t.Errorf("long/heartbeat = %v/%d, want true/90000", full.Long, full.HeartbeatMs)
	}
	if !full.HasCheck || full.Ready || !strings.Contains(full.CheckError, "not warmed up") {
		t.Errorf("check state = hasCheck %v ready %v err %q", full.HasCheck, full.Ready, full.CheckError)
	}
	if full.Origin == nil || *full.Origin != (registry.Origin{Kind: "script", Ref: "demo.star", Digest: "sha256:abc"}) {
		t.Errorf("origin = %+v", full.Origin)
	}
	if old.Origin != nil {
		t.Errorf("old has no origin, descriptor shows %+v", old.Origin)
	}
	if !full.Current {
		t.Error("demo.full should be current")
	}
	if len(old.SupersededBy) != 1 || old.SupersededBy[0] != "demo.full" {
		t.Errorf("old supersededBy = %v, want [demo.full]", old.SupersededBy)
	}

	// List form hides the superseded entry; JSON round-trips.
	list := r.Descriptors(false)
	if len(list) != 1 || list[0].Name != "demo.full" {
		t.Fatalf("Descriptors(list) = %+v, want only demo.full", list)
	}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"heartbeatMs":90000`, `"hasCheck":true`, `"ref":"demo.full@2"`, `"origin":{"kind":"script","ref":"demo.star","digest":"sha256:abc"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON missing %s: %s", want, b)
		}
	}

	// Views filter descriptors exactly as they filter entries.
	v := r.View(registry.Tagged("demo"))
	if got := v.Descriptors(true); len(got) != 1 || got[0].Name != "demo.full" {
		t.Errorf("view descriptors = %+v, want only demo.full", got)
	}
}

func TestExamplesValidateAndProject(t *testing.T) {
	fn := func(_ context.Context, _ descIn) (descOut, error) { return descOut{}, nil }
	r := registry.New()
	defer r.Close()
	err := r.Add(registry.Func("demo.ex", "Example demo", fn,
		registry.Describe("Has examples."),
		registry.WithExample("by id", descIn{ID: "a1"}),
		registry.WithExample("", map[string]any{"id": "b2", "n": 3}),
	))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e, _ := r.Get("demo.ex")
	note := registry.ExampleNote(e.Capability)
	if want := "Examples:\n- by id: {\"id\":\"a1\"}\n- {\"id\":\"b2\",\"n\":3}"; note != want {
		t.Fatalf("ExampleNote = %q, want %q", note, want)
	}
	d := e.Descriptor()
	if len(d.Examples) != 2 || d.Examples[0].Note != "by id" {
		t.Fatalf("descriptor examples = %+v", d.Examples)
	}

	bad := []registry.Capability{
		registry.Func("demo.bad1", "Bad", fn, registry.WithExample("wrong type", map[string]any{"id": 7})),
		registry.Func("demo.bad2", "Bad", fn, registry.WithExample("missing id", map[string]any{"n": 1})),
		registry.Func("demo.bad3", "Bad", fn, registry.WithExample("two\nlines", descIn{ID: "x"})),
	}
	for _, c := range bad {
		if err := r.Add(c); registry.CodeOf(err) != registry.Invalid {
			t.Errorf("Add(%s) = %v, want Invalid", c.Name, err)
		}
	}
	noInput := registry.Capability{Name: "demo.noinput", Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil },
		Examples: []registry.Example{{Input: json.RawMessage(`{}`)}}}
	if err := r.Add(noInput); registry.CodeOf(err) != registry.Invalid {
		t.Errorf("Add(no input schema with examples) = %v, want Invalid", err)
	}
}
