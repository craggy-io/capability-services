package registry_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/craggy-io/capability-services/registry"
	"github.com/google/jsonschema-go/jsonschema"
)

// constCap answers out and records nested calls to requires through the
// handler's own view.
func constCap(name, out, digest string, requires ...string) registry.Capability {
	return registry.Capability{
		Name: name, Title: name, Input: &jsonschema.Schema{}, Requires: requires,
		Origin: registry.Origin{Kind: "test", Ref: name, Digest: digest},
		Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
			res := map[string]any{"out": out}
			var call struct{ Call string }
			_ = json.Unmarshal(in, &call)
			if call.Call != "" {
				nested, err := registry.ViewFrom(ctx).Invoke(ctx, call.Call, nil)
				if err != nil {
					return nil, err
				}
				res["nested"] = json.RawMessage(nested)
			}
			return res, nil
		},
	}
}

func outOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v struct{ Out string }
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.Out
}

func TestApplyAllOrNothing(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, constCap("a.x", "a1", "d1"), constCap("b.x", "b1", "d1"))
	bad := constCap("c.x", "c1", "")
	bad.Handler = nil
	err := r.Apply(
		registry.Change{Prefix: "a", Caps: []registry.Capability{constCap("a.x", "a2", "d2")}},
		registry.Change{Prefix: "c", Caps: []registry.Capability{bad}},
	)
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("Apply with an invalid capability = %v, want invalid", err)
	}
	out, err := r.Invoke(context.Background(), "a.x", nil)
	if err != nil || outOf(t, out) != "a1" {
		t.Fatalf("a.x after a failed Apply = %s, %v; want the previous a1", out, err)
	}
	if err := r.Apply(registry.Change{Prefix: "a"}, registry.Change{Prefix: "a.y"}); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("overlapping prefixes = %v, want invalid", err)
	}
	if err := r.Apply(
		registry.Change{Prefix: "a", Caps: []registry.Capability{constCap("a.x", "a2", "d2")}},
		registry.Change{Prefix: "b"},
	); err != nil {
		t.Fatal(err)
	}
	if out, _ := r.Invoke(context.Background(), "a.x", nil); outOf(t, out) != "a2" {
		t.Fatalf("a.x = %s, want a2", out)
	}
	if _, ok := r.Get("b.x"); ok {
		t.Fatal("b.x survived an Apply that unloaded b")
	}
}

func TestApplyIsOneSnapshot(t *testing.T) {
	r := registry.New()
	defer r.Close()
	set := func(v string) []registry.Change {
		return []registry.Change{
			{Prefix: "a", Caps: []registry.Capability{constCap("a.x", v, v)}},
			{Prefix: "b", Caps: []registry.Capability{constCap("b.x", v, v)}},
		}
	}
	if err := r.Apply(set("v0")...); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i < 200; i++ {
			_ = r.Apply(set(string(rune('a' + i%26)))...)
		}
		stop.Store(true)
	}()
	for !stop.Load() {
		es := r.List()
		if len(es) == 2 && es[0].Origin.Digest != es[1].Origin.Digest {
			t.Fatalf("List saw a mix: %s and %s", es[0].Origin.Digest, es[1].Origin.Digest)
		}
	}
	wg.Wait()
}

func TestPinKeepsOriginalAcrossReplace(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, constCap("p.top", "top1", "t1", "p.leaf"), constCap("p.leaf", "leaf1", "l1"))
	pin, err := r.View(registry.All).Pin("p.top")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	call := func(v *registry.View) (string, string) {
		raw, err := v.Invoke(ctx, "p.top", json.RawMessage(`{"call":"p.leaf"}`))
		if err != nil {
			t.Fatal(err)
		}
		var res struct {
			Out    string
			Nested struct{ Out string }
		}
		_ = json.Unmarshal(raw, &res)
		return res.Out, res.Nested.Out
	}
	if top, leaf := call(pin.View()); top != "top1" || leaf != "leaf1" {
		t.Fatalf("first pinned call = %s/%s", top, leaf)
	}
	if err := r.Replace("p", constCap("p.top", "top2", "t2", "p.leaf"), constCap("p.leaf", "leaf2", "l2")); err != nil {
		t.Fatal(err)
	}
	if top, leaf := call(pin.View()); top != "top1" || leaf != "leaf1" {
		t.Fatalf("pinned call after replace = %s/%s, want the original closure", top, leaf)
	}
	fresh, err := r.View(registry.All).Pin("p.top")
	if err != nil {
		t.Fatal(err)
	}
	if top, leaf := call(fresh.View()); top != "top2" || leaf != "leaf2" {
		t.Fatalf("new pin = %s/%s, want the activated closure", top, leaf)
	}
	ids := pin.Identities()
	if len(ids) != 2 || ids[0].Ref() != "p.leaf@1" || ids[0].Origin.Digest != "l1" || ids[1].Origin.Digest != "t1" {
		t.Fatalf("Identities = %+v", ids)
	}
	if calls := pin.Calls(); len(calls) != 2 {
		t.Fatalf("Calls = %+v, want both", calls)
	}
}

func TestPinRejectsUndeclared(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, constCap("q.top", "top", "t", "q.leaf"), constCap("q.leaf", "leaf", "l"), constCap("q.other", "other", "o"))
	pin, err := r.View(registry.All).Pin("q.top")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = pin.View().Invoke(ctx, "q.top", json.RawMessage(`{"call":"q.other"}`))
	if registry.CodeOf(err) != registry.Denied || registry.ReasonOf(err) != registry.ReasonUndeclared {
		t.Fatalf("nested undeclared call = %v, want denied/undeclared", err)
	}
	if _, err := pin.View().Invoke(ctx, "q.other", nil); registry.ReasonOf(err) != registry.ReasonUndeclared {
		t.Fatalf("direct undeclared call = %v", err)
	}
	if calls := pin.Calls(); len(calls) != 1 || calls[0].Name != "q.top" {
		t.Fatalf("Calls = %+v, want only q.top", calls)
	}
	if _, err := r.View(registry.All).Pin("q.missing"); registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("pin of a missing ref = %v", err)
	}
	mustAdd(t, r, constCap("q.broken", "b", "b", "q.nowhere"))
	if _, err := r.View(registry.All).Pin("q.broken"); registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("pin with a missing requirement = %v", err)
	}
}

func TestPinIntersectsCallerView(t *testing.T) {
	r := registry.New()
	defer r.Close()
	secret := constCap("s.secret", "s", "s")
	secret.Tags = []string{"admin"}
	mustAdd(t, r, constCap("s.top", "top", "t", "s.secret"), secret)
	pin, err := r.View(registry.All).Pin("s.top")
	if err != nil {
		t.Fatal(err)
	}
	user := r.View(registry.Not(registry.Tagged("admin")))
	ctx := registry.WithView(context.Background(), user)
	_, err = pin.View().Invoke(ctx, "s.top", json.RawMessage(`{"call":"s.secret"}`))
	if registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("pinned call outside the caller's view = %v, want not_found", err)
	}
}
