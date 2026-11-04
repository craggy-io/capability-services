package registry_test

// Regression tests for the 2026-11-04 full-repo review findings in the
// registry package. Each test names the defect it pins.

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/registry"
	"github.com/google/jsonschema-go/jsonschema"
)

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A capability whose tag list repeats a limited tag must not acquire the
// same 1-slot semaphore twice and deadlock itself in queued.
func TestReviewDuplicateLimitedTagNoDeadlock(t *testing.T) {
	r := registry.New(registry.Limit("analysis", 1), registry.CheckEvery(0))
	defer r.Close()
	c := echoCap("dup.tag")
	c.Tags = []string{"analysis", "analysis"}
	mustAdd(t, r, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := r.Invoke(ctx, "dup.tag", json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("Invoke with duplicated limited tag: %v", err)
	}
	if string(out) != `{"a":1}` {
		t.Fatalf("out = %s", out)
	}
}

// Mutating slices/schemas on a returned Entry must not rewrite registry
// state (view enforcement reads the same backing arrays).
func TestReviewEntryMutationIsolated(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	c := echoCap("alias.x")
	c.Tags = []string{"agent"}
	mustAdd(t, r, c)
	e, ok := r.Get("alias.x")
	if !ok {
		t.Fatal("Get failed")
	}
	e.Tags[0] = "admin"
	e.Input.Type = "string"
	e2, _ := r.Get("alias.x")
	if !e2.HasTag("agent") || e2.HasTag("admin") {
		t.Fatalf("registry tags rewritten via Entry: %v", e2.Tags)
	}
	if e2.Input.Type == "string" {
		t.Fatal("registry input schema rewritten via Entry")
	}
	if _, ok := r.View(registry.Not(registry.Tagged("admin"))).Get("alias.x"); !ok {
		t.Fatal("view enforcement changed by Entry mutation")
	}
	// Mutating the caller's schema after Add must not rewrite the snapshot.
	s := &jsonschema.Schema{Type: "object"}
	c2 := echoCap("alias.y")
	c2.Input = s
	mustAdd(t, r, c2)
	s.Type = "string"
	e3, _ := r.Get("alias.y")
	if e3.Input.Type == "string" {
		t.Fatal("registry schema aliases the caller's schema")
	}
}

// A Check runs host code; calling Add from inside it must not self-deadlock
// on writeMu.
func TestReviewCheckReentrantAdd(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	var added atomic.Bool
	c := echoCap("reentrant.x")
	c.Check = func(ctx context.Context) error {
		if added.CompareAndSwap(false, true) {
			return r.Add(echoCap("reentrant.other"))
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.Add(c) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Add deadlocked: Check ran under writeMu")
	}
}

// CheckTimeout must bound a Check that ignores its context.
func TestReviewCheckTimeoutBoundsIgnoringCheck(t *testing.T) {
	r := registry.New(registry.CheckEvery(0), registry.CheckTimeout(50*time.Millisecond))
	defer r.Close()
	c := echoCap("slow.check")
	c.Check = func(ctx context.Context) error { time.Sleep(600 * time.Millisecond); return nil }
	start := time.Now()
	mustAdd(t, r, c)
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("Add took %v; CheckTimeout did not bound the check", elapsed)
	}
	if e, _ := r.Get("slow.check"); e.Ready {
		t.Fatal("timed-out check reported ready")
	}
}

// A slow RefreshChecks must not overwrite readiness a concurrent Replace
// just published.
func TestReviewRefreshDoesNotOverwriteReplace(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	gate := make(chan struct{})
	var calls atomic.Int64
	mk := func(failAfterGate bool) registry.Capability {
		c := echoCap("flip.x")
		c.Check = func(ctx context.Context) error {
			if failAfterGate && calls.Add(1) > 1 {
				<-gate
				return context.DeadlineExceeded // any error: stale failure
			}
			return nil
		}
		return c
	}
	mustAdd(t, r, mk(true))
	refreshDone := make(chan struct{})
	go func() { defer close(refreshDone); r.RefreshChecks(context.Background()) }()
	waitFor(t, "refresh to enter gated check", 2*time.Second, func() bool { return calls.Load() > 1 })
	healthy := echoCap("flip.x")
	healthy.Check = func(ctx context.Context) error { return nil }
	if err := r.Replace("flip", healthy); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	close(gate)
	<-refreshDone
	if e, _ := r.Get("flip.x"); !e.Ready {
		t.Fatal("stale refresh overwrote the Replace readiness")
	}
}

// A panicking middleware constructor (or a nil middleware) must surface as
// an internal error / be ignored, never panic the caller.
func TestReviewPanickingMiddlewareConstructor(t *testing.T) {
	r := registry.New(registry.CheckEvery(0), registry.Use(
		func(next registry.Handler, c registry.Capability) registry.Handler { panic("mw boom") }))
	defer r.Close()
	mustAdd(t, r, echoCap("mw.x"))
	_, err := r.Invoke(context.Background(), "mw.x", json.RawMessage(`{}`))
	if err == nil || registry.CodeOf(err) != registry.Internal {
		t.Fatalf("err = %v, want internal", err)
	}
}

func TestReviewNilMiddlewareIgnored(t *testing.T) {
	r := registry.New(registry.CheckEvery(0), registry.Use(nil))
	defer r.Close()
	mustAdd(t, r, echoCap("mw.y"))
	if _, err := r.Invoke(context.Background(), "mw.y", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Invoke with nil middleware: %v", err)
	}
}

// A Func returning []byte advertises a base64 string schema; its result
// must be encoded as one even when the bytes happen to be valid JSON.
func TestReviewFuncBytesOutputBase64(t *testing.T) {
	r := registry.New(registry.CheckEvery(0), registry.ValidateOutput(true))
	defer r.Close()
	mustAdd(t, r, registry.Func("bytes.out", "bytes",
		func(ctx context.Context, _ struct{}) ([]byte, error) { return []byte("123"), nil }))
	out, err := r.Invoke(context.Background(), "bytes.out", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(out) != `"MTIz"` {
		t.Fatalf("out = %s, want base64 string %q", out, `"MTIz"`)
	}
}

// After Close, every Start/Invoke fails unavailable/closed — including
// unknown refs and invalid input.
func TestReviewClosedStartAlwaysClosed(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	mustAdd(t, r, echoCap("closed.x"))
	r.Close()
	for _, ref := range []string{"closed.x", "nope.missing"} {
		_, err := r.Invoke(context.Background(), ref, json.RawMessage(`{bad`))
		if registry.CodeOf(err) != registry.Unavailable || registry.ReasonOf(err) != registry.ReasonClosed {
			t.Fatalf("Invoke(%s) after Close: %v (code=%s reason=%q)", ref, err, registry.CodeOf(err), registry.ReasonOf(err))
		}
	}
}

// No-input capabilities accept empty/null/{} semantically, whitespace too.
func TestReviewNoInputWhitespaceBodies(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	c := echoCap("noin.x")
	c.Input = nil
	mustAdd(t, r, c)
	for _, body := range []string{"", "null", "{}", "{ }", " {}\n", " null "} {
		if _, err := r.Invoke(context.Background(), "noin.x", json.RawMessage(body)); err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
	}
	if _, err := r.Invoke(context.Background(), "noin.x", json.RawMessage(`{"x":1}`)); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("non-empty body: err = %v, want invalid", err)
	}
}

// The job runner must see the input as it was at Start, not whatever the
// caller later writes into the shared buffer.
func TestReviewStartInputCopied(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	release := make(chan struct{})
	seen := make(chan string, 1)
	c := echoCap("copy.in")
	c.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		<-release
		seen <- string(in)
		return in, nil
	}
	mustAdd(t, r, c)
	buf := json.RawMessage(`{"v":"original"}`)
	job, err := r.Start(context.Background(), "copy.in", buf)
	if err != nil {
		t.Fatal(err)
	}
	copy(buf, `{"v":"mutated!"}`)
	close(release)
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; !strings.Contains(got, "original") {
		t.Fatalf("handler saw mutated input %s", got)
	}
}

// Cancelling a parent that already finished normally leaves its children
// running (docs/jobs.md).
func TestReviewCancelFinishedParentLeavesChildRunning(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	childStarted := make(chan struct{})
	childRelease := make(chan struct{})
	var childJob atomic.Value // *registry.Job
	child := echoCap("fam.child")
	child.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		close(childStarted)
		select {
		case <-childRelease:
			return json.RawMessage(`{"ok":true}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	parent := echoCap("fam.parent")
	parent.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		j, err := registry.ViewFrom(ctx).Start(ctx, "fam.child", json.RawMessage(`{}`))
		if err != nil {
			return nil, err
		}
		childJob.Store(j)
		return json.RawMessage(`{"spawned":true}`), nil
	}
	mustAdd(t, r, child, parent)
	pjob, err := r.Start(context.Background(), "fam.parent", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-childStarted
	if _, err := pjob.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	pjob.Cancel() // already succeeded: must be a no-op for the child
	cjob := childJob.Load().(*registry.Job)
	time.Sleep(100 * time.Millisecond)
	if st := cjob.Status().State; st.Terminal() {
		t.Fatalf("child state = %s after cancelling finished parent", st)
	}
	close(childRelease)
	if _, err := cjob.Wait(context.Background()); err != nil {
		t.Fatalf("child Wait: %v", err)
	}
}

// Progress Data is copied and JSON-validated: unmarshalable data drops the
// update instead of breaking every later status/journal marshal, and later
// mutation of the caller's map is invisible.
func TestReviewProgressDataValidatedAndCopied(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	data := map[string]any{"n": float64(1)}
	c := echoCap("prog.data")
	c.Long = true
	c.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		registry.Progress(ctx, registry.Update{Stage: "bad", Data: math.NaN()})
		registry.Progress(ctx, registry.Update{Stage: "good", Done: 1, Total: 2, Data: data})
		data["n"] = float64(99)
		return json.RawMessage(`{}`), nil
	}
	mustAdd(t, r, c)
	job, err := r.Start(context.Background(), "prog.data", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := job.Status()
	if st.Last.Stage != "good" {
		t.Fatalf("last stage = %q (NaN update should have been dropped)", st.Last.Stage)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("status does not marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"n":1`) {
		t.Fatalf("status data not the copied value: %s", raw)
	}
}

// Status snapshots are isolated: mutating one must not rewrite the job.
func TestReviewStatusIsolated(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	c := echoCap("iso.x")
	c.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		return nil, registry.Errorf(registry.Conflict, "original message")
	}
	mustAdd(t, r, c)
	ctx := registry.WithCaller(context.Background(), registry.Caller{ID: "u1", Attrs: map[string]string{"k": "v"}})
	job, err := r.Start(ctx, "iso.x", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = job.Wait(context.Background())
	st := job.Status()
	st.Error.Message = "tampered"
	st.Caller.Attrs["k"] = "tampered"
	st2 := job.Status()
	if st2.Error.Message != "original message" {
		t.Fatalf("error message = %q", st2.Error.Message)
	}
	if st2.Caller.Attrs["k"] != "v" {
		t.Fatalf("caller attrs = %v", st2.Caller.Attrs)
	}
}

// A cancelled job keeps the handler error's reason and clean message.
func TestReviewCancelPreservesReason(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	c := echoCap("cancel.reason")
	c.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, registry.WrapReason(registry.Cancelled, registry.ReasonBackendDown,
			registry.Errorf(registry.Cancelled, "backend went away"))
	}
	mustAdd(t, r, c)
	job, err := r.Start(context.Background(), "cancel.reason", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job running", 2*time.Second, func() bool { return job.Status().State == registry.JobRunning })
	job.Cancel()
	_, werr := job.Wait(context.Background())
	if registry.ReasonOf(werr) != registry.ReasonBackendDown {
		t.Fatalf("reason = %q (err %v)", registry.ReasonOf(werr), werr)
	}
	if strings.Contains(werr.Error(), "cancelled: cancelled:") {
		t.Fatalf("doubled prefix: %v", werr)
	}
}

// Close called from a Check running on the readiness loop returns.
func TestReviewCloseFromCheckReturns(t *testing.T) {
	r := registry.New(registry.CheckEvery(10 * time.Millisecond))
	var calls atomic.Int64
	closedReturned := make(chan struct{})
	c := echoCap("close.fromcheck")
	c.Check = func(ctx context.Context) error {
		if calls.Add(1) > 1 {
			r.Close()
			close(closedReturned)
		}
		return nil
	}
	mustAdd(t, r, c)
	select {
	case <-closedReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Close from readiness Check deadlocked")
	}
}

// Restored jobs keep the capability fields view filters decide on.
func TestReviewRestoredJobRespectsTagView(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.jsonl")
	r := registry.New(registry.CheckEvery(0))
	if err := r.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	c := echoCap("admin.thing")
	c.Tags = []string{"admin"}
	c.Effects = registry.Destructive
	mustAdd(t, r, c)
	job, err := r.Start(context.Background(), "admin.thing", json.RawMessage(`{"secret":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()

	r2 := registry.New(registry.CheckEvery(0))
	defer r2.Close()
	if err := r2.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	restricted := r2.View(registry.Not(registry.Tagged("admin")))
	if _, ok := restricted.Job(job.ID()); ok {
		t.Fatal("restored admin job visible through Not(Tagged(admin)) view")
	}
	if _, ok := r2.Job(job.ID()); !ok {
		t.Fatal("restored job missing from the full view")
	}
}

// A result larger than the old 16MiB scanner cap still journals and reopens.
func TestReviewJournalLargeResultReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.jsonl")
	r := registry.New(registry.CheckEvery(0))
	if err := r.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 17<<20)
	c := echoCap("big.out")
	c.Handler = func(ctx context.Context, in json.RawMessage) (any, error) { return big, nil }
	mustAdd(t, r, c)
	job, err := r.Start(context.Background(), "big.out", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r2 := registry.New(registry.CheckEvery(0))
	defer r2.Close()
	if err := r2.OpenJobJournal(path); err != nil {
		t.Fatalf("reopen with >16MiB record: %v", err)
	}
	j2, ok := r2.Job(job.ID())
	if !ok {
		t.Fatal("large job not restored")
	}
	if out, err := j2.Wait(context.Background()); err != nil || len(out) < 17<<20 {
		t.Fatalf("restored Wait: err=%v len=%d", err, len(out))
	}
}

// A failed OpenJobJournal commits nothing: no jobs, and a retry is allowed.
func TestReviewFailedJournalOpenLeavesClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.jsonl")
	bad := `{"status":{"id":"job-7","capability":"unpinned","caller":{"id":"","kind":"","session":""},"state":"succeeded","last":{},"seq":1,"started":"2026-01-01T00:00:00Z","heartbeat":"2026-01-01T00:00:00Z"}}` + "\n"
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	if err := r.OpenJobJournal(path); err == nil {
		t.Fatal("Open with unpinned capability ref should fail")
	}
	if got := r.Jobs(registry.JobFilter{}); len(got) != 0 {
		t.Fatalf("failed open left %d jobs behind", len(got))
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.OpenJobJournal(path); err != nil {
		t.Fatalf("retry after failed open: %v", err)
	}
}

// Finished jobs are purged (and the journal compacted) without any Job
// lookup and without a readiness loop.
func TestReviewJournalCompactsAfterPurgeWithoutLookups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.jsonl")
	r := registry.New(registry.CheckEvery(0), registry.KeepJobs(30*time.Millisecond))
	defer r.Close()
	if err := r.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, r, echoCap("purge.me"))
	job, err := r.Start(context.Background(), "purge.me", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "journal compaction after purge", 3*time.Second, func() bool {
		b, err := os.ReadFile(path)
		return err == nil && !strings.Contains(string(b), job.ID())
	})
}
