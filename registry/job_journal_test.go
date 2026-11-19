package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// A read-only job is not journaled, so a page polling one writes nothing
// to disk, and one left by an older journal is dropped at the next open.
func TestJobJournalLeavesOutReadOnlyJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	r1 := New()
	if err := r1.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	read := Func("test.read", "Read", func(context.Context, struct{}) (string, error) {
		return "a large result", nil
	}, WithEffects(ReadOnly|Idempotent))
	write := Func("test.write", "Write", func(context.Context, struct{}) (string, error) {
		return "done", nil
	})
	if err := r1.Add(read, write); err != nil {
		t.Fatal(err)
	}
	run := func(r *Registry, ref string) string {
		t.Helper()
		job, err := r.Start(context.Background(), ref, json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := job.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		return job.ID()
	}
	readID, writeID := run(r1, "test.read"), run(r1, "test.write")
	r1.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), readID) || strings.Contains(string(data), "a large result") {
		t.Fatalf("read-only job journaled: %s", data)
	}
	if !strings.Contains(string(data), writeID) {
		t.Fatalf("job with effects not journaled: %s", data)
	}

	// An older journal that still holds a read-only record.
	old := JobRecord{Status: JobStatus{ID: "job-999999", Capability: "test.read@1", State: JobSucceeded,
		Started: time.Now(), Ended: time.Now()}, Result: json.RawMessage(`"stale"`), Effects: ReadOnly}
	line, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(data, line...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := New()
	defer r2.Close()
	if err := r2.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := r2.Job(old.Status.ID); ok {
		t.Fatal("read-only job from an older journal was restored")
	}
	if _, ok := r2.Job(writeID); !ok {
		t.Fatal("job with effects was not restored")
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "stale") {
		t.Fatal("compaction kept the read-only record")
	}
}
