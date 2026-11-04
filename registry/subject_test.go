package registry_test

import (
	"context"
	"testing"

	"github.com/craggy-io/capability-services/registry"
)

func TestSubjectsRoundTripTrimAndIsolate(t *testing.T) {
	r := registry.New()
	t.Cleanup(func() { r.Close() })
	accepts := []registry.Subject{{Type: " android.apk ", Level: " validated "}}
	c := registry.Func("apk.decompile", "Decompile",
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		registry.WithAccepts(accepts...),
		registry.WithEmits(registry.Subject{Type: "android.dex"}),
	)
	if err := r.Add(c); err != nil {
		t.Fatal(err)
	}
	accepts[0].Type = "mutated"
	e, ok := r.Get("apk.decompile")
	if !ok {
		t.Fatal("missing apk.decompile")
	}
	if len(e.Accepts) != 1 || e.Accepts[0].Type != "android.apk" || e.Accepts[0].Level != "validated" {
		t.Fatalf("accepts = %+v", e.Accepts)
	}
	if len(e.Emits) != 1 || e.Emits[0].Type != "android.dex" || e.Emits[0].Level != "" {
		t.Fatalf("emits = %+v", e.Emits)
	}
	note := registry.SubjectNote(e.Capability)
	if note != "Accepts: android.apk (validated)\n\nEmits: android.dex" {
		t.Fatalf("note = %q", note)
	}
}

func TestSubjectsRejectEmptyType(t *testing.T) {
	r := registry.New()
	t.Cleanup(func() { r.Close() })
	c := registry.Func("apk.x", "X",
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		registry.WithAccepts(registry.Subject{Type: "  "}),
	)
	err := r.Add(c)
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("empty type: %v", err)
	}
}
