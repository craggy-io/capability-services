package starlarkcap_test

// Regression tests for the 2026-11-04 review findings in starlarkcap.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

// Unknown kwargs are an error, not silently dropped metadata.
func TestReviewUnknownCapabilityKwargRejected(t *testing.T) {
	r := baseRegistry(t)
	before := len(r.All())
	src := `capability(name = "b.x", tag = ["admin"], run = lambda ctx, i: {})`
	_, err := load(r, "b", "b.star", src, starlarkcap.LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("err = %v, want an error naming the unknown kwarg", err)
	}
	if _, ok := r.Get("b.x"); ok || len(r.All()) != before {
		t.Fatal("capability with unknown kwarg installed")
	}
	if _, err := load(r, "b", "b.star", `capability(name = "b.x", tags = ["admin"], run = lambda ctx, i: {})`, starlarkcap.LoadOptions{}); err != nil {
		t.Fatalf("correct kwarg: %v", err)
	}
}

// The documented capability(name, ...) signature takes the name
// positionally.
func TestReviewCapabilityPositionalName(t *testing.T) {
	r := baseRegistry(t)
	if _, err := load(r, "p", "p.star", `capability("p.x", run = lambda ctx, i: {})`, starlarkcap.LoadOptions{}); err != nil {
		t.Fatalf("positional name: %v", err)
	}
	if _, ok := r.Get("p.x"); !ok {
		t.Fatal("p.x not installed")
	}
}

// A version that overflows int64 is an error, not silently version 1.
func TestReviewVersionOverflowRejected(t *testing.T) {
	r := baseRegistry(t)
	src := `capability(name = "v.x", version = 1180591620717411303424, run = lambda ctx, i: {})`
	if _, err := load(r, "v", "v.star", src, starlarkcap.LoadOptions{}); err == nil {
		t.Fatal("overflow version loaded")
	}
	if _, ok := r.Get("v.x"); ok {
		t.Fatal("overflow version installed as v.x@1")
	}
}

// A check returning a wrong-typed value is not ready.
func TestReviewCheckWrongTypeNotReady(t *testing.T) {
	r := baseRegistry(t)
	src := `capability(name = "c.x", run = lambda ctx, i: {}, check = lambda ctx: [])`
	if _, err := load(r, "c", "c.star", src, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	e, ok := r.Get("c.x")
	if !ok {
		t.Fatal("c.x not installed")
	}
	if e.Ready || !strings.Contains(e.CheckError, "check returned") {
		t.Fatalf("ready=%v checkError=%q", e.Ready, e.CheckError)
	}
	if _, err := r.Invoke(context.Background(), "c.x", nil); registry.ReasonOf(err) != registry.ReasonNotReady {
		t.Fatalf("Invoke err = %v, want not_ready", err)
	}
}

// An outside-prefix supersession target that is not registered yet fails
// closed: the policy cannot judge a capability it cannot see, and letting
// the supersession lie dormant would spring it on whatever registers
// under that name later.
func TestReviewSupersedeUnregisteredTargetDenied(t *testing.T) {
	r := baseRegistry(t)
	src := `capability(name = "apk.t", supersedes = ["future.thing"], run = lambda ctx, i: {})`
	_, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{MaySupersede: registry.Not(registry.Tagged("admin"))})
	if registry.CodeOf(err) != registry.Denied {
		t.Fatalf("err = %v, want denied", err)
	}
	if _, ok := r.Get("apk.t"); ok {
		t.Fatal("capability installed despite unregistered target")
	}
}

// The policy is checked at install time, not while the file compiles: a
// target version registered while the file's top level is still running
// must pass it too.
func TestReviewMaySupersedeCheckedAtInstall(t *testing.T) {
	r := baseRegistry(t)
	mk := func(v int, tags ...string) registry.Capability {
		return registry.Func("x.tool", "Tool", func(ctx context.Context, in struct{}) (struct{}, error) { return in, nil },
			append([]registry.Option{registry.WithVersion(v)}, registry.WithTags(tags...))...)
	}
	if err := r.Add(mk(1), mk(2)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	opts := starlarkcap.LoadOptions{
		MaySupersede: registry.Not(registry.Tagged("admin")),
		Print: func(pos starlarkcap.Position, msg string) {
			if msg == "gate" {
				once.Do(func() { close(entered) })
				<-release
			}
		},
	}
	src := `
capability(name = "apk.t", supersedes = ["x.tool"], run = lambda ctx, i: {})
print("gate")
`
	done := make(chan error, 1)
	go func() {
		_, err := load(r, "apk", "apk.star", src, opts)
		done <- err
	}()
	<-entered
	if err := r.Add(mk(3, "admin")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; registry.CodeOf(err) != registry.Denied {
		t.Fatalf("load err = %v, want denied by the install-time check", err)
	}
	if _, ok := r.Get("apk.t"); ok {
		t.Fatal("apk.t installed despite failing the recheck")
	}
	if e, _ := r.Get("x.tool@3"); e.Superseded() {
		t.Fatal("x.tool@3 superseded without passing the policy")
	}
}

// A capability that is also a namespace prefix stays callable: a.b() and
// a.b.c() both work.
func TestReviewGlobalsPrefixCapabilityCallable(t *testing.T) {
	r := baseRegistry(t)
	if err := r.Add(
		registry.Func("a.b", "B", func(ctx context.Context, in echoIn) (echoIn, error) { return echoIn{ID: "b:" + in.ID}, nil }),
		registry.Func("a.b.c", "C", func(ctx context.Context, in echoIn) (echoIn, error) { return echoIn{ID: "c:" + in.ID}, nil }),
	); err != nil {
		t.Fatal(err)
	}
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	out, _, err := runScript(t, ctx, `
x = a.b(id = "1")["id"]
y = a.b.c(id = "2")["id"]
`)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	if strVal(t, out, "x") != "b:1" || strVal(t, out, "y") != "c:2" {
		t.Fatalf("x=%q y=%q", out["x"], out["y"])
	}
}

// Exec hooks fire for jobs.start (started) and for refs the snapshot
// rejects (settled), not only for direct calls.
func TestReviewExecHooksJobsStartAndRejectedCall(t *testing.T) {
	r := baseRegistry(t)
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	var events []starlarkcap.CallEvent
	hooks := starlarkcap.Hooks{Call: func(ev starlarkcap.CallEvent) { events = append(events, ev) }}

	events = nil
	res, err := starlarkcap.Exec(ctx, starlarkcap.Program{
		Filename: "s.star",
		Source:   []byte("j = jobs.start(\"slow.work\", id = \"x\")\nout = j.wait()\n"),
	}, starlarkcap.Limits{}, hooks)
	if err != nil {
		t.Fatalf("Exec jobs.start: %v", err)
	}
	_ = res
	started := 0
	for _, ev := range events {
		if ev.Ref == "slow.work@1" && !ev.Done && ev.JobID != "" {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("jobs.start events = %+v, want one started with a JobID", events)
	}

	events = nil
	if _, err := starlarkcap.Exec(ctx, starlarkcap.Program{
		Filename: "s.star",
		Source:   []byte("x = call(\"nope.gone\")\n"),
	}, starlarkcap.Limits{}, hooks); err == nil {
		t.Fatal("Exec of unknown ref should fail")
	}
	settled := 0
	for _, ev := range events {
		if ev.Done && ev.Err != nil && ev.Err.Code == registry.NotFound {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("rejected-call events = %+v, want one settled not_found", events)
	}
}

// Closing the watcher from an unload's OnResult stops the remaining
// unloads of that catalog.
func TestReviewWatcherCloseStopsPendingRemovals(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeManifest(t, dir, "test.watch",
		"{id: wa, kind: capability.script@1, path: wa.star}",
		"{id: wb, kind: capability.script@1, path: wb.star}")
	writeFile(t, dir, "wa.star", `capability(name = "wa.x", run = lambda ctx, i: {})`)
	writeFile(t, dir, "wb.star", `capability(name = "wb.x", run = lambda ctx, i: {})`)
	var mu sync.Mutex
	var w *starlarkcap.Watcher
	unloads := 0
	w0, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Sources: []bundle.Source{bundle.Dir(dir)}, Interval: 10 * time.Millisecond,
		OnResult: func(prefix string, l starlarkcap.Loaded, err error) {
			if err != nil || len(l.Capabilities) != 0 {
				return // installs and errors are not unloads
			}
			mu.Lock()
			unloads++
			self := w
			mu.Unlock()
			if self != nil {
				self.Close()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	w = w0
	mu.Unlock()
	defer w0.Close()
	if _, a := r.Get("wa.x"); !a {
		t.Fatal("wa not installed")
	}
	// One manifest write drops both files from the next catalog.
	writeManifest(t, dir, "test.watch")
	// One unload happens and closes the watcher; give the reload room to
	// (incorrectly) process the second, then check both counts.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := unloads
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := unloads
	mu.Unlock()
	if n != 1 {
		t.Fatalf("unload reports after Close = %d, want exactly 1", n)
	}
	_, a := r.Get("wa.x")
	_, b := r.Get("wb.x")
	if a == b {
		t.Fatalf("exactly one prefix should remain installed (wa=%v wb=%v)", a, b)
	}
}
