package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Update is one progress report. Done and Total describe only the named
// stage, in Unit. They are never whole-job progress: Done == Total means
// the stage finished, not the job. A stage change may reset the counters or
// change the unit. Total == 0 means the stage total is unknown and no
// percentage is available.
type Update struct {
	Stage   string `json:"stage,omitempty"`   // "pull-image", "decompile", "store-output"
	Message string `json:"message,omitempty"` //
	Done    int64  `json:"done,omitempty"`    // completed units in this stage
	Total   int64  `json:"total,omitempty"`   // stage total; 0 if unknown
	Unit    string `json:"unit,omitempty"`    // "bytes", "files", "steps", ""; shared by Done and Total
	Data    any    `json:"data,omitempty"`    // optional structured detail
}

func validUpdate(u Update) bool {
	if u.Done < 0 || u.Total < 0 {
		return false
	}
	if u.Total > 0 && u.Done > u.Total {
		return false
	}
	return true
}

// Percent returns the stage completion percentage, or -1 when the stage
// total is unknown. It is never a whole-job percentage.
func (u Update) Percent() float64 {
	if u.Total <= 0 {
		return -1
	}
	return float64(u.Done) * 100 / float64(u.Total)
}

// JobState is the lifecycle state of a job. Only the job runner sets a
// terminal state (succeeded, failed, cancelled, lost), and only after the
// full handler chain returns and result encoding/validation succeeds.
type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobStalled   JobState = "stalled"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
	JobLost      JobState = "lost"
)

// Terminal reports whether the state is final.
func (s JobState) Terminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobCancelled, JobLost:
		return true
	}
	return false
}

// JobStatus is a point-in-time snapshot of a job.
type JobStatus struct {
	ID           string     `json:"id"`
	Capability   string     `json:"capability"` // exact name@version
	Caller       Caller     `json:"caller"`
	Parent       string     `json:"parent,omitempty"` // job whose handler started this one; cancelling it cancels this
	State        JobState   `json:"state"`
	Last         Update     `json:"last"`                   // latest progress
	Seq          int64      `json:"seq"`                    // monotonic per-job event sequence
	SupersededBy []string   `json:"supersededBy,omitempty"` // notice at start time, for audit
	Started      time.Time  `json:"started"`
	Heartbeat    time.Time  `json:"heartbeat"` // time of the last Progress
	Ended        time.Time  `json:"ended,omitempty"`
	Error        *ErrorInfo `json:"error,omitempty"` // code, reason + message once failed/cancelled
}

// Event is one entry in a job's history: a state change or a progress
// update. Seq increases monotonically from 1. Replay always ends with the
// terminal event for a finished job. Replay keeps only the latest of
// consecutive progress updates for one stage, so its Seqs can skip;
// live subscribers see every update.
type Event struct {
	Seq       int64      `json:"seq"`
	Time      time.Time  `json:"time"`
	State     JobState   `json:"state"`
	Update    *Update    `json:"update,omitempty"` // set for progress events
	Error     *ErrorInfo `json:"error,omitempty"`
	resultSet bool
}

// JobFilter selects jobs in View.Jobs. Empty fields match everything.
// Capability is a name glob (same rules as Match).
type JobFilter struct {
	State      JobState
	Capability string
}

// Job is a running or finished call. Every Invoke and Start creates one;
// for quick capabilities it lives for milliseconds.
type Job struct {
	id     string
	capab  Capability // snapshot copy taken at start
	supBy  []string
	caller Caller
	parent string // parent job id, "" for a top-level call

	cancel    context.CancelFunc
	cancelReq atomic.Bool
	done      chan struct{}

	mu        sync.Mutex
	state     JobState // stored state; stalled is derived in Status
	last      Update
	seq       int64
	started   time.Time
	heartbeat time.Time
	ended     time.Time
	errInfo   *ErrorInfo
	result    json.RawMessage
	history   []Event
	subs      map[chan Event]struct{}
	persist   func(jobRecord)

	hbDur time.Duration
}

// ID returns the job ID.
func (j *Job) ID() string { return j.id }

// Capability returns the exact "name@version" that is running.
func (j *Job) Capability() string { return j.capab.Ref() }

// Status returns a snapshot. A running job whose last heartbeat is older
// than its capability's Heartbeat reports stalled; it flips back to running
// on the next update.
func (j *Job) Status() JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.statusLocked(time.Now())
}

func (j *Job) statusLocked(now time.Time) JobStatus {
	st := j.state
	if st == JobRunning && j.hbDur > 0 && now.Sub(j.heartbeat) > j.hbDur {
		st = JobStalled
	}
	return JobStatus{
		ID: j.id, Capability: j.capab.Ref(), Caller: j.caller, Parent: j.parent, State: st,
		Last: j.last, Seq: j.seq, SupersededBy: append([]string(nil), j.supBy...),
		Started: j.started, Heartbeat: j.heartbeat, Ended: j.ended, Error: j.errInfo,
	}
}

// Result returns the encoded result if the job succeeded.
func (j *Job) Result() (json.RawMessage, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != JobSucceeded {
		return nil, false
	}
	return append(json.RawMessage(nil), j.result...), true
}

// Wait blocks until the job reaches a terminal state or ctx ends. A ctx
// that ends leaves the job running; only Cancel stops it.
func (j *Job) Wait(ctx context.Context) (json.RawMessage, error) {
	select {
	case <-j.done:
	case <-ctx.Done():
		return nil, WrapCode(Cancelled, ctx.Err())
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	switch j.state {
	case JobSucceeded:
		return append(json.RawMessage(nil), j.result...), nil
	case JobCancelled:
		e := &Error{Code: Cancelled, Message: infoMessage(j.errInfo, "job cancelled")}
		if j.errInfo != nil {
			e.Reason = j.errInfo.Reason
		}
		return nil, e
	default:
		if j.errInfo != nil {
			return nil, &Error{Code: j.errInfo.Code, Reason: j.errInfo.Reason, Message: j.errInfo.Message}
		}
		return nil, &Error{Code: Internal, Message: "job failed"}
	}
}

func infoMessage(i *ErrorInfo, fallback string) string {
	if i != nil && i.Message != "" {
		return i.Message
	}
	return fallback
}

// Cancel requests cancellation: the handler's context is cancelled. If the
// handler later returns success anyway, the job still ends cancelled —
// cancellation cannot be converted to success.
func (j *Job) Cancel() {
	if j.cancelReq.CompareAndSwap(false, true) && j.cancel != nil {
		j.cancel()
	}
}

// Events streams state changes and updates, replaying from the start. The
// channel closes when the job is terminal and replay is drained, or when
// ctx ends.
func (j *Job) Events(ctx context.Context) <-chan Event {
	out := make(chan Event, 16)
	ch := make(chan Event, 64)
	j.mu.Lock()
	hist := append([]Event(nil), j.history...)
	terminal := j.state.Terminal()
	if !terminal {
		if j.subs == nil {
			j.subs = map[chan Event]struct{}{}
		}
		j.subs[ch] = struct{}{}
	}
	j.mu.Unlock()
	go func() {
		defer close(out)
		defer func() {
			j.mu.Lock()
			delete(j.subs, ch)
			j.mu.Unlock()
		}()
		for _, ev := range hist {
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
		if terminal {
			return
		}
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
				if ev.State.Terminal() {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (j *Job) emitLocked(ev Event) {
	j.seq++
	ev.Seq = j.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if n := len(j.history); n > 0 && sameStageProgress(j.history[n-1], ev) {
		// Replay needs each stage's latest progress, not every step: a
		// 200k-file transfer would otherwise keep 200k events.
		j.history[n-1] = ev
	} else {
		j.history = append(j.history, ev)
	}
	for ch := range j.subs {
		select {
		case ch <- ev:
		default:
			// Slow subscriber: drop for this event; Events replay plus
			// Status remain authoritative. Never block the job runner.
		}
	}
	if j.persist != nil {
		j.persist(j.recordLocked())
	}
}

func (j *Job) recordLocked() jobRecord {
	return jobRecord{Status: j.statusLocked(time.Now()), Result: append(json.RawMessage(nil), j.result...)}
}

// sameStageProgress reports whether next only supersedes prev: both are
// progress updates for the same stage in the same state.
func sameStageProgress(prev, next Event) bool {
	return prev.Update != nil && next.Update != nil && prev.Error == nil && next.Error == nil &&
		prev.State == next.State && prev.Update.Stage == next.Update.Stage
}

func (j *Job) reportProgress(u Update) {
	if !validUpdate(u) {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Terminal() {
		return // late callbacks never revive a terminal job
	}
	if j.state == JobQueued {
		j.state = JobRunning
	}
	now := time.Now()
	j.last = u
	j.heartbeat = now
	upd := u
	j.emitLocked(Event{Time: now, State: j.state, Update: &upd})
}

func (j *Job) markRunning() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != JobQueued {
		return
	}
	j.state = JobRunning
	j.heartbeat = time.Now()
	j.emitLocked(Event{State: JobRunning})
}

// finish is the only path to a terminal state. Called by the job runner
// after the handler chain returned and encoding/validation succeeded.
func (j *Job) finish(result json.RawMessage, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Terminal() {
		return
	}
	now := time.Now()
	j.ended = now
	switch {
	case j.cancelReq.Load():
		j.state = JobCancelled
		msg := "job cancelled"
		if err != nil {
			msg = err.Error()
		}
		j.errInfo = &ErrorInfo{Code: Cancelled, Message: msg}
		j.result = nil
	case err != nil:
		if CodeOf(err) == Cancelled {
			j.state = JobCancelled
		} else {
			j.state = JobFailed
		}
		j.errInfo = Info(err)
		j.result = nil
	default:
		j.state = JobSucceeded
		j.result = append(json.RawMessage(nil), result...)
	}
	j.emitLocked(Event{Time: now, State: j.state, Error: j.errInfo})
	for ch := range j.subs {
		close(ch)
		delete(j.subs, ch)
	}
	close(j.done)
}

var jobCounter atomic.Uint64

func newJobID() string {
	return fmt.Sprintf("job-%d", jobCounter.Add(1))
}

func sortJobStatuses(s []JobStatus) {
	sort.Slice(s, func(i, k int) bool {
		if !s[i].Started.Equal(s[k].Started) {
			return s[i].Started.Before(s[k].Started)
		}
		return s[i].ID < s[k].ID
	})
}
