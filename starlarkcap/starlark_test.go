package starlarkcap_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sl "go.starlark.net/starlark"

	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

type echoIn struct {
	ID string `json:"id"`
}

func baseRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r := registry.New()
	t.Cleanup(r.Close)
	caps := []registry.Capability{
		registry.Func("storage.artifact.get", "Get", func(ctx context.Context, in echoIn) (map[string]string, error) {
			return map[string]string{"got": in.ID + "@v1"}, nil
		}, registry.WithVersion(1), registry.WithEffects(registry.ReadOnly)),
		registry.Func("storage.artifact.get", "Get", func(ctx context.Context, in echoIn) (map[string]string, error) {
			return map[string]string{"got": in.ID}, nil
		}, registry.WithVersion(2), registry.WithEffects(registry.ReadOnly)),
		registry.Func("vault.secret.read", "Secret", func(ctx context.Context, in echoIn) (echoIn, error) {
			return echoIn{ID: "topsecret"}, nil
		}, registry.WithTags("admin")),
		registry.Func("slow.work", "Slow", func(ctx context.Context, in echoIn) (echoIn, error) {
			registry.Progress(ctx, registry.Update{Stage: "run", Done: 1, Total: 1, Unit: "files"})
			return echoIn{ID: "done-" + in.ID}, nil
		}, registry.AsLong(time.Second)),
	}
	caps = append(caps, jobs.Capabilities()...)
	if err := r.Add(caps...); err != nil {
		t.Fatal(err)
	}
	return r
}

// load is Load of an in-memory file with no modules.
func load(r *registry.Registry, prefix, filename, src string, opts starlarkcap.LoadOptions) (starlarkcap.Loaded, error) {
	return starlarkcap.Load(context.Background(), r, prefix, starlarkcap.Source{Filename: filename, Src: []byte(src)}, opts)
}

func runScript(t *testing.T, ctx context.Context, src string) (sl.StringDict, string, error) {
	t.Helper()
	var prints []string
	thread := &sl.Thread{Name: "test", Print: func(th *sl.Thread, msg string) { prints = append(prints, msg) }}
	thread.SetMaxExecutionSteps(1_000_000)
	globals := starlarkcap.Globals(ctx)
	out, err := sl.ExecFile(thread, "test.star", src, globals)
	return out, strings.Join(prints, "\n"), err
}

func strVal(t *testing.T, globals sl.StringDict, name string) string {
	t.Helper()
	v, ok := globals[name].(sl.String)
	if !ok {
		t.Fatalf("%s is not a string: %v (%T)", name, globals[name], globals[name])
	}
	return string(v)
}

func TestGlobalsCallKwargsAndDicts(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	out, _, err := runScript(t, ctx, `
res = storage.artifact.get(id = "a1")
got = res["got"]
res2 = storage.artifact.get({"id": "b2"})
got2 = res2["got"]
pinned = call("storage.artifact.get@1", id = "c3")["got"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if strVal(t, out, "got") != "a1" || strVal(t, out, "got2") != "b2" || strVal(t, out, "pinned") != "c3@v1" {
		t.Fatalf("got=%q got2=%q pinned=%q", strVal(t, out, "got"), strVal(t, out, "got2"), strVal(t, out, "pinned"))
	}
	// Error codes cross as messages.
	_, _, err = runScript(t, ctx, `x = storage.artifact.get(id = 42)`)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("bad input err=%v", err)
	}
	_, _, err = runScript(t, ctx, `x = call("no.such.cap")`)
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown err=%v", err)
	}
	// No view in ctx: builtins exist? Globals returns only call/jobs; a
	// direct namespace reference fails resolution, call() reports unavailable.
	_, _, err = runScript(t, context.Background(), `x = call("storage.artifact.get", id="a")`)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("no-view err=%v", err)
	}
}

func TestGlobalsViewIsWholeWorld(t *testing.T) {
	r := baseRegistry(t)
	agents := r.View(registry.Not(registry.Tagged("admin")))
	ctx := registry.WithView(context.Background(), agents)
	// The admin namespace is not even visible.
	_, _, err := runScript(t, ctx, `x = vault.secret.read(id="a")`)
	if err == nil {
		t.Fatal("admin cap visible in narrowed Globals")
	}
	_, _, err = runScript(t, ctx, `x = call("vault.secret.read", id="a")`)
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("admin via call err=%v", err)
	}
}

func TestGlobalsJobsStartWaitCancel(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	out, _, err := runScript(t, ctx, `
job = jobs.start("slow.work", id = "x1")
jid = job.id
w = job.wait(timeout_seconds = 5)
state = w["status"]["state"]
res = w["result"]["id"]
st = job.status()["state"]
# Long caps also block when called directly:
direct = slow.work(id = "y2")["id"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if strVal(t, out, "state") != "succeeded" || strVal(t, out, "res") != "done-x1" || strVal(t, out, "direct") != "done-y2" {
		t.Fatalf("out state=%q res=%q direct=%q jid=%q st=%q", strVal(t, out, "state"), strVal(t, out, "res"), strVal(t, out, "direct"), strVal(t, out, "jid"), strVal(t, out, "st"))
	}
}

func TestGlobalsSupersededWarning(t *testing.T) {
	r := baseRegistry(t)
	repl := registry.Func("new.get", "replacement",
		func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
		registry.WithSupersedes("storage.artifact.get"))
	if err := r.Add(repl); err != nil {
		t.Fatal(err)
	}
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	out, prints, err := runScript(t, ctx, `got = call("storage.artifact.get@1", id = "q")["got"]`)
	if err != nil {
		t.Fatal(err)
	}
	if strVal(t, out, "got") != "q@v1" {
		t.Fatalf("got=%q", strVal(t, out, "got"))
	}
	if !strings.Contains(prints, "warning: storage.artifact.get is superseded by new.get") {
		t.Fatalf("prints=%q", prints)
	}
}

func TestLoadDefinesAndSupersedes(t *testing.T) {
	r := baseRegistry(t)
	src := `
def _decompile(ctx, input):
    ctx.progress(stage = "run", done = 1, total = 2, unit = "files", data = {"tool": "fake"})
    got = ctx.call("storage.artifact.get", id = input["artifact"])
    ctx.progress(stage = "finalise")
    return {"sources": got["got"]}

capability(
    name = "apk.decompile",
    title = "Decompile an APK",
    description = "Decompile an APK to sources in an isolated container",
    supersedes = ["storage.artifact.get"],
    long = True,
    heartbeat = 5,
    effects = ["open_world"],
    tags = ["agent"],
    input = {"artifact": schema.string(required = True, description = "artifact id")},
    output = {"sources": schema.string(required = True)},
    run = _decompile,
)
`
	// Superseding outside the prefix is policy: allow storage.* here.
	if _, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{MaySupersede: registry.Match("storage.*")}); err != nil {
		t.Fatal(err)
	}
	e, ok := r.Get("apk.decompile")
	if !ok || !e.Long || !e.Effects.Has(registry.OpenWorld) || !e.HasTag("agent") || e.Heartbeat != 5*time.Second {
		t.Fatalf("entry=%+v", e)
	}
	b, _ := json.Marshal(e.Input)
	if !strings.Contains(string(b), `"required":["artifact"]`) || !strings.Contains(string(b), "artifact id") {
		t.Fatalf("script input schema=%s", b)
	}
	// Superseded Go cap drops out of List but the script cap can still call
	// it through the caller's (wide) view.
	for _, le := range r.List() {
		if le.Name == "storage.artifact.get" {
			t.Fatal("superseded Go cap still in List")
		}
	}
	job, err := r.Start(context.Background(), "apk.decompile", json.RawMessage(`{"artifact":"app.apk"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := job.Wait(context.Background())
	if err != nil || string(out) != `{"sources":"app.apk"}` {
		t.Fatalf("script job=%s %v", out, err)
	}
	var sawData bool
	for ev := range job.Events(context.Background()) {
		if ev.Update != nil && ev.Update.Stage == "run" {
			if m, ok := ev.Update.Data.(map[string]any); ok && m["tool"] == "fake" {
				sawData = true
			}
		}
	}
	if !sawData {
		t.Fatal("ctx.progress data not carried")
	}
	// Visible via Globals immediately (script caps visible by default).
	// The view must also include storage.*: the script's nested call runs
	// through the caller's view and cannot widen it.
	ctx := registry.WithView(context.Background(), r.View(registry.Match("apk.*", "storage.*")))
	gout, _, err := runScript(t, ctx, `s = apk.decompile(artifact = "z.apk")["sources"]`)
	if err != nil {
		t.Fatal(err)
	}
	if strVal(t, gout, "s") != "z.apk" {
		t.Fatalf("globals script cap=%q", strVal(t, gout, "s"))
	}
	// Reload is Replace: an updated file swaps the cap; unload restores.
	src2 := `
def _v2(ctx, input):
    return {"sources": "v2-" + input["artifact"]}
capability(name = "apk.decompile", version = 2, long = True,
           input = {"artifact": schema.string(required = True)}, run = _v2)
`
	if _, err := load(r, "apk", "apk.star", src2, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err = r.Invoke(context.Background(), "apk.decompile", json.RawMessage(`{"artifact":"a"}`))
	if err != nil || string(out) != `{"sources":"v2-a"}` {
		t.Fatalf("reloaded=%s %v", out, err)
	}
	if err := r.Replace("apk"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, le := range r.List() {
		if le.Name == "storage.artifact.get" {
			found = true
		}
	}
	if !found {
		t.Fatal("unloading script did not restore superseded Go cap")
	}
}

func TestLoadSubjects(t *testing.T) {
	r := baseRegistry(t)
	src := `
capability(
    name = "apk.decompile",
    accepts = ["android.apk", {"type": "archive.zip", "level": "validated"}],
    emits = [{"type": "text.java"}],
    run = lambda ctx, i: {},
)
`
	if _, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	e, ok := r.Get("apk.decompile")
	if !ok || len(e.Accepts) != 2 || e.Accepts[0].Type != "android.apk" || e.Accepts[0].Level != "" {
		t.Fatalf("accepts = %+v ok=%v", e.Accepts, ok)
	}
	if e.Accepts[1].Type != "archive.zip" || e.Accepts[1].Level != "validated" {
		t.Fatalf("second accept = %+v", e.Accepts[1])
	}
	if len(e.Emits) != 1 || e.Emits[0].Type != "text.java" {
		t.Fatalf("emits = %+v", e.Emits)
	}
	bad := `capability(name = "apk.bad", accepts = [{"level": "validated"}], run = lambda ctx, i: {})`
	if _, err := load(r, "apk", "bad.star", bad, starlarkcap.LoadOptions{}); err == nil {
		t.Fatal("accept without type was registered")
	}
}

func TestLoadNoPrivilegeEscalation(t *testing.T) {
	r := baseRegistry(t)
	src := `
def _summary(ctx, input):
    s = ctx.call("vault.secret.read", id = input["artifact"])
    return {"leaked": s["id"]}
capability(name = "apk.summary", description = "summary",
           input = {"artifact": schema.string(required = True)}, run = _summary)
`
	if _, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	// A read-only agent view (no admin) calls the script cap: its nested
	// call to the admin cap must fail.
	agents := r.View(registry.Not(registry.Tagged("admin")))
	_, err := agents.Invoke(context.Background(), "apk.summary", json.RawMessage(`{"artifact":"a"}`))
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("escalation not blocked: %v", err)
	}
	// A wide caller gets through (the cap itself works).
	out, err := r.Invoke(context.Background(), "apk.summary", json.RawMessage(`{"artifact":"a"}`))
	if err != nil || string(out) != `{"leaked":"topsecret"}` {
		t.Fatalf("wide caller=%s %v", out, err)
	}
}

func TestLoadFailuresLeaveRegistryUnchanged(t *testing.T) {
	r := baseRegistry(t)
	before := len(r.All())
	cases := map[string]string{
		"syntax":    `def broken(`,
		"no-name":   `capability(run = lambda ctx, i: {})`,
		"no-run":    `capability(name = "apk.x")`,
		"outside":   `capability(name = "other.x", run = lambda ctx, i: {})`,
		"badschema": `capability(name = "apk.x", input = "{not json", run = lambda ctx, i: {})`,
		"badeffect": `capability(name = "apk.x", effects = ["explode"], run = lambda ctx, i: {})`,
	}
	for name, src := range cases {
		if _, err := load(r, "apk", name+".star", src, starlarkcap.LoadOptions{}); err == nil {
			t.Fatalf("%s: Load succeeded, want error", name)
		}
		if len(r.All()) != before {
			t.Fatalf("%s: failed Load mutated registry", name)
		}
	}
}

func TestLoadStepAndTimeLimitsAndCheck(t *testing.T) {
	r := baseRegistry(t)
	src := `
def _spin(ctx, input):
    x = 0
    for i in range(100000000):
        x += i
    return {"x": x}
capability(name = "apk.spin", input = "{}", run = _spin)

def _down(ctx):
    return "container runtime missing"
capability(name = "apk.down", input = "{}", run = lambda ctx, i: {}, check = _down)
`
	lim := starlarkcap.DefaultLimits
	lim.MaxSteps = 100_000
	if _, err := load(r, "apk", "spin.star", src, starlarkcap.LoadOptions{Limits: lim}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := r.Invoke(context.Background(), "apk.spin", json.RawMessage(`{}`))
	if err == nil || time.Since(start) > 10*time.Second {
		// Note: input "{}" is a raw JSON schema string (empty schema=any object ok)
		t.Fatalf("spin not step-limited: err=%v elapsed=%v", err, time.Since(start))
	}
	e, _ := r.Get("apk.down")
	if e.Ready || e.CheckError != "container runtime missing" {
		t.Fatalf("script check entry=%+v", e)
	}
	if _, err := r.Invoke(context.Background(), "apk.down", nil); registry.CodeOf(err) != registry.Unavailable {
		t.Fatalf("script check invoke=%v", err)
	}
}

func TestLoadRawAndDictSchemas(t *testing.T) {
	r := baseRegistry(t)
	src := `
capability(name = "apk.raw",
           input = {"type": "object", "properties": {"n": {"type": "integer", "minimum": 2}}, "required": ["n"]},
           run = lambda ctx, i: {"double": i["n"] * 2})
capability(name = "apk.obj",
           input = schema.object({"cfg": schema.object({"level": schema.integer(required = True)}, required = True)}),
           run = lambda ctx, i: {"level": i["cfg"]["level"]})
`
	if _, err := load(r, "apk", "schemas.star", src, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := r.Invoke(context.Background(), "apk.raw", json.RawMessage(`{"n":21}`))
	if err != nil || string(out) != `{"double":42}` {
		t.Fatalf("raw=%s %v", out, err)
	}
	if _, err := r.Invoke(context.Background(), "apk.raw", json.RawMessage(`{"n":1}`)); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("raw minimum validation: %v", err)
	}
	out, err = r.Invoke(context.Background(), "apk.obj", json.RawMessage(`{"cfg":{"level":3}}`))
	if err != nil || string(out) != `{"level":3}` {
		t.Fatalf("obj=%s %v", out, err)
	}
	if _, err := r.Invoke(context.Background(), "apk.obj", json.RawMessage(`{}`)); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("obj required validation: %v", err)
	}
}
