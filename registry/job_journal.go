package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/craggy-io/foundation-kit/fsx"
)

// JobRecord is the durable projection of one job: what the job journal and a
// SharedJobs store keep. The journal appends the
// latest record after each state or progress event; startup keeps the newest
// record per id. Handler goroutines cannot be resumed after a process exits,
// so restored non-terminal jobs are explicitly marked lost while preserving
// their last progress and caller identity.
type JobRecord struct {
	Status JobStatus       `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	// The capability's filter-relevant fields, persisted so a restored
	// job is judged by view filters exactly as the live job was. Without
	// them a restart would strip Tags/Effects and let e.g. an admin job
	// back in through a Not(Tagged(admin)) view.
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Effects     Effects   `json:"effects,omitempty"`
	Supersedes  []string  `json:"supersedes,omitempty"`
	Accepts     []Subject `json:"accepts,omitempty"`
	Emits       []Subject `json:"emits,omitempty"`
	Long        bool      `json:"long,omitempty"`
	HeartbeatMs int64     `json:"heartbeatMs,omitempty"`
	Origin      Origin    `json:"origin,omitempty"`
}

// OpenJobJournal enables durable job status at path and restores previous
// records. Call it before starting jobs. A non-terminal restored job becomes
// lost with reason process_restarted; terminal results remain waitable.
func (r *Registry) OpenJobJournal(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("registry: job journal path is empty")
	}
	r.jobsMu.Lock()
	defer r.jobsMu.Unlock()
	if len(r.jobs) != 0 || r.jobJournalPath != "" {
		return fmt.Errorf("registry: job journal must be opened before jobs start")
	}
	if r.cfg.shared != nil {
		return fmt.Errorf("registry: a registry with ShareJobs keeps no job journal")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("registry: create job journal directory: %w", err)
	}
	latest, err := readJobJournal(path)
	if err != nil {
		return err
	}
	// All-or-nothing: build every restored job and the compacted file
	// first, and commit path + jobs only once all of it succeeded. A
	// failed Open then leaves the registry untouched and retryable.
	restored := make(map[string]*Job, len(latest))
	records := make([]JobRecord, 0, len(latest))
	cutoff := time.Now().Add(-r.cfg.keepJobs)
	for id, rec := range latest {
		if r.cfg.keepJobs > 0 && rec.Status.State.Terminal() && !rec.Status.Ended.IsZero() && rec.Status.Ended.Before(cutoff) {
			continue // already expired under KeepJobs; don't resurrect it
		}
		job, err := restoredJob(rec, r.persistJob)
		if err != nil {
			return fmt.Errorf("registry: restore job %s: %w", id, err)
		}
		restored[id] = job
		job.mu.Lock()
		records = append(records, job.recordLocked())
		job.mu.Unlock()
	}
	if err := writeJobJournalFile(path, records); err != nil {
		return err
	}
	r.jobJournalPath = path
	for id, job := range restored {
		r.jobs[id] = job
		advanceJobCounter(id)
	}
	return nil
}

// JobJournalError reports a write failure encountered after the journal was
// opened. Jobs keep running, but the host can expose this as a health failure
// instead of claiming restart recovery is current.
func (r *Registry) JobJournalError() error {
	r.jobJournalMu.Lock()
	defer r.jobJournalMu.Unlock()
	return r.jobJournalErr
}

func readJobJournal(path string) (map[string]JobRecord, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]JobRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: open job journal: %w", err)
	}
	defer f.Close()
	latest := map[string]JobRecord{}
	// ReadBytes, not a Scanner: records carry whole results, and a fixed
	// token cap would make any journal past it unreadable.
	br := bufio.NewReader(f)
	for line := 1; ; line++ {
		raw, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(raw)) > 0 {
			var rec JobRecord
			if uerr := json.Unmarshal(raw, &rec); uerr != nil {
				return nil, fmt.Errorf("registry: job journal line %d: %w", line, uerr)
			}
			if rec.Status.ID == "" {
				return nil, fmt.Errorf("registry: job journal line %d has no job id", line)
			}
			latest[rec.Status.ID] = rec
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("registry: read job journal: %w", err)
		}
	}
	return latest, nil
}

func restoredJob(rec JobRecord, persist func(JobRecord)) (*Job, error) {
	if !rec.Status.State.Terminal() {
		rec.Status.State = JobLost
		rec.Status.Ended = time.Now()
		rec.Status.Error = &ErrorInfo{Code: Internal, Reason: "process_restarted",
			Message: "the process restarted before the job completed", Retryable: false}
		rec.Result = nil
	}
	return recordJob(rec, persist)
}

// recordJob rebuilds a finished Job from rec.
func recordJob(rec JobRecord, persist func(JobRecord)) (*Job, error) {
	name, version, pinned, ok := ParseRef(rec.Status.Capability)
	if !ok || !pinned {
		return nil, fmt.Errorf("invalid capability %q", rec.Status.Capability)
	}
	status := rec.Status
	done := make(chan struct{})
	close(done)
	capab := Capability{Name: name, Version: version, Title: rec.Title, Description: rec.Description,
		Tags: append([]string(nil), rec.Tags...), Effects: rec.Effects,
		Supersedes: append([]string(nil), rec.Supersedes...),
		Accepts:    append([]Subject(nil), rec.Accepts...), Emits: append([]Subject(nil), rec.Emits...),
		Long: rec.Long, Heartbeat: time.Duration(rec.HeartbeatMs) * time.Millisecond, Origin: rec.Origin}
	j := &Job{id: status.ID, capab: capab, caller: cloneCaller(status.Caller),
		parent: status.Parent, supBy: append([]string(nil), status.SupersededBy...), done: done,
		state: status.State, last: status.Last, seq: status.Seq, started: status.Started,
		heartbeat: status.Heartbeat, ended: status.Ended, errInfo: status.Error,
		result: append(json.RawMessage(nil), rec.Result...), history: []Event{{Seq: status.Seq, Time: status.Ended, State: status.State, Error: status.Error}},
		subs: map[chan Event]struct{}{}, persist: persist}
	return j, nil
}

func advanceJobCounter(id string) {
	n, err := strconv.ParseUint(strings.TrimPrefix(id, "job-"), 10, 64)
	if err != nil || id != "job-"+strconv.FormatUint(n, 10) {
		return
	}
	for cur := jobCounter.Load(); cur < n && !jobCounter.CompareAndSwap(cur, n); cur = jobCounter.Load() {
	}
}

func (r *Registry) persistJob(rec JobRecord) {
	if r.cfg.shared != nil {
		r.putShared(rec)
		return
	}
	r.jobJournalMu.Lock()
	defer r.jobJournalMu.Unlock()
	if r.jobJournalPath == "" || r.jobJournalErr != nil {
		return
	}
	f, err := os.OpenFile(r.jobJournalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		var data []byte
		data, err = json.Marshal(rec)
		if err == nil {
			data = append(data, '\n')
			_, err = f.Write(data)
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		r.jobJournalErr = fmt.Errorf("registry: append job journal: %w", err)
	}
}

func (r *Registry) jobRecordsLocked() []JobRecord {
	records := make([]JobRecord, 0, len(r.jobs))
	for _, job := range r.jobs {
		job.mu.Lock()
		records = append(records, job.recordLocked())
		job.mu.Unlock()
	}
	return records
}

// writeJobJournalFile replaces the journal at path with exactly records,
// atomically and durably with fsx.Write, so a failed or interrupted
// compaction never truncates the live journal.
func writeJobJournalFile(path string, records []JobRecord) error {
	err := fsx.Write(path, 0o600, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, rec := range records {
			if err := enc.Encode(rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("registry: compact job journal: %w", err)
	}
	return nil
}
