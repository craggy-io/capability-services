package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/registry"
	"github.com/google/jsonschema-go/jsonschema"
)

func echoCap(name string) registry.Capability {
	return registry.Capability{
		Name: name, Title: "echo", Input: &jsonschema.Schema{},
		Handler: func(ctx context.Context, in json.RawMessage) (any, error) { return in, nil },
	}
}

type addIn struct {
	A int `json:"a" jsonschema:"first"`
	B int `json:"b" jsonschema:"second"`
}
type addOut struct {
	Sum int `json:"sum"`
}

func addCap() registry.Capability {
	return registry.Func("math.add", "Add two ints",
		func(ctx context.Context, in addIn) (addOut, error) { return addOut{in.A + in.B}, nil },
		registry.WithEffects(registry.ReadOnly|registry.Idempotent), registry.WithTags("agent"))
}

func mustAdd(t *testing.T, r *registry.Registry, caps ...registry.Capability) {
	t.Helper()
	if err := r.Add(caps...); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestFuncInvokeAndSchema(t *testing.T) {
	r := registry.New()
	defer r.Close()
	c := addCap()
	if c.Input == nil || c.Output == nil {
		t.Fatal("Func did not derive schemas")
	}
	b, _ := json.Marshal(c.Input)
	if !strings.Contains(string(b), `"first"`) || !strings.Contains(string(b), `"a"`) {
		t.Fatalf("derived input schema unexpected: %s", b)
	}
	mustAdd(t, r, c)
	out, err := r.Invoke(context.Background(), "math.add", json.RawMessage(`{"a":2,"b":40}`))
	if err != nil || string(out) != `{"sum":42}` {
		t.Fatalf("invoke = %s, %v", out, err)
	}
	// Schema validation happens in the registry, before the handler.
	_, err = r.Invoke(context.Background(), "math.add", json.RawMessage(`{"a":"x","b":1}`))
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("bad types: code=%v err=%v", registry.CodeOf(err), err)
	}
	_, err = r.Invoke(context.Background(), "math.add", json.RawMessage(`{"a":1}`))
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("missing required: code=%v err=%v", registry.CodeOf(err), err)
	}
	_, err = r.Invoke(context.Background(), "math.add", json.RawMessage(`{nope`))
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("bad json: code=%v", registry.CodeOf(err))
	}
	_, err = r.Invoke(context.Background(), "math.nope", nil)
	if registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("unknown: code=%v", registry.CodeOf(err))
	}
}

func TestAddValidation(t *testing.T) {
	r := registry.New()
	defer r.Close()
	bad := []registry.Capability{
		{Name: "Bad.Name", Handler: echoCap("x.y").Handler},
		{Name: "has_underscore.x", Handler: echoCap("x.y").Handler},
		{Name: "a..b", Handler: echoCap("x.y").Handler},
		{Name: strings.Repeat("a", 65), Handler: echoCap("x.y").Handler},
		{Name: "ok.name"}, // nil handler
		{Name: "ok.name", Version: -2, Handler: echoCap("x.y").Handler},
		{Name: "ok.name", Supersedes: []string{"Bad Ref!"}, Handler: echoCap("x.y").Handler},
		{Name: "ok.name", Handler: echoCap("x.y").Handler,
			Input: &jsonschema.Schema{Ref: "#/$defs/missing"}},
	}
	for i, c := range bad {
		if err := r.Add(c); err == nil {
			t.Fatalf("case %d (%s): Add succeeded, want error", i, c.Name)
		} else if registry.CodeOf(err) != registry.Invalid {
			t.Fatalf("case %d: code=%v want invalid (%v)", i, registry.CodeOf(err), err)
		}
	}
	if n := len(r.All()); n != 0 {
		t.Fatalf("failed Adds registered %d caps", n)
	}
	mustAdd(t, r, echoCap("a.b"))
	err := r.Add(echoCap("a.b"))
	if registry.CodeOf(err) != registry.Conflict {
		t.Fatalf("duplicate: code=%v err=%v", registry.CodeOf(err), err)
	}
	// Batch Add is all-or-nothing.
	err = r.Add(echoCap("c.d"), echoCap("a.b"))
	if err == nil {
		t.Fatal("batch with duplicate succeeded")
	}
	if _, ok := r.Get("c.d"); ok {
		t.Fatal("partial batch registered")
	}
}

func TestVersions(t *testing.T) {
	r := registry.New()
	defer r.Close()
	v := func(ver int, out string) registry.Capability {
		return registry.Capability{Name: "x.y", Version: ver, Input: &jsonschema.Schema{},
			Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
				return map[string]string{"v": out}, nil
			}}
	}
	mustAdd(t, r, v(1, "one"), v(2, "two"))
	if err := r.Add(v(0, "zero-is-one")); registry.CodeOf(err) != registry.Conflict {
		t.Fatalf("version 0 should normalize to 1 and conflict, got %v", err)
	}
	out, _ := r.Invoke(context.Background(), "x.y", nil)
	if string(out) != `{"v":"two"}` {
		t.Fatalf("bare name = %s, want current v2", out)
	}
	out, _ = r.Invoke(context.Background(), "x.y@1", nil)
	if string(out) != `{"v":"one"}` {
		t.Fatalf("pinned = %s, want v1", out)
	}
	list := r.List()
	if len(list) != 1 || !list[0].Current || list[0].Version != 2 {
		t.Fatalf("List = %+v", list)
	}
	if len(r.All()) != 2 {
		t.Fatalf("All = %d", len(r.All()))
	}
	r.Remove("x.y@2")
	out, _ = r.Invoke(context.Background(), "x.y", nil)
	if string(out) != `{"v":"one"}` {
		t.Fatalf("after Remove@2 bare = %s, want v1", out)
	}
	r.Remove("x.y")
	if len(r.All()) != 0 {
		t.Fatal("Remove(name) should drop all versions")
	}
	r.Remove("not a ref!", "also@nope") // ignored, must not panic
}

func TestSupersession(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, echoCap("jadx.decompile"), echoCap("apktool.decode"))
	if len(r.List()) != 2 {
		t.Fatalf("initial List=%d", len(r.List()))
	}
	apk := echoCap("apk.decompile")
	apk.Supersedes = []string{"jadx.decompile", "apktool.decode"}
	mustAdd(t, r, apk)
	list := r.List()
	if len(list) != 1 || list[0].Name != "apk.decompile" {
		t.Fatalf("List after supersede = %+v", list)
	}
	e, ok := r.Get("jadx.decompile")
	if !ok || !e.Superseded() || e.SupersededBy[0] != "apk.decompile" {
		t.Fatalf("jadx entry = %+v ok=%v", e, ok)
	}
	// Superseded capabilities still work when called by name.
	out, err := r.Invoke(context.Background(), "jadx.decompile", json.RawMessage(`{"x":1}`))
	if err != nil || string(out) != `{"x":1}` {
		t.Fatalf("superseded invoke = %s %v", out, err)
	}
	// Removing the replacement brings the originals back.
	if err := r.Replace("apk"); err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 2 {
		t.Fatalf("List after unload = %+v", r.List())
	}
	// Pinned supersession hits only that version.
	v2 := echoCap("jadx.decompile")
	v2.Version = 2
	mustAdd(t, r, v2)
	pin := echoCap("z.onlyv1")
	pin.Supersedes = []string{"jadx.decompile@1"}
	mustAdd(t, r, pin)
	e1, _ := r.Get("jadx.decompile@1")
	e2, _ := r.Get("jadx.decompile@2")
	if !e1.Superseded() || e2.Superseded() {
		t.Fatalf("pinned supersede: v1=%v v2=%v", e1.SupersededBy, e2.SupersededBy)
	}
	// Rule naming a not-yet-registered capability takes effect on arrival.
	early := echoCap("z.early")
	early.Supersedes = []string{"future.cap"}
	mustAdd(t, r, early, echoCap("future.cap"))
	if e, _ := r.Get("future.cap"); !e.Superseded() {
		t.Fatal("early supersession rule did not apply")
	}
}

func TestSupersessionCycleRejected(t *testing.T) {
	r := registry.New()
	defer r.Close()
	a := echoCap("c.a")
	a.Supersedes = []string{"c.b"}
	b := echoCap("c.b")
	b.Supersedes = []string{"c.a"}
	if err := r.Add(a, b); err == nil || registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("cycle Add err = %v", err)
	}
	if len(r.All()) != 0 {
		t.Fatal("cycle Add mutated registry")
	}
	self := echoCap("c.self")
	self.Supersedes = []string{"c.self"}
	if err := r.Add(self); err == nil {
		t.Fatal("self-supersede accepted")
	}
}

func TestReplace(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, echoCap("data.src.a.query"), echoCap("data.src.b.query"), echoCap("storage.x"))
	if err := r.Replace("data.src", echoCap("data.src.c.query")); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range r.All() {
		names[e.Name] = true
	}
	if names["data.src.a.query"] || names["data.src.b.query"] || !names["data.src.c.query"] || !names["storage.x"] {
		t.Fatalf("after Replace: %v", names)
	}
	if err := r.Replace("data.src", echoCap("outside.prefix")); err == nil {
		t.Fatal("Replace accepted cap outside prefix")
	}
	if err := r.Replace("Bad Prefix"); err == nil {
		t.Fatal("Replace accepted bad prefix")
	}
	// Failed Replace leaves state alone.
	bad := echoCap("data.src.d.query")
	bad.Handler = nil
	if err := r.Replace("data.src", bad); err == nil {
		t.Fatal("Replace with bad cap accepted")
	}
	if _, ok := r.Get("data.src.c.query"); !ok {
		t.Fatal("failed Replace mutated registry")
	}
}

func TestReplaceDuringInvokeKeepsInFlight(t *testing.T) {
	r := registry.New()
	defer r.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	slow := registry.Capability{Name: "p.slow", Input: &jsonschema.Schema{},
		Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
			close(started)
			<-release
			return map[string]string{"gen": "old"}, nil
		}}
	mustAdd(t, r, slow)
	job, err := r.Start(context.Background(), "p.slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	fast := registry.Capability{Name: "p.slow", Input: &jsonschema.Schema{},
		Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
			return map[string]string{"gen": "new"}, nil
		}}
	if err := r.Replace("p", fast); err != nil {
		t.Fatal(err)
	}
	close(release)
	out, err := job.Wait(context.Background())
	if err != nil || string(out) != `{"gen":"old"}` {
		t.Fatalf("in-flight after Replace = %s %v", out, err)
	}
	out, _ = r.Invoke(context.Background(), "p.slow", nil)
	if string(out) != `{"gen":"new"}` {
		t.Fatalf("post-Replace = %s", out)
	}
}

func TestConcurrentReplaceAndInvoke(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, addCap())
	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for k := 0; k < 50; k++ {
				if err := r.Replace("dyn", echoCap(fmt.Sprintf("dyn.c%d", n))); err != nil {
					errCh <- err
					return
				}
				if _, err := r.Invoke(context.Background(), "math.add", json.RawMessage(`{"a":1,"b":2}`)); err != nil {
					errCh <- err
					return
				}
				_ = r.List()
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestWatchCoalescing(t *testing.T) {
	r := registry.New()
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := r.Watch(ctx)
	mustAdd(t, r, echoCap("w.a"))
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not fire on Add")
	}
	// A burst without a receiver must leave at most a couple of pending fires.
	for i := 0; i < 20; i++ {
		mustAdd(t, r, echoCap(fmt.Sprintf("w.c%d", i)))
	}
	time.Sleep(50 * time.Millisecond)
	fires := 0
	for {
		select {
		case <-ch:
			fires++
			if fires > 3 {
				t.Fatalf("Watch not coalesced: %d fires", fires)
			}
		case <-time.After(100 * time.Millisecond):
			goto done
		}
	}
done:
	if fires == 0 {
		t.Fatal("expected at least one coalesced fire")
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Watch channel not closed on ctx cancel")
		}
	}
}

func TestFiltersAndViews(t *testing.T) {
	r := registry.New()
	defer r.Close()
	ro := echoCap("storage.artifact.get")
	ro.Effects = registry.ReadOnly
	ro.Tags = []string{"agent"}
	admin := echoCap("storage.artifact.delete")
	admin.Effects = registry.Destructive
	admin.Tags = []string{"admin"}
	mustAdd(t, r, ro, admin, addCap())

	agents := r.View(registry.And(registry.Match("storage.*", "math.*"), registry.Not(registry.Tagged("admin"))))
	if len(agents.List()) != 2 {
		t.Fatalf("agent List=%+v", agents.List())
	}
	// Outside the view = not_found even though it exists.
	_, err := agents.Invoke(context.Background(), "storage.artifact.delete", nil)
	if registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("view enforcement: %v", err)
	}
	if _, ok := agents.Get("storage.artifact.delete"); ok {
		t.Fatal("Get leaked cap outside view")
	}
	if !registry.ReadOnlyOnly(ro) || registry.ReadOnlyOnly(admin) {
		t.Fatal("ReadOnlyOnly wrong")
	}
	if got := registry.Select([]registry.Capability{ro, admin}, registry.Tagged("agent")); len(got) != 1 {
		t.Fatalf("Select=%v", got)
	}
	if got := r.View(registry.Or(registry.Match("math.*"), registry.Tagged("admin"))).List(); len(got) != 2 {
		t.Fatalf("Or List=%v", got)
	}
	if registry.ToolName("storage.artifact.get") != "storage_artifact_get" ||
		registry.FromToolName("storage_artifact_get") != "storage.artifact.get" {
		t.Fatal("tool name round-trip broken")
	}
	if !registry.ValidName("a-b.c1") || registry.ValidName("a_b") || registry.ValidName("") {
		t.Fatal("ValidName wrong")
	}
}

var errSentinel = errors.New("sentinel")

func TestErrorsAndCodes(t *testing.T) {
	codes := registry.Codes{errSentinel: registry.Conflict}
	w := codes.Wrap(fmt.Errorf("outer: %w", errSentinel))
	if registry.CodeOf(w) != registry.Conflict || !errors.Is(w, errSentinel) {
		t.Fatalf("Codes.Wrap = %v (%v)", w, registry.CodeOf(w))
	}
	if codes.Wrap(nil) != nil {
		t.Fatal("Wrap(nil) != nil")
	}
	plain := errors.New("plain")
	if codes.Wrap(plain) != plain || registry.CodeOf(plain) != registry.Internal {
		t.Fatal("unmatched sentinel should pass through as internal")
	}
	if registry.CodeOf(context.Canceled) != registry.Cancelled {
		t.Fatal("ctx cancel should map to cancelled")
	}
	e := registry.Errorf(registry.Denied, "no %s", "way")
	if e.Error() != "denied: no way" || registry.Info(e).Code != registry.Denied {
		t.Fatalf("Errorf=%v info=%+v", e, registry.Info(e))
	}
}

// Codes.Wrap is deterministic when an error matches more than one sentinel:
// agreeing codes apply, disagreeing ones are an ambiguous table and give
// internal, every time, whatever the map's iteration order.
func TestCodesWrapIsDeterministic(t *testing.T) {
	errA, errB, errC := errors.New("a"), errors.New("b"), errors.New("c")
	codes := registry.Codes{errA: registry.NotFound, errB: registry.Conflict, errC: registry.NotFound}
	agree := errors.Join(errA, errC)
	clash := errors.Join(errA, errB)
	for i := 0; i < 200; i++ {
		if w := codes.Wrap(agree); registry.CodeOf(w) != registry.NotFound {
			t.Fatalf("iteration %d: agreeing sentinels gave %v", i, w)
		}
		w := codes.Wrap(clash)
		if registry.CodeOf(w) != registry.Internal || !errors.Is(w, errA) || !errors.Is(w, errB) {
			t.Fatalf("iteration %d: clashing sentinels gave %v (%s)", i, w, registry.CodeOf(w))
		}
		if want := "internal: registry.Codes: error matches sentinels with different codes (conflict, not_found): a\nb"; w.Error() != want {
			t.Fatalf("iteration %d: clash message = %q, want %q", i, w.Error(), want)
		}
	}
}

func TestMiddlewareOrderAndCaller(t *testing.T) {
	var order []string
	mw := func(tag string) registry.Middleware {
		return func(next registry.Handler, c registry.Capability) registry.Handler {
			return func(ctx context.Context, in json.RawMessage) (any, error) {
				order = append(order, tag+"-in")
				out, err := next(ctx, in)
				order = append(order, tag+"-out")
				return out, err
			}
		}
	}
	r := registry.New(registry.Use(mw("audit"), mw("policy")))
	defer r.Close()
	var gotCaller registry.Caller
	var hasCaller bool
	c := echoCap("m.x")
	inner := c.Handler
	c.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		gotCaller, hasCaller = registry.CallerFrom(ctx)
		if _, ok := registry.JobIDFrom(ctx); !ok {
			t.Error("handler ctx missing job id")
		}
		if registry.ViewFrom(ctx) == nil {
			t.Error("handler ctx missing view")
		}
		return inner(ctx, in)
	}
	mustAdd(t, r, c)
	ctx := registry.WithCaller(context.Background(), registry.Caller{ID: "agent:t-7", Kind: "agent", Session: "s1"})
	if _, err := r.Invoke(ctx, "m.x", nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"audit-in", "policy-in", "policy-out", "audit-out"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("middleware order=%v", order)
	}
	if !hasCaller || gotCaller.ID != "agent:t-7" || gotCaller.Session != "s1" {
		t.Fatalf("caller=%+v has=%v", gotCaller, hasCaller)
	}
}

func TestNestedCallClampedToCallerView(t *testing.T) {
	r := registry.New()
	defer r.Close()
	secret := echoCap("vault.read")
	secret.Tags = []string{"admin"}
	outer := registry.Capability{Name: "wrap.go", Input: &jsonschema.Schema{}, Tags: []string{"agent"},
		Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
			// Even with a handle on the wide registry, the ctx view clamps.
			return r.Invoke(ctx, "vault.read", in)
		}}
	mustAdd(t, r, secret, outer)
	narrow := r.View(registry.Not(registry.Tagged("admin")))
	_, err := narrow.Invoke(context.Background(), "wrap.go", json.RawMessage(`{}`))
	if registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("nested escalation not clamped: %v", err)
	}
	// The same call through the wide view works.
	if _, err := r.Invoke(context.Background(), "wrap.go", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("wide nested call: %v", err)
	}
}

func TestPanicRecoveryAndOutputValidation(t *testing.T) {
	r := registry.New(registry.ValidateOutput(true))
	defer r.Close()
	panicky := echoCap("p.boom")
	panicky.Handler = func(ctx context.Context, in json.RawMessage) (any, error) { panic("kaboom") }
	badOut := registry.Func("p.badout", "lies about output",
		func(ctx context.Context, in struct{}) (addOut, error) { return addOut{1}, nil })
	// Replace handler post-hoc to return a wrong-shaped value.
	badOut.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		return map[string]string{"sum": "not-a-number"}, nil
	}
	mustAdd(t, r, panicky, badOut)
	if _, err := r.Invoke(context.Background(), "p.boom", nil); registry.CodeOf(err) != registry.Internal {
		t.Fatalf("panic: %v", err)
	}
	if _, err := r.Invoke(context.Background(), "p.badout", nil); registry.CodeOf(err) != registry.Internal {
		t.Fatalf("output validation: %v", err)
	}
	// Registry still healthy.
	mustAdd(t, r, addCap())
	if _, err := r.Invoke(context.Background(), "math.add", json.RawMessage(`{"a":1,"b":1}`)); err != nil {
		t.Fatal(err)
	}
}

func TestNoInputCapability(t *testing.T) {
	r := registry.New()
	defer r.Close()
	c := registry.Capability{Name: "n.none", Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
		return "ok", nil
	}}
	mustAdd(t, r, c)
	out, err := r.Invoke(context.Background(), "n.none", nil)
	if err != nil || string(out) != `"ok"` {
		t.Fatalf("no-input invoke=%s %v", out, err)
	}
	if _, err := r.Invoke(context.Background(), "n.none", json.RawMessage(`{"surprise":1}`)); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("no-input with args: %v", err)
	}
}

func TestPagination(t *testing.T) {
	items := []int{1, 2, 3, 4, 5, 6, 7}
	seen := map[int]int{}
	cursor := ""
	pages := 0
	for {
		p, err := registry.Paginate(items, registry.PageIn{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, it := range p.Items {
			seen[it]++
		}
		if p.Next == "" {
			break
		}
		cursor = p.Next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 || len(seen) != 7 {
		t.Fatalf("pages=%d seen=%v", pages, seen)
	}
	for _, n := range seen {
		if n != 1 {
			t.Fatalf("item visited %d times", n)
		}
	}
	if _, err := registry.Paginate(items, registry.PageIn{Cursor: "bogus!!"}); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("bad cursor: %v", err)
	}
	if registry.NormalizeLimit(0) != 50 || registry.NormalizeLimit(9999) != 500 || registry.NormalizeLimit(10) != 10 {
		t.Fatal("NormalizeLimit wrong")
	}
	empty, _ := registry.Paginate([]int{}, registry.PageIn{})
	if empty.Items == nil {
		t.Fatal("empty page Items must be non-nil for JSON []")
	}
}
