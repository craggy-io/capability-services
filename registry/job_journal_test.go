package registry

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestJobJournalRestoresRunningJobAsLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs", "jobs.jsonl")
	started := make(chan struct{})
	release := make(chan struct{})
	r1 := New()
	if err := r1.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	if err := r1.Add(Func("test.long", "Long test", func(ctx context.Context, _ struct{}) (struct{}, error) {
		Progress(ctx, Update{Stage: "work", Message: "worker is live", Done: 7, Total: 10, Unit: "steps"})
		close(started)
		select {
		case <-release:
			return struct{}{}, nil
		case <-ctx.Done():
			return struct{}{}, ctx.Err()
		}
	}, AsLong(time.Minute))); err != nil {
		t.Fatal(err)
	}
	job, err := r1.Start(context.Background(), "test.long", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-started

	r2 := New()
	defer r2.Close()
	if err := r2.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	restored, ok := r2.Job(job.ID())
	if !ok {
		t.Fatalf("restored job %s not found", job.ID())
	}
	status := restored.Status()
	if status.State != JobLost || status.Last.Done != 7 || status.Error == nil || status.Error.Reason != "process_restarted" {
		t.Fatalf("restored status = %+v", status)
	}
	if _, err := restored.Wait(context.Background()); CodeOf(err) != Internal {
		t.Fatalf("restored wait error = %v", err)
	}

	close(release)
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	r1.Close()
}

func TestJobJournalRestoresCompletedResultAndAdvancesIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	r1 := New()
	if err := r1.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	capability := Func("test.echo", "Echo", func(context.Context, struct{}) (map[string]string, error) {
		return map[string]string{"value": "kept"}, nil
	})
	if err := r1.Add(capability); err != nil {
		t.Fatal(err)
	}
	first, err := r1.Start(context.Background(), "test.echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	want, err := first.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r1.Close()

	r2 := New()
	defer r2.Close()
	if err := r2.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	if err := r2.Add(capability); err != nil {
		t.Fatal(err)
	}
	restored, ok := r2.Job(first.ID())
	if !ok {
		t.Fatalf("completed job %s not restored", first.ID())
	}
	got, err := restored.Wait(context.Background())
	if err != nil || string(got) != string(want) {
		t.Fatalf("restored result = %s, %v; want %s", got, err, want)
	}
	next, err := r2.Start(context.Background(), "test.echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.ID() == first.ID() {
		t.Fatalf("job id reused after restore: %s", next.ID())
	}
	// Let the runner journal its terminal record before TempDir cleanup.
	if _, err := next.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
