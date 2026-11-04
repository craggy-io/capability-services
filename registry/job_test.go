package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/registry"
	"github.com/google/jsonschema-go/jsonschema"
)

// fakeContainer mimics a long decompile: multi-stage progress with unit
// changes, an optional stall window, publish step at the end.
type fakeIn struct {
	StallMS int  `json:"stall_ms,omitempty"`
	FailPub bool `json:"fail_publish,omitempty"`
	WaitMS  int  `json:"wait_ms,omitempty"`
}

func fakeContainer() registry.Capability {
	return registry.Func("fake.decompile", "Fake long decompile",
		func(ctx context.Context, in fakeIn) (map[string]string, error) {
			registry.Progress(ctx, registry.Update{Stage: "push", Done: 50, Total: 100, Unit: "bytes"})
			registry.Progress(ctx, registry.Update{Stage: "push", Done: 100, Total: 100, Unit: "bytes"})
			if in.StallMS > 0 {
				select {
				case <-time.After(time.Duration(in.StallMS) * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			registry.Progress(ctx, registry.Update{Stage: "run", Done: 1, Total: 3, Unit: "files", Message: "decompiling"})
			if in.WaitMS > 0 {
				select {
				case <-time.After(time.Duration(in.WaitMS) * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			registry.Progress(ctx, registry.Update{Stage: "publish-output", Done: 0, Total: 0})
			if in.FailPub {
				return nil, registry.Errorf(registry.Internal, "publication failed")
			}
			registry.Progress(ctx, registry.Update{Stage: "finalise"})
			return map[string]string{"done": "yes"}, nil
		}, registry.AsLong(80*time.Millisecond), registry.WithTags("agent"))
}

func TestJobLifecycleAndEventsReplay(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, fakeContainer())
	job, err := r.Start(context.Background(), "fake.decompile", json.RawMessage(`{"wait_ms":30}`))
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	var lastSeq int64
	sawPush100ButRunning := false
	for ev := range job.Events(context.Background()) {
		if ev.Seq <= lastSeq {
			t.Fatalf("event seq not monotonic: %d after %d", ev.Seq, lastSeq)
		}
		lastSeq = ev.Seq
		if ev.Update != nil {
			stages = append(stages, ev.Update.Stage)
			if ev.Update.Stage == "push" && ev.Update.Percent() == 100 && ev.State == registry.JobRunning {
				sawPush100ButRunning = true // stage 100% != job completion
			}
		}
	}
	if !sawPush100ButRunning {
		t.Fatal("push reaching 100% must leave the job running")
	}
	st := job.Status()
	if st.State != registry.JobSucceeded || st.Capability != "fake.decompile@1" {
		t.Fatalf("status=%+v", st)
	}
	out, err := job.Wait(context.Background())
	if err != nil || string(out) != `{"done":"yes"}` {
		t.Fatalf("wait=%s %v", out, err)
	}
	// Replay after completion preserves history and terminal state.
	count := 0
	var terminal registry.JobState
	for ev := range job.Events(context.Background()) {
		count++
		terminal = ev.State
	}
	if count < 4 || terminal != registry.JobSucceeded {
		t.Fatalf("replay count=%d last=%s", count, terminal)
	}
	// Unit switch retained verbatim.
	found := map[string]string{}
	for ev := range job.Events(context.Background()) {
		if ev.Update != nil {
			found[ev.Update.Stage] = ev.Update.Unit
		}
	}
	if found["push"] != "bytes" || found["run"] != "files" {
		t.Fatalf("units=%v", found)
	}
	if got := (registry.Update{Stage: "x"}).Percent(); got != -1 {
		t.Fatalf("unknown total percent=%v want -1", got)
	}
}

func TestJobStalledRecovers(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, fakeContainer())
	job, _ := r.Start(context.Background(), "fake.decompile", json.RawMessage(`{"stall_ms":250}`))
	deadline := time.Now().Add(2 * time.Second)
	sawStalled := false
	for time.Now().Before(deadline) {
		st := job.Status().State
		if st == registry.JobStalled {
			sawStalled = true
			break
		}
		if st.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawStalled {
		t.Fatal("job never showed stalled during heartbeat gap")
	}
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if job.Status().State != registry.JobSucceeded {
		t.Fatal("stalled job must recover to running then succeed")
	}
}

func TestJobCancelAndLateProgress(t *testing.T) {
	r := registry.New()
	defer r.Close()
	lateCtx := make(chan context.Context, 1)
	c := registry.Capability{Name: "slow.work", Input: &jsonschema.Schema{}, Long: true,
		Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
			lateCtx <- ctx
			<-ctx.Done()
			registry.Progress(ctx, registry.Update{Stage: "zombie", Done: 1, Total: 1})
			return map[string]string{"should": "not succeed"}, nil // success after cancel
		}}
	mustAdd(t, r, c)
	job, _ := r.Start(context.Background(), "slow.work", nil)
	hctx := <-lateCtx
	job.Cancel()
	_, err := job.Wait(context.Background())
	if registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("cancelled wait err=%v", err)
	}
	st := job.Status()
	if st.State != registry.JobCancelled {
		t.Fatalf("state=%s want cancelled (late success must not convert)", st.State)
	}
	seq := st.Seq
	registry.Progress(hctx, registry.Update{Stage: "after-terminal", Done: 1, Total: 1})
	if job.Status().Seq != seq {
		t.Fatal("terminal job accepted late progress")
	}
}

// Tool "exit zero" but publication failure: the job fails, no result.
func TestJobPublicationFailureFailsJob(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, fakeContainer())
	job, _ := r.Start(context.Background(), "fake.decompile", json.RawMessage(`{"fail_publish":true}`))
	if _, err := job.Wait(context.Background()); err == nil {
		t.Fatal("publication failure must fail the job")
	}
	st := job.Status()
	if st.State != registry.JobFailed || st.Error == nil {
		t.Fatalf("status=%+v", st)
	}
	if _, ok := job.Result(); ok {
		t.Fatal("failed job emitted a result")
	}
	// Replay preserves terminal failed.
	var last registry.JobState
	for ev := range job.Events(context.Background()) {
		last = ev.State
	}
	if last != registry.JobFailed {
		t.Fatalf("replay last=%s", last)
	}
}

func TestJobOutlivesCallerContext(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, fakeContainer())
	ctx, cancel := context.WithCancel(context.Background())
	job, err := r.Start(ctx, "fake.decompile", json.RawMessage(`{"wait_ms":60}`))
	if err != nil {
		t.Fatal(err)
	}
	cancel() // caller goes away immediately
	out, err := job.Wait(context.Background())
	if err != nil || string(out) != `{"done":"yes"}` {
		t.Fatalf("job should outlive caller ctx: %s %v state=%s", out, err, job.Status().State)
	}
	// Invoke with a ctx that times out returns, but the job keeps going.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	_, err = r.Invoke(ctx2, "fake.decompile", json.RawMessage(`{"wait_ms":80}`))
	if registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("invoke timeout err=%v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		done := false
		for _, st := range r.Jobs(registry.JobFilter{State: registry.JobSucceeded}) {
			if st.ID != job.ID() {
				done = true
			}
		}
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Invoke's job did not finish in background after ctx timeout")
}

func TestInvalidProgressIgnored(t *testing.T) {
	r := registry.New()
	defer r.Close()
	c := registry.Capability{Name: "bad.progress", Input: &jsonschema.Schema{}, Long: true,
		Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
			registry.Progress(ctx, registry.Update{Stage: "s", Done: -1})
			registry.Progress(ctx, registry.Update{Stage: "s", Done: 9, Total: 3})
			registry.Progress(ctx, registry.Update{Stage: "s", Done: 2, Total: 3, Unit: "steps"})
			return "ok", nil
		}}
	mustAdd(t, r, c)
	job, _ := r.Start(context.Background(), "bad.progress", nil)
	updates := 0
	var last registry.Update
	for ev := range job.Events(context.Background()) {
		if ev.Update != nil {
			updates++
			last = *ev.Update
		}
	}
	if updates != 1 || last.Done != 2 || last.Unit != "steps" {
		t.Fatalf("updates=%d last=%+v (invalid ones must be dropped)", updates, last)
	}
	// Progress outside a job is a no-op (must not panic).
	registry.Progress(context.Background(), registry.Update{Stage: "nowhere"})
}

func TestLimitQueue(t *testing.T) {
	r := registry.New(registry.Limit("analysis", 1))
	defer r.Close()
	var concurrent, maxSeen atomic.Int64
	started := make(chan struct{}, 8)
	c := registry.Capability{Name: "a.work", Input: &jsonschema.Schema{}, Long: true, Tags: []string{"analysis"},
		Handler: func(ctx context.Context, in json.RawMessage) (any, error) {
			started <- struct{}{}
			n := concurrent.Add(1)
			for {
				m := maxSeen.Load()
				if n <= m || maxSeen.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			concurrent.Add(-1)
			return "ok", nil
		}}
	mustAdd(t, r, c)
	j1, _ := r.Start(context.Background(), "a.work", nil)
	<-started // j1 holds the only slot before j2 exists (deterministic order)
	j2, _ := r.Start(context.Background(), "a.work", nil)
	time.Sleep(10 * time.Millisecond)
	if st := j2.Status().State; st != registry.JobQueued {
		t.Fatalf("second job state=%s want queued behind Limit(1)", st)
	}
	for _, j := range []*registry.Job{j1, j2} {
		if _, err := j.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if maxSeen.Load() != 1 {
		t.Fatalf("max concurrent=%d want 1", maxSeen.Load())
	}
	// Cancelling a queued job frees nothing it didn't hold and ends cancelled.
	j3, _ := r.Start(context.Background(), "a.work", nil)
	<-started
	// Drain the extra start signals from j1/j2's handlers if buffered.
	j4, _ := r.Start(context.Background(), "a.work", nil)
	j4.Cancel()
	if _, err := j4.Wait(context.Background()); registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("queued cancel err=%v", err)
	}
	if _, err := j3.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReadiness(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	c := echoCap("ready.x")
	c.Check = func(ctx context.Context) error {
		if down.Load() {
			return errors.New("docker daemon unreachable")
		}
		return nil
	}
	mustAdd(t, r, c)
	e, _ := r.Get("ready.x")
	if e.Ready || e.CheckError != "docker daemon unreachable" {
		t.Fatalf("entry=%+v", e)
	}
	if got := r.List(); len(got) != 1 {
		t.Fatal("unready cap must stay in List with Ready:false")
	}
	_, err := r.Invoke(context.Background(), "ready.x", nil)
	if registry.CodeOf(err) != registry.Unavailable {
		t.Fatalf("unready invoke: %v", err)
	}
	down.Store(false)
	r.RefreshChecks(context.Background())
	if e, _ := r.Get("ready.x"); !e.Ready {
		t.Fatal("RefreshChecks did not flip readiness")
	}
	if _, err := r.Invoke(context.Background(), "ready.x", nil); err != nil {
		t.Fatal(err)
	}
	// A panicking Check means not ready, not a crashed registry.
	p := echoCap("ready.panic")
	p.Check = func(ctx context.Context) error { panic("nope") }
	mustAdd(t, r, p)
	if e, _ := r.Get("ready.panic"); e.Ready || e.CheckError == "" {
		t.Fatalf("panic check entry=%+v", e)
	}
}

func TestReadinessPeriodicAndWatch(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	r := registry.New(registry.CheckEvery(30 * time.Millisecond))
	defer r.Close()
	c := echoCap("ready.y")
	c.Check = func(ctx context.Context) error {
		if down.Load() {
			return errors.New("down")
		}
		return nil
	}
	mustAdd(t, r, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := r.Watch(ctx)
	down.Store(false)
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("periodic readiness flip did not fire Watch")
	}
	if e, _ := r.Get("ready.y"); !e.Ready {
		t.Fatal("periodic check did not update readiness")
	}
}

func TestKeepJobsPurge(t *testing.T) {
	r := registry.New(registry.KeepJobs(time.Nanosecond), registry.CheckEvery(0))
	defer r.Close()
	mustAdd(t, r, echoCap("k.x"))
	job, _ := r.Start(context.Background(), "k.x", nil)
	_, _ = job.Wait(context.Background())
	time.Sleep(2 * time.Millisecond)
	if _, ok := r.Job(job.ID()); ok {
		t.Fatal("finished job past KeepJobs was not purged")
	}
	// The Job handle itself stays usable after purge.
	if job.Status().State != registry.JobSucceeded {
		t.Fatal("purged job handle broken")
	}
}

func TestCloseCancelsJobs(t *testing.T) {
	r := registry.New()
	mustAdd(t, r, fakeContainer())
	job, _ := r.Start(context.Background(), "fake.decompile", json.RawMessage(`{"wait_ms":5000}`))
	time.Sleep(20 * time.Millisecond)
	r.Close()
	if _, err := job.Wait(context.Background()); registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("Close should cancel running jobs: %v state=%s", err, job.Status().State)
	}
	r.Close() // idempotent
}

// A job started from a handler is its child: it names the parent, and
// cancelling the parent cancels it; a parent that finishes normally leaves
// it running.
func TestChildJobsFollowParentCancellation(t *testing.T) {
	r := registry.New()
	defer r.Close()
	type parentIn struct {
		Hold bool `json:"hold,omitempty"`
	}
	children := make(chan *registry.Job, 2)
	mustAdd(t, r,
		registry.Func("child.block", "Block until cancelled",
			func(ctx context.Context, _ struct{}) (struct{}, error) {
				<-ctx.Done()
				return struct{}{}, ctx.Err()
			}, registry.AsLong(time.Second)),
		registry.Func("parent.spawn", "Start a child",
			func(ctx context.Context, in parentIn) (struct{}, error) {
				child, err := registry.ViewFrom(ctx).Start(ctx, "child.block", nil)
				if err != nil {
					return struct{}{}, err
				}
				children <- child
				if in.Hold {
					<-ctx.Done()
					return struct{}{}, ctx.Err()
				}
				return struct{}{}, nil
			}, registry.AsLong(time.Second)),
	)

	held, err := r.Start(context.Background(), "parent.spawn", json.RawMessage(`{"hold":true}`))
	if err != nil {
		t.Fatal(err)
	}
	child := <-children
	if got := child.Status().Parent; got != held.ID() {
		t.Fatalf("child parent = %q, want %q", got, held.ID())
	}
	held.Cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := child.Wait(ctx); registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("child after parent cancel: %v (state %s)", err, child.Status().State)
	}

	done, err := r.Start(context.Background(), "parent.spawn", nil)
	if err != nil {
		t.Fatal(err)
	}
	orphan := <-children
	if _, err := done.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if st := orphan.Status().State; st.Terminal() {
		t.Fatalf("child of a finished parent ended: %s", st)
	}
	orphan.Cancel()
}

// A per-file progress stream keeps one replay event per stage, the
// latest, while live subscribers see every update.
func TestReplayCoalescesSameStageProgress(t *testing.T) {
	r := registry.New()
	defer r.Close()
	release := make(chan struct{})
	burst := registry.Func("burst.files", "Burst",
		func(ctx context.Context, in fakeIn) (fakeIn, error) {
			registry.Progress(ctx, registry.Update{Stage: "push", Done: 1, Total: 1, Unit: "bytes"})
			for i := int64(1); i <= 1000; i++ {
				registry.Progress(ctx, registry.Update{Stage: "pull", Done: i, Unit: "files"})
			}
			<-release
			return in, nil
		})
	mustAdd(t, r, burst)
	job, err := r.Start(context.Background(), "burst.files", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	live := job.Events(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for job.Status().Last.Done != 1000 {
		if time.Now().After(deadline) {
			t.Fatal("burst never reported its last update")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	liveUpdates := 0
	for ev := range live {
		if ev.Update != nil {
			liveUpdates++
		}
	}
	if liveUpdates < 2 {
		t.Fatalf("live subscriber saw %d updates", liveUpdates)
	}
	var replay []registry.Update
	for ev := range job.Events(context.Background()) {
		if ev.Update != nil {
			replay = append(replay, *ev.Update)
		}
	}
	if len(replay) != 2 || replay[0].Stage != "push" || replay[1].Stage != "pull" || replay[1].Done != 1000 {
		t.Fatalf("replay updates = %+v, want push then the last pull (1000)", replay)
	}
}
