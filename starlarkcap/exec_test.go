package starlarkcap_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sl "go.starlark.net/starlark"

	"github.com/craggy-io/script-services/modules"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

func TestExecRunsBodyThenEntryWithHooks(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.Not(registry.Tagged("admin"))))
	var mu sync.Mutex
	var prints []string
	var calls []starlarkcap.CallEvent
	hooks := starlarkcap.Hooks{
		Print: func(pos starlarkcap.Position, msg string) {
			mu.Lock()
			defer mu.Unlock()
			prints = append(prints, msg+"@"+strconv.Itoa(pos.Line))
		},
		Call: func(ev starlarkcap.CallEvent) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, ev)
		},
	}
	src := `
seen = []
for k in ["a", "b"]:
    seen.append(storage.artifact.get(id = k)["got"])
print("seen", seen)

def run(input):
    return {"seen": seen, "who": input["who"], "host": host_value()}
`
	host := sl.StringDict{"host_value": sl.NewBuiltin("host_value", func(*sl.Thread, *sl.Builtin, sl.Tuple, []sl.Tuple) (sl.Value, error) {
		return sl.String("h"), nil
	})}
	res, err := starlarkcap.Exec(ctx, starlarkcap.Program{
		Filename: "nb.star", Source: []byte(src), Input: map[string]any{"who": "me"}, Predeclared: host,
	}, starlarkcap.Limits{MaxSteps: 100_000, Timeout: 5 * time.Second}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Value, &out); err != nil {
		t.Fatal(err)
	}
	if out["who"] != "me" || out["host"] != "h" || len(out["seen"].([]any)) != 2 {
		t.Fatalf("result = %s", res.Value)
	}
	if res.Steps == 0 {
		t.Fatal("no steps counted")
	}
	if len(prints) != 1 || prints[0] != `seen ["a", "b"]@5` {
		t.Fatalf("prints = %q", prints)
	}
	if len(calls) != 4 || calls[0].Done || !calls[1].Done || calls[1].Pos.Line != 4 || calls[1].JobID == "" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestExecErrorsCarryPositionAndCode(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.Not(registry.Tagged("admin"))))
	lim := starlarkcap.Limits{MaxSteps: 10_000, Timeout: 5 * time.Second}
	cases := []struct {
		name, src string
		code      registry.Code
		line      int
		msg       string
	}{
		{"syntax", "x = (\n", registry.Invalid, 2, ""},
		{"undefined", "x = 1\ny = nope\n", registry.Invalid, 2, "undefined: nope"},
		{"runtime", "def f():\n    return 1 // 0\nf()\n", registry.Invalid, 2, "division by zero"},
		{"capability", "vault.secret.read(id = 'x')\n", registry.Invalid, 1, "undefined: vault"},
		{"not found", "call('nope.gone')\n", registry.NotFound, 1, "not found"},
		{"steps", "n = 0\nwhile True:\n    n += 1\n", registry.Invalid, 0, "too many steps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := starlarkcap.Exec(ctx, starlarkcap.Program{Filename: "e.star", Source: []byte(tc.src)}, lim, starlarkcap.Hooks{})
			se, ok := err.(*starlarkcap.Error)
			if !ok {
				t.Fatalf("err = %v (%T)", err, err)
			}
			if se.Code != tc.code || registry.CodeOf(err) != tc.code {
				t.Fatalf("code = %s, want %s (%v)", se.Code, tc.code, err)
			}
			if tc.line > 0 && se.Pos.Line != tc.line {
				t.Fatalf("line = %d, want %d (%v)", se.Pos.Line, tc.line, err)
			}
			if !strings.Contains(se.Message, tc.msg) {
				t.Fatalf("message %q lacks %q", se.Message, tc.msg)
			}
		})
	}
}

func TestExecHostNameClashIsInvalid(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	_, err := starlarkcap.Exec(ctx, starlarkcap.Program{Source: []byte("x = 1"), Predeclared: sl.StringDict{"storage": sl.None}},
		starlarkcap.Limits{}, starlarkcap.Hooks{})
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("err = %v", err)
	}
}

// A cancelled script cancels the jobs it started, including the one it is
// waiting on and background jobs from jobs.start.
func TestExecCancelStopsStartedJobs(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	started := make(chan *registry.Job, 4)
	if err := r.Add(registry.Func("work.block", "Block",
		func(ctx context.Context, _ struct{}) (struct{}, error) {
			<-ctx.Done()
			return struct{}{}, ctx.Err()
		}, registry.AsLong(time.Second))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(registry.WithView(context.Background(), r.View(registry.All)))
	done := make(chan error, 1)
	go func() {
		_, err := starlarkcap.Exec(ctx, starlarkcap.Program{Source: []byte(`
bg = jobs.start("work.block")
work.block()
`)}, starlarkcap.Limits{MaxSteps: 10_000}, starlarkcap.Hooks{Call: func(ev starlarkcap.CallEvent) {
			if ev.JobID != "" && !ev.Done {
				if j, ok := r.View(registry.All).Job(ev.JobID); ok {
					started <- j
				}
			}
		}})
		done <- err
	}()
	waited := <-started
	cancel()
	if err := <-done; registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("exec err = %v", err)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	if _, err := waited.Wait(wctx); registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("waited job: %v", err)
	}
	for _, st := range r.View(registry.All).Jobs(registry.JobFilter{Capability: "work.block"}) {
		j, _ := r.View(registry.All).Job(st.ID)
		if _, err := j.Wait(wctx); registry.CodeOf(err) != registry.Cancelled {
			t.Fatalf("job %s: %v", st.ID, err)
		}
	}
}

func TestCheckReportsUnknownCapabilitiesAndArguments(t *testing.T) {
	r := baseRegistry(t)
	v := r.View(registry.Not(registry.Tagged("admin")))
	src := `
a = storage.artifact.gte(id = "x")
b = storage.artifact.get(idd = "x")
c = storage.artifact(id = "x")
d = call("storage.artifact.got", id = "x")
e = jobs.start("slow.work", id = "y")
f = storage.artifact.get({"id": "ok"})
g = helper(missing_name)
def run(input):
    return [x.upper() for x in input["names"]]
`
	diags := starlarkcap.Check(v, "c.star", []byte(src), "helper")
	var got []string
	for _, d := range diags {
		got = append(got, strconv.Itoa(d.Pos.Line)+" "+d.Message)
	}
	want := []string{
		"2 unknown capability storage.artifact.gte (did you mean storage.artifact.get?)",
		`3 storage.artifact.get has no input "idd" (did you mean id?)`,
		`3 storage.artifact.get requires input "id"`,
		"4 storage.artifact is a namespace, not a capability",
		`5 unknown capability "storage.artifact.got" (did you mean storage.artifact.get?)`,
		"8 undefined: missing_name",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("diagnostics:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if d := starlarkcap.Check(v, "ok.star", []byte("x = storage.artifact.get(id = 'a')\n")); len(d) != 0 {
		t.Fatalf("clean script: %+v", d)
	}
}

func TestExecErrorCarriesCapabilityReason(t *testing.T) {
	r := baseRegistry(t)
	if err := r.Add(registry.Func("pool.acquire", "Acquire", func(ctx context.Context, in struct{}) (struct{}, error) {
		return in, registry.WrapReason(registry.Unavailable, registry.ReasonNoCapacity, errors.New("pool full"))
	})); err != nil {
		t.Fatal(err)
	}
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	lim := starlarkcap.Limits{MaxSteps: 10_000, Timeout: 5 * time.Second}
	_, err := starlarkcap.Exec(ctx, starlarkcap.Program{Filename: "r.star", Source: []byte("pool.acquire()\n")}, lim, starlarkcap.Hooks{})
	var se *starlarkcap.Error
	if !errors.As(err, &se) || se.Code != registry.Unavailable || se.Reason != registry.ReasonNoCapacity {
		t.Fatalf("err = %v (%+v)", err, se)
	}
	if registry.CodeOf(err) != registry.Unavailable || registry.ReasonOf(err) != registry.ReasonNoCapacity {
		t.Fatalf("CodeOf/ReasonOf = %s/%q", registry.CodeOf(err), registry.ReasonOf(err))
	}
	if b, _ := json.Marshal(se); !strings.Contains(string(b), `"reason":"no_capacity"`) {
		t.Fatalf("Error JSON lacks reason: %s", b)
	}
}

func TestAnalyzeListsCapabilitiesUsedOncePerRef(t *testing.T) {
	r := baseRegistry(t)
	v := r.View(registry.Not(registry.Tagged("admin")))
	src := `
a = storage.artifact.get(id = "x")
b = jobs.start("slow.work", id = "y")
c = storage.artifact.get(id = "z")
d = call("slow.work", id = "w")
e = storage.artifact.gte(id = "x")
`
	got := starlarkcap.Analyze(v, "u.star", []byte(src))
	var refs []string
	for _, e := range got.Uses {
		refs = append(refs, e.Capability.Ref())
	}
	if want := "storage.artifact.get@2 slow.work@1"; strings.Join(refs, " ") != want {
		t.Fatalf("uses = %v, want %s", refs, want)
	}
	if len(got.Diagnostics) != 1 || !strings.Contains(got.Diagnostics[0].Message, "storage.artifact.gte") {
		t.Fatalf("diagnostics = %+v, want the one unknown capability", got.Diagnostics)
	}
}

func TestExecWithModulesReportsRefsAndDigest(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	loader := modules.NewLoader(modules.Map(map[string]string{
		"lib/fmt.star":  "load('./util.star', 'up')\ndef shout(s):\n    return up(s) + '!'\n",
		"lib/util.star": "def up(s):\n    return s.upper()\n",
	}), modules.Options{})
	src := []byte(`
load("//lib/fmt.star", "shout")
def run(input):
    return shout(storage.artifact.get(id = input)["got"])
`)
	res, err := starlarkcap.Exec(ctx, starlarkcap.Program{Filename: "m.star", Source: src, Input: "abc", Modules: loader},
		starlarkcap.Limits{MaxSteps: 100_000}, starlarkcap.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Value) != `"ABC!"` {
		t.Fatalf("value = %s", res.Value)
	}
	if len(res.Modules) != 2 || res.Modules[0].Path != "lib/fmt.star" || res.Modules[1].Path != "lib/util.star" {
		t.Fatalf("modules = %+v", res.Modules)
	}
	// The digest covers the direct dependency (spec as written, paired with
	// its digest); lib/util.star counts through lib/fmt.star's digest.
	if res.Digest != modules.Digest(src, []modules.Dep{{Spec: "//lib/fmt.star", Digest: res.Modules[0].Digest}}) {
		t.Fatalf("digest = %s", res.Digest)
	}
	// Without a loader, load() fails as an invalid script.
	_, err = starlarkcap.Exec(ctx, starlarkcap.Program{Filename: "m.star", Source: src}, starlarkcap.Limits{}, starlarkcap.Hooks{})
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("no loader: %v", err)
	}
}

func TestExecErrorCodeMapping(t *testing.T) {
	r := baseRegistry(t)
	if err := r.Add(registry.Func("store.missing", "Missing", func(ctx context.Context, in struct{}) (struct{}, error) {
		return in, registry.Errorf(registry.NotFound, "artifact gone")
	}), registry.Func("pool.busy", "Busy", func(ctx context.Context, in struct{}) (struct{}, error) {
		return in, registry.WrapReason(registry.Unavailable, registry.ReasonBackendDown, errors.New("backend down"))
	})); err != nil {
		t.Fatal(err)
	}
	view := registry.WithView(context.Background(), r.View(registry.All))
	cancelled, cancel := context.WithCancel(view)
	cancel()
	cases := []struct {
		name   string
		ctx    context.Context
		src    string
		lim    starlarkcap.Limits
		code   registry.Code
		reason string
		msg    string
	}{
		{"capability not_found", view, "def run():\n    return store.missing()\n", starlarkcap.Limits{}, registry.NotFound, "", "artifact gone"},
		{"capability reason", view, "pool.busy()\n", starlarkcap.Limits{}, registry.Unavailable, registry.ReasonBackendDown, "backend down"},
		{"cancelled", cancelled, "x = 1\n", starlarkcap.Limits{}, registry.Cancelled, "", ""},
		{"timeout is invalid", view, "n = 0\nwhile True:\n    n += 1\n", starlarkcap.Limits{Timeout: 50 * time.Millisecond}, registry.Invalid, "", "script exceeded its time limit"},
		{"step limit is invalid", view, "n = 0\nwhile True:\n    n += 1\n", starlarkcap.Limits{MaxSteps: 1000}, registry.Invalid, "", "too many steps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := starlarkcap.Exec(tc.ctx, starlarkcap.Program{Filename: "c.star", Source: []byte(tc.src)}, tc.lim, starlarkcap.Hooks{})
			var se *starlarkcap.Error
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T)", err, err)
			}
			if se.Code != tc.code || se.Reason != tc.reason || registry.CodeOf(err) != tc.code || registry.ReasonOf(err) != tc.reason {
				t.Fatalf("code/reason = %s/%q, want %s/%q (%v)", se.Code, se.Reason, tc.code, tc.reason, err)
			}
			if !strings.Contains(se.Message, tc.msg) {
				t.Fatalf("message %q lacks %q", se.Message, tc.msg)
			}
			var re *registry.Error
			if !errors.As(err, &re) || re.Code != tc.code {
				t.Fatalf("Unwrap does not yield a *registry.Error with the code: %v", re)
			}
		})
	}
}

func TestExecErrorJSONShape(t *testing.T) {
	se := &starlarkcap.Error{Code: registry.Invalid, Message: "m", Pos: starlarkcap.Position{File: "f.star", Line: 2, Col: 3},
		Backtrace: []starlarkcap.Frame{{Func: "run", Pos: starlarkcap.Position{File: "f.star", Line: 2, Col: 3}}}, Steps: 7}
	b, _ := json.Marshal(se)
	want := `{"code":"invalid","message":"m","pos":{"file":"f.star","line":2,"col":3},"backtrace":[{"func":"run","pos":{"file":"f.star","line":2,"col":3}}],"steps":7}`
	if string(b) != want {
		t.Fatalf("JSON = %s\nwant %s", b, want)
	}
}

func TestAnalyzeWithModulesAndLoads(t *testing.T) {
	r := baseRegistry(t)
	v := r.View(registry.All)
	src := []byte(`
load("//lib/a.star", "f", "nope")
load("./lib/a.star", "f")
x = f(storage.artifact.get(id = "a"))
`)
	a := starlarkcap.AnalyzeWith(v, "s.star", src, starlarkcap.CheckOptions{
		Modules: modules.Map(map[string]string{"lib/a.star": "def f(x):\n    return x\n"}),
	})
	if len(a.Loads) != 1 || a.Loads[0] != "lib/a.star" {
		t.Fatalf("loads = %v", a.Loads)
	}
	if len(a.Uses) != 1 || a.Uses[0].Name != "storage.artifact.get" {
		t.Fatalf("uses = %v", a.Uses)
	}
	if len(a.Diagnostics) != 1 || !strings.Contains(a.Diagnostics[0].Message, "does not define nope") {
		t.Fatalf("diagnostics = %+v", a.Diagnostics)
	}
}

// slowRegistry has slow.block, which takes 2s unless cancelled, and reports
// each job's end on ended.
func slowRegistry(t *testing.T) (*registry.Registry, chan error) {
	t.Helper()
	r := registry.New()
	t.Cleanup(r.Close)
	ended := make(chan error, 4)
	if err := r.Add(registry.Func("slow.block", "Block for 2s", func(ctx context.Context, _ struct{}) (struct{}, error) {
		select {
		case <-ctx.Done():
			ended <- ctx.Err()
			return struct{}{}, ctx.Err()
		case <-time.After(2 * time.Second):
			ended <- nil
			return struct{}{}, nil
		}
	})); err != nil {
		t.Fatal(err)
	}
	return r, ended
}

func wantCancelled(t *testing.T, ended chan error) {
	t.Helper()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the 2s job ran to completion")
		}
	case <-time.After(time.Second):
		t.Fatal("the 2s job was not cancelled")
	}
}

// Limits.Timeout interrupts a script blocked in a capability call, reports
// the time limit (not a cancellation), and cancels the call's job.
func TestExecTimeoutInterruptsCapabilityCall(t *testing.T) {
	r, ended := slowRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	begin := time.Now()
	_, err := starlarkcap.Exec(ctx, starlarkcap.Program{Filename: "t.star", Source: []byte("slow.block()\n")},
		starlarkcap.Limits{Timeout: 100 * time.Millisecond}, starlarkcap.Hooks{})
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("Exec took %v with a 100ms timeout", d)
	}
	se, ok := err.(*starlarkcap.Error)
	if !ok || se.Code != registry.Invalid || se.Message != "script exceeded its time limit" || se.Reason != "" {
		t.Fatalf("err = %#v, want invalid with the time-limit message", err)
	}
	wantCancelled(t, ended)
}

// The same holds inside a script-defined capability: ctx.call is bounded by
// LoadOptions.Limits.Timeout, and the nested job is cancelled.
func TestLoadTimeoutInterruptsCtxCall(t *testing.T) {
	r, ended := slowRegistry(t)
	if _, err := load(r, "a", "a.star", `capability(name = "a.wait", run = lambda ctx, i: ctx.call("slow.block"))`,
		starlarkcap.LoadOptions{Limits: starlarkcap.Limits{MaxSteps: 10_000, Timeout: 100 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	_, err := r.Invoke(context.Background(), "a.wait", nil)
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("Invoke took %v with a 100ms script timeout", d)
	}
	if registry.CodeOf(err) != registry.Internal || !strings.Contains(err.Error(), "script exceeded its time limit") {
		t.Fatalf("err = %v, want internal with the time-limit message", err)
	}
	wantCancelled(t, ended)
}

// Error() names the code once even when the message, a failed call's error
// text, already starts with it; the fields are unchanged.
func TestExecErrorDoesNotRepeatCode(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	_, err := starlarkcap.Exec(ctx, starlarkcap.Program{Filename: "bad.star", Source: []byte(`call("nope.gone")`)}, starlarkcap.Limits{}, starlarkcap.Hooks{})
	se, ok := err.(*starlarkcap.Error)
	if !ok {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if want := `not_found: bad.star:1:5: capability "nope.gone" not found`; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	if se.Code != registry.NotFound || se.Message != `not_found: capability "nope.gone" not found` {
		t.Fatalf("fields changed: %+v", se)
	}
}

// call() resolves against the snapshot taken when the globals were built:
// a capability registered later is not reachable from that script run.
func TestCallResolvesAgainstSnapshot(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	globals := starlarkcap.Globals(ctx)
	if err := r.Add(registry.Func("late.cap", "Late", func(ctx context.Context, in struct{}) (struct{}, error) { return in, nil })); err != nil {
		t.Fatal(err)
	}
	thread := &sl.Thread{Name: "snap"}
	for _, src := range []string{`call("late.cap")`, `call("late.cap@1")`, `jobs.start("late.cap")`} {
		_, err := sl.ExecFile(thread, "snap.star", src, globals)
		if err == nil || !strings.Contains(err.Error(), "not_found") {
			t.Fatalf("%s after the snapshot = %v, want not_found", src, err)
		}
	}
	if _, err := sl.ExecFile(thread, "snap.star", `x = call("storage.artifact.get", id = "a")`, globals); err != nil {
		t.Fatalf("call of a snapshot capability: %v", err)
	}
}
