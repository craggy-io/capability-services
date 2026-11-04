package registry

import (
	"context"
	"encoding/json"

	"path/filepath"
	"testing"
	"time"
)

// Close must wait for cancelled job runners, so the terminal journal record
// is written before Close returns (and before a caller tears down the
// journal's directory).
func TestCloseWaitsForJobRunners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	r := New()
	if err := r.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	if err := r.Add(Func("test.slowstop", "Slow to stop", func(ctx context.Context, _ struct{}) (struct{}, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // cleanup after cancellation
		return struct{}{}, ctx.Err()
	}, AsLong(time.Minute))); err != nil {
		t.Fatal(err)
	}
	job, err := r.Start(context.Background(), "test.slowstop", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	r.Close()

	if st := job.Status().State; st != JobCancelled {
		t.Fatalf("after Close, job state = %s, want cancelled", st)
	}
	latest, err := readJobJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := latest[job.ID()].Status.State; st != JobCancelled {
		t.Fatalf("journal state after Close = %s, want cancelled", st)
	}
}

// A handler that ignores cancellation cannot hold Close forever: the wait
// is bounded by JobDrainTimeout.
func TestCloseDrainIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	r := New(JobDrainTimeout(50 * time.Millisecond))
	started := make(chan struct{})
	if err := r.Add(Func("test.stubborn", "Ignores cancellation", func(ctx context.Context, _ struct{}) (struct{}, error) {
		close(started)
		<-release
		return struct{}{}, nil
	}, AsLong(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background(), "test.stubborn", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	<-started
	begin := time.Now()
	r.Close()
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("Close took %v with a 50ms drain timeout", d)
	}
}

// A Start after Close fails fast with unavailable/closed instead of creating
// a job no one will cancel or wait for.
func TestStartAfterCloseFails(t *testing.T) {
	r := New()
	if err := r.Add(Func("test.block", "Blocks until cancelled", func(ctx context.Context, _ struct{}) (struct{}, error) {
		<-ctx.Done()
		return struct{}{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	r.Close()
	begin := time.Now()
	job, err := r.Start(context.Background(), "test.block", json.RawMessage(`{}`))
	if err == nil {
		job.Cancel()
		t.Fatalf("Start after Close created job %s", job.ID())
	}
	if CodeOf(err) != Unavailable || ReasonOf(err) != ReasonClosed {
		t.Fatalf("Start after Close = %v (code %s, reason %q), want unavailable/closed", err, CodeOf(err), ReasonOf(err))
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("Start after Close took %v", d)
	}
	if n := len(r.Jobs(JobFilter{})); n != 0 {
		t.Fatalf("Start after Close left %d jobs in the table", n)
	}
}

// A Start racing Close either fails with unavailable or creates a job that
// Close cancels and waits for: never a job left running, never a Close that
// sits out the drain timeout.
func TestStartRacingCloseLeavesNoRunningJob(t *testing.T) {
	const drain = 3 * time.Second
	for i := 0; i < 200; i++ {
		r := New(JobDrainTimeout(drain))
		if err := r.Add(Func("test.block", "Blocks until cancelled", func(ctx context.Context, _ struct{}) (struct{}, error) {
			<-ctx.Done()
			return struct{}{}, ctx.Err()
		})); err != nil {
			t.Fatal(err)
		}
		var (
			job    *Job
			err    error
			closed = make(chan time.Duration)
			start  = make(chan struct{})
		)
		go func() {
			<-start
			begin := time.Now()
			r.Close()
			closed <- time.Since(begin)
		}()
		close(start)
		job, err = r.Start(context.Background(), "test.block", json.RawMessage(`{}`))
		d := <-closed
		if d >= drain/2 {
			t.Fatalf("iteration %d: Close took %v: it waited out the drain timeout for a job it never cancelled", i, d)
		}
		if err != nil {
			if CodeOf(err) != Unavailable {
				t.Fatalf("iteration %d: Start = %v, want unavailable", i, err)
			}
			continue
		}
		if st := job.Status().State; !st.Terminal() {
			job.Cancel()
			t.Fatalf("iteration %d: job %s is %s after Close returned", i, job.ID(), st)
		}
	}
}
