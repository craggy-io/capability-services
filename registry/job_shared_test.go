package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/registry/jobstoretest"
)

func TestMemoryJobStoreConformance(t *testing.T) {
	jobstoretest.Run(t, func(t *testing.T) func() registry.SharedJobs {
		s := jobstoretest.Memory()
		return func() registry.SharedJobs { return s }
	})
}

// slow is a long capability that reports progress and waits for release or
// cancellation.
func slow(release <-chan struct{}) registry.Capability {
	return registry.Func("demo.slow", "Slow work", func(ctx context.Context, _ struct{}) (string, error) {
		registry.Progress(ctx, registry.Update{Stage: "work", Done: 1, Total: 2, Unit: "steps"})
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}, registry.AsLong(time.Minute))
}

func instance(t *testing.T, store registry.SharedJobs, c registry.Capability, opts ...registry.Option) *registry.Registry {
	t.Helper()
	r := registry.New(append([]registry.Option{registry.ShareJobs(store)}, opts...)...)
	if err := r.Add(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

var ann = registry.Caller{ID: "user:ann", Kind: "user", Session: "s-ann"}

func TestSharedJobsAreFollowedFromAnotherInstance(t *testing.T) {
	store := jobstoretest.Memory()
	release := make(chan struct{})
	a := instance(t, store, slow(release))
	b := instance(t, store, slow(release))
	ctx := registry.WithCaller(context.Background(), ann)

	job, err := a.Start(ctx, "demo.slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(job.ID(), "job-") || len(job.ID()) < 20 {
		t.Fatalf("shared job ids are random, got %q", job.ID())
	}
	remote, ok := b.View(registry.All).JobFor(ctx, job.ID())
	if !ok {
		t.Fatal("instance b cannot see a's job")
	}
	if _, ok := b.View(registry.All).JobFor(registry.WithCaller(context.Background(), registry.Caller{ID: "user:bob", Session: "s-bob"}), job.ID()); ok {
		t.Fatal("bob sees ann's job through b")
	}
	if list := b.Jobs(registry.JobFilter{}); len(list) != 1 || list[0].ID != job.ID() {
		t.Fatalf("b lists %+v", list)
	}
	close(release)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := remote.Wait(wctx)
	if err != nil || string(out) != `"done"` {
		t.Fatal(string(out), err)
	}
	if st := remote.Status(); st.State != registry.JobSucceeded {
		t.Fatal(st)
	}
}

func TestSharedCancelReachesTheRunningInstance(t *testing.T) {
	store := jobstoretest.Memory()
	release := make(chan struct{})
	defer close(release)
	a := instance(t, store, slow(release))
	b := instance(t, store, slow(release))
	ctx := registry.WithCaller(context.Background(), ann)
	job, err := a.Start(ctx, "demo.slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	remote, ok := b.Job(job.ID())
	if !ok {
		t.Fatal("not visible on b")
	}
	remote.Cancel()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := job.Wait(wctx); registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("a's job: %v", err)
	}
	if _, err := remote.Wait(wctx); registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("b's handle: %v", err)
	}
}

// queue is a Dispatcher that delivers each task to a chosen instance, as Cloud
// Tasks would, optionally twice.
type queue struct {
	mu     sync.Mutex
	to     func() *registry.Registry
	twice  bool
	runs   []error
	wg     sync.WaitGroup
	refuse bool
}

func (q *queue) Dispatch(_ context.Context, task registry.Task) error {
	if q.refuse {
		return errors.New("queue full")
	}
	raw, _ := json.Marshal(task) // the task crosses the wire
	deliver := func() {
		defer q.wg.Done()
		var got registry.Task
		_ = json.Unmarshal(raw, &got)
		err := q.to().RunTask(context.Background(), got)
		q.mu.Lock()
		q.runs = append(q.runs, err)
		q.mu.Unlock()
	}
	n := 1
	if q.twice {
		n = 2
	}
	for i := 0; i < n; i++ {
		q.wg.Add(1)
		go deliver()
	}
	return nil
}

func TestDispatchedJobRunsOnceWhereTheQueueDelivers(t *testing.T) {
	store := jobstoretest.Memory()
	var mu sync.Mutex
	ran := map[string]int{}
	work := func(name string) registry.Capability {
		return registry.Func("demo.report", "A report", func(ctx context.Context, in struct{ N int }) (int, error) {
			caller, _ := registry.CallerFrom(ctx)
			mu.Lock()
			ran[name+" "+caller.ID]++
			mu.Unlock()
			registry.Progress(ctx, registry.Update{Stage: "sum", Done: 1, Total: 1})
			return in.N * 2, nil
		}, registry.AsLong(time.Minute))
	}
	q := &queue{twice: true}
	a := instance(t, store, work("a"), registry.DispatchLong(q))
	b := instance(t, store, work("b"), registry.DispatchLong(q))
	q.to = func() *registry.Registry { return b }

	ctx := registry.WithCaller(context.Background(), ann)
	job, err := a.Start(ctx, "demo.report", json.RawMessage(`{"N":21}`))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := job.Wait(wctx)
	if err != nil || string(out) != "42" {
		t.Fatal(string(out), err)
	}
	q.wg.Wait()
	if ran["b user:ann"] != 1 || len(ran) != 1 {
		t.Fatalf("ran %v, want once on b as ann", ran)
	}
	notQueued := 0
	for _, err := range q.runs {
		if errors.Is(err, registry.ErrTaskNotQueued) {
			notQueued++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if notQueued != 1 {
		t.Fatalf("second delivery: %v", q.runs)
	}
	if st, ok := a.Job(job.ID()); !ok || st.Status().State != registry.JobSucceeded {
		t.Fatal("a cannot see the finished job")
	}
}

func TestQuickCapabilitiesAreNotDispatched(t *testing.T) {
	q := &queue{refuse: true}
	quick := registry.Func("demo.quick", "Quick", func(context.Context, struct{}) (string, error) { return "now", nil })
	r := instance(t, jobstoretest.Memory(), quick, registry.DispatchLong(q))
	out, err := r.Invoke(context.Background(), "demo.quick", nil)
	if err != nil || string(out) != `"now"` {
		t.Fatal(string(out), err)
	}
}

func TestRefusedDispatchFailsTheStart(t *testing.T) {
	q := &queue{refuse: true}
	release := make(chan struct{})
	defer close(release)
	r := instance(t, jobstoretest.Memory(), slow(release), registry.DispatchLong(q))
	if _, err := r.Start(context.Background(), "demo.slow", nil); registry.CodeOf(err) != registry.Unavailable {
		t.Fatal(err)
	}
}

func TestDispatchNeedsSharedJobs(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	r := registry.New(registry.DispatchLong(&queue{}))
	t.Cleanup(r.Close)
	if err := r.Add(slow(release)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background(), "demo.slow", nil); registry.CodeOf(err) != registry.Unavailable {
		t.Fatal(err)
	}
}

func TestSharedJobsRefuseAJournal(t *testing.T) {
	r := registry.New(registry.ShareJobs(jobstoretest.Memory()))
	t.Cleanup(r.Close)
	if err := r.OpenJobJournal(t.TempDir() + "/jobs.jsonl"); err == nil {
		t.Fatal("opened a journal on a shared registry")
	}
}
