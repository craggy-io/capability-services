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
