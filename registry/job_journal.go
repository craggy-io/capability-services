package registry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// jobRecord is the durable projection of one job. The journal appends the
// latest record after each state or progress event; startup keeps the newest
// record per id. Handler goroutines cannot be resumed after a process exits,
// so restored non-terminal jobs are explicitly marked lost while preserving
// their last progress and caller identity.
type jobRecord struct {
	Status JobStatus       `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("registry: create job journal directory: %w", err)
	}
	latest, err := readJobJournal(path)
	if err != nil {
		return err
	}
	r.jobJournalPath = path
	for id, rec := range latest {
		job, err := restoredJob(rec, r.persistJob)
		if err != nil {
			return fmt.Errorf("registry: restore job %s: %w", id, err)
		}
		r.jobs[id] = job
		advanceJobCounter(id)
	}
	return r.rewriteJobJournalLocked()
}

// JobJournalError reports a write failure encountered after the journal was
// opened. Jobs keep running, but the host can expose this as a health failure
// instead of claiming restart recovery is current.
func (r *Registry) JobJournalError() error {
	r.jobJournalMu.Lock()
	defer r.jobJournalMu.Unlock()
	return r.jobJournalErr
}

func readJobJournal(path string) (map[string]jobRecord, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]jobRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: open job journal: %w", err)
	}
	defer f.Close()
	latest := map[string]jobRecord{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for line := 1; sc.Scan(); line++ {
		var rec jobRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("registry: job journal line %d: %w", line, err)
		}
		if rec.Status.ID == "" {
			return nil, fmt.Errorf("registry: job journal line %d has no job id", line)
		}
		latest[rec.Status.ID] = rec
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("registry: read job journal: %w", err)
	}
	return latest, nil
}

func restoredJob(rec jobRecord, persist func(jobRecord)) (*Job, error) {
	name, version, pinned, ok := ParseRef(rec.Status.Capability)
	if !ok || !pinned {
		return nil, fmt.Errorf("invalid capability %q", rec.Status.Capability)
	}
	status := rec.Status
	if !status.State.Terminal() {
		status.State = JobLost
		status.Ended = time.Now()
		status.Error = &ErrorInfo{Code: Internal, Reason: "process_restarted",
			Message: "the process restarted before the job completed", Retryable: false}
		rec.Result = nil
	}
	done := make(chan struct{})
	close(done)
	j := &Job{id: status.ID, capab: Capability{Name: name, Version: version}, caller: status.Caller,
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

func (r *Registry) persistJob(rec jobRecord) {
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

// rewriteJobJournalLocked compacts the journal to one record per current job.
// The caller holds jobsMu and no job can start while the replacement occurs.
func (r *Registry) rewriteJobJournalLocked() error {
	return r.rewriteJobJournal(r.jobRecordsLocked())
}

func (r *Registry) jobRecordsLocked() []jobRecord {
	records := make([]jobRecord, 0, len(r.jobs))
	for _, job := range r.jobs {
		job.mu.Lock()
		records = append(records, job.recordLocked())
		job.mu.Unlock()
	}
	return records
}

func (r *Registry) rewriteJobJournal(records []jobRecord) error {
	if r.jobJournalPath == "" {
		return nil
	}
	f, err := os.OpenFile(r.jobJournalPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("registry: create compacted job journal: %w", err)
	}
	enc := json.NewEncoder(f)
	for _, rec := range records {
		if err = enc.Encode(rec); err != nil {
			break
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("registry: compact job journal: %w", err)
	}
	return nil
}
