//go:build windows

package registry_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/registry"
)

func TestJournalCompactionRetriesAfterWindowsFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	r := registry.New(registry.CheckEvery(0), registry.KeepJobs(30*time.Millisecond))
	defer r.Close()
	if err := r.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, r, echoCap("retry.compaction"))
	job, err := r.Start(context.Background(), "retry.compaction", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if handle != syscall.InvalidHandle {
			_ = syscall.CloseHandle(handle)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	r.Jobs(registry.JobFilter{}) // purges the expired job; replacement is locked
	if err := r.JobJournalError(); err == nil {
		t.Fatal("locked replacement did not report a journal error")
	}
	if err := syscall.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = syscall.InvalidHandle

	r.Jobs(registry.JobFilter{}) // no new purge: retry the failed snapshot
	if err := r.JobJournalError(); err != nil {
		t.Fatalf("journal did not recover: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), job.ID()) {
		t.Fatal("expired job remained in recovered journal")
	}
}

// A journal another process holds open (a backup reading it) does not stop
// the registry opening: the failed compaction is reported, then retried.
func TestJournalOpensWhileAnotherProcessHoldsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	r1 := registry.New(registry.CheckEvery(0))
	if err := r1.OpenJobJournal(path); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, r1, echoCap("held.job"))
	job, err := r1.Start(context.Background(), "held.job", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	r1.Close()

	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	r2 := registry.New(registry.CheckEvery(0), registry.KeepJobs(time.Hour))
	defer r2.Close()
	if err := r2.OpenJobJournal(path); err != nil {
		_ = syscall.CloseHandle(handle)
		t.Fatalf("open with the journal held: %v", err)
	}
	if r2.JobJournalError() == nil {
		_ = syscall.CloseHandle(handle)
		t.Fatal("the failed compaction was not reported")
	}
	if _, ok := r2.Job(job.ID()); !ok {
		_ = syscall.CloseHandle(handle)
		t.Fatal("job not restored while the journal was held")
	}
	if err := syscall.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	r2.Jobs(registry.JobFilter{}) // the sweep retries the compaction
	if err := r2.JobJournalError(); err != nil {
		t.Fatalf("journal did not recover: %v", err)
	}

	// Without an expiry sweep nothing would retry, so Open fails as before.
	handle, err = syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	r3 := registry.New(registry.CheckEvery(0), registry.KeepJobs(0))
	defer r3.Close()
	if err := r3.OpenJobJournal(path); err == nil {
		t.Fatal("open without a sweep succeeded on a held journal")
	}
}
