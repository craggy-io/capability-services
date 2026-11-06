package starlarkcap_test

import (
	"context"
	"encoding/json"
	"testing"
	"testing/fstest"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

func catalogOf(t *testing.T, files map[string]string, ids ...string) *bundle.Catalog {
	t.Helper()
	var contribs []string
	for _, id := range ids {
		contribs = append(contribs, "{id: "+id+", kind: capability.script@1, path: "+id+".star}")
	}
	fsys := fstest.MapFS{"bundle.yaml": {Data: manifest("test.caps", contribs...)}}
	for p, text := range files {
		fsys[p] = &fstest.MapFile{Data: []byte(text)}
	}
	cat, err := bundle.Load(context.Background(), bundle.Options{
		Sources:   []bundle.Source{bundle.Embedded("test", fsys)},
		Compilers: starlarkcap.Compilers(starlarkcap.LoadOptions{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func answer(t *testing.T, r *registry.Registry, ref string) string {
	t.Helper()
	out, err := r.Invoke(context.Background(), ref, nil)
	if err != nil {
		t.Fatalf("%s: %v", ref, err)
	}
	var v string
	_ = json.Unmarshal(out, &v)
	return v
}

func TestActivateWholeCatalog(t *testing.T) {
	r := registry.New()
	defer r.Close()
	a := starlarkcap.NewActivator(r, starlarkcap.LoadOptions{})
	v1 := catalogOf(t, map[string]string{
		"a.star": `capability(name = "a.x", run = lambda ctx, i: "a1")`,
		"b.star": `capability(name = "b.x", run = lambda ctx, i: "b1")`,
	}, "a", "b")
	act := a.Activate(context.Background(), v1)
	if act.Err != nil || act.Active.Catalog != v1.Digest() || len(act.Active.Files) != 2 {
		t.Fatalf("first activation = %+v", act)
	}
	// A candidate whose second file supersedes something outside its prefix
	// is refused whole: a.x stays a1 even though a.star changed too.
	if err := r.Add(registry.Capability{Name: "vault.read", Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }}); err != nil {
		t.Fatal(err)
	}
	bad := catalogOf(t, map[string]string{
		"a.star": `capability(name = "a.x", run = lambda ctx, i: "a2")`,
		"b.star": `capability(name = "b.x", supersedes = ["vault.read"], run = lambda ctx, i: "b2")`,
	}, "a", "b")
	act = a.Activate(context.Background(), bad)
	if act.Err == nil || act.Attempted.Catalog != bad.Digest() || act.Active.Catalog != v1.Digest() {
		t.Fatalf("refused activation = %+v", act)
	}
	if got := answer(t, r, "a.x"); got != "a1" {
		t.Fatalf("a.x after a refused activation = %q, want a1", got)
	}
	if a.Active().Catalog != v1.Digest() {
		t.Fatal("Active moved on a refused activation")
	}
	// Dropping b unloads it; a moves to a3 in the same change.
	v3 := catalogOf(t, map[string]string{"a.star": `capability(name = "a.x", run = lambda ctx, i: "a3")`}, "a")
	if act = a.Activate(context.Background(), v3); act.Err != nil {
		t.Fatal(act.Err)
	}
	if got := answer(t, r, "a.x"); got != "a3" {
		t.Fatalf("a.x = %q, want a3", got)
	}
	if _, ok := r.Get("b.x"); ok {
		t.Fatal("b.x still registered after leaving the catalog")
	}
}

func TestActivateRefusesForeignPrefix(t *testing.T) {
	r := registry.New()
	defer r.Close()
	if _, err := starlarkcap.Load(context.Background(), r, []bundle.Source{bundle.Embedded("other", fstest.MapFS{
		"bundle.yaml": {Data: manifest("other.caps", "{id: a, kind: capability.script@1, path: a.star}")},
		"a.star":      {Data: []byte(`capability(name = "a.x", run = lambda ctx, i: "other")`)},
	})}, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	a := starlarkcap.NewActivator(r, starlarkcap.LoadOptions{})
	act := a.Activate(context.Background(), catalogOf(t, map[string]string{"a.star": `capability(name = "a.x", run = lambda ctx, i: "mine")`}, "a"))
	if registry.CodeOf(act.Err) != registry.Conflict {
		t.Fatalf("activation over a foreign prefix = %v, want conflict", act.Err)
	}
	if got := answer(t, r, "a.x"); got != "other" {
		t.Fatalf("a.x = %q", got)
	}
}

func TestRequiresPinsNestedScriptCalls(t *testing.T) {
	r := registry.New()
	defer r.Close()
	a := starlarkcap.NewActivator(r, starlarkcap.LoadOptions{})
	files := func(leaf string) map[string]string {
		return map[string]string{
			"top.star":   `capability(name = "top.run", requires = ["leaf.get"], run = lambda ctx, i: ctx.call("leaf.get"))`,
			"leaf.star":  `capability(name = "leaf.get", run = lambda ctx, i: "` + leaf + `")`,
			"stray.star": `capability(name = "stray.run", run = lambda ctx, i: ctx.call("top.run") + ctx.call("leaf.get"))`,
		}
	}
	if act := a.Activate(context.Background(), catalogOf(t, files("one"), "top", "leaf", "stray")); act.Err != nil {
		t.Fatal(act.Err)
	}
	pin, err := r.View(registry.All).Pin("top.run")
	if err != nil {
		t.Fatal(err)
	}
	if act := a.Activate(context.Background(), catalogOf(t, files("two"), "top", "leaf", "stray")); act.Err != nil {
		t.Fatal(act.Err)
	}
	out, err := pin.View().Invoke(context.Background(), "top.run", nil)
	if err != nil || string(out) != `"one"` {
		t.Fatalf("pinned nested call = %s, %v; want the original leaf", out, err)
	}
	if got := answer(t, r, "top.run"); got != "two" {
		t.Fatalf("unpinned call = %q, want the activated leaf", got)
	}
	var digests []string
	for _, id := range pin.Identities() {
		if id.Origin.Kind != "script" || id.Origin.Digest == "" {
			t.Fatalf("identity without script provenance: %+v", id)
		}
		digests = append(digests, id.Origin.Digest)
	}
	if len(digests) != 2 {
		t.Fatalf("closure = %v, want top and leaf", digests)
	}
	strayPin, err := r.View(registry.All).Pin("stray.run")
	if err != nil {
		t.Fatal(err)
	}
	_, err = strayPin.View().Invoke(context.Background(), "stray.run", nil)
	if registry.ReasonOf(err) != registry.ReasonUndeclared {
		t.Fatalf("undeclared nested script call = %v, want denied/undeclared", err)
	}
}
