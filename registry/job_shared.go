package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// SharedJobs is a job table shared by every instance of one service, so a job
// started on one instance can be followed, waited for and cancelled from any
// other. Implementations keep the newest record per job and never let a
// terminal record be replaced. See docs/jobs.md, "Shared jobs".
type SharedJobs interface {
	// Put stores rec unless the stored record is terminal or has a higher Seq.
	Put(ctx context.Context, rec JobRecord) error
	// Get returns the newest record for id.
	Get(ctx context.Context, id string) (JobRecord, bool, error)
	// List returns the records of jobs that are unfinished or ended within
	// keep (every finished job if keep <= 0).
	List(ctx context.Context, keep time.Duration) ([]JobRecord, error)
	// Claim moves a queued job to running and reports whether this caller
	// won it; a task delivered twice runs once.
	Claim(ctx context.Context, id string) (bool, error)
	// RequestCancel marks id for cancellation; CancelRequested reads the mark.
	RequestCancel(ctx context.Context, id string) error
	CancelRequested(ctx context.Context, id string) (bool, error)
}

// Task is one long job handed to a Dispatcher: everything RunTask needs to run
// it on whichever instance receives it.
type Task struct {
	Job        string          `json:"job"`
	Capability string          `json:"capability"` // exact name@version
	Input      json.RawMessage `json:"input"`
	Caller     Caller          `json:"caller"`
	Parent     string          `json:"parent,omitempty"`
}

// Dispatcher hands a long job to a queue (Cloud Tasks, in the cloud) that
// delivers it to RunTask inside a request, so the job keeps CPU for as long as
// it runs. Dispatch must not run the job itself.
type Dispatcher interface {
	Dispatch(ctx context.Context, t Task) error
}

// ShareJobs keeps every job in store, so any instance sharing it can follow
// any job. Job IDs become random. It cannot be combined with OpenJobJournal.
func ShareJobs(store SharedJobs) Option {
	return Option{regFn: func(c *config) { c.shared = store }}
}

// DispatchLong sends every Long capability's job to d instead of running it on
// a goroutine of this process; RunTask runs it where d delivers it. It needs
// ShareJobs. Quick capabilities still run in the calling request.
func DispatchLong(d Dispatcher) Option {
	return Option{regFn: func(c *config) { c.dispatcher = d }}
}

// sharedTimeout bounds one shared-store call made from a job's own path.
const sharedTimeout = 5 * time.Second

// putEvery is the most often a running job's progress is written to the
// shared store; state changes are always written.
const putEvery = time.Second

// pollEvery is how often a job held by another instance is re-read.
const pollEvery = 250 * time.Millisecond

func randomJobID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "job-" + hex.EncodeToString(b[:])
}

func (r *Registry) newJobIDFor() string {
	if r.cfg.shared != nil {
		return randomJobID()
	}
	return newJobID()
}

// JobStoreError reports the first failed write to the shared job store. Jobs
// keep running; a host can expose it as a health failure.
func (r *Registry) JobStoreError() error {
	r.jobJournalMu.Lock()
	defer r.jobJournalMu.Unlock()
	return r.sharedErr
}

// putShared writes rec to the shared store, coalescing a running job's progress
// to one write per putEvery.
func (r *Registry) putShared(rec JobRecord) {
	r.jobJournalMu.Lock()
	id, state := rec.Status.ID, rec.Status.State
	last, seen := r.sharedPut[id]
	if seen && !state.Terminal() && last.state == state && time.Since(last.at) < putEvery {
		r.jobJournalMu.Unlock()
		return
	}
	if state.Terminal() {
		delete(r.sharedPut, id)
	} else {
		r.sharedPut[id] = sharedPutMark{state: state, at: time.Now()}
	}
	r.jobJournalMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	if err := r.cfg.shared.Put(ctx, rec); err != nil {
		r.jobJournalMu.Lock()
		if r.sharedErr == nil {
			r.sharedErr = err
		}
		r.jobJournalMu.Unlock()
	}
}

type sharedPutMark struct {
	state JobState
	at    time.Time
}

// remoteJob returns a handle on a job another instance holds, or nil.
func (r *Registry) remoteJob(id string) *Job {
	if r.cfg.shared == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	rec, ok, err := r.cfg.shared.Get(ctx, id)
	if err != nil || !ok {
		return nil
	}
	j, err := recordJob(rec, nil)
	if err != nil {
		return nil
	}
	j.remote = r.cfg.shared
	j.done = make(chan struct{})
	j.apply(rec)
	return j
}

// remoteJobs lists the records of jobs this process doesn't hold.
func (r *Registry) remoteJobs(local map[string]bool) []*Job {
	if r.cfg.shared == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	recs, err := r.cfg.shared.List(ctx, r.cfg.keepJobs)
	if err != nil {
		return nil
	}
	out := make([]*Job, 0, len(recs))
	for _, rec := range recs {
		if local[rec.Status.ID] {
			continue
		}
		j, err := recordJob(rec, nil)
		if err != nil {
			continue
		}
		j.remote = r.cfg.shared
		j.done = make(chan struct{})
		j.apply(rec)
		out = append(out, j)
	}
	return out
}

// apply loads rec into a remote handle, closing done once it is terminal.
func (j *Job) apply(rec JobRecord) {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := rec.Status
	if st.Seq < j.seq {
		return
	}
	j.state, j.last, j.seq, j.heartbeat, j.ended, j.errInfo = st.State, st.Last, st.Seq, st.Heartbeat, st.Ended, st.Error
	j.result = append(json.RawMessage(nil), rec.Result...)
	if st.State.Terminal() {
		select {
		case <-j.done:
		default:
			close(j.done)
		}
	}
}

// refresh re-reads a remote handle's record; a failed read keeps the snapshot.
func (j *Job) refresh(ctx context.Context) {
	if j.remote == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	if rec, ok, err := j.remote.Get(ctx, j.id); err == nil && ok {
		j.apply(rec)
	}
}

// follow re-reads a remote handle until it is terminal or ctx ends, calling
// changed after each read that moved its Seq.
func (j *Job) follow(ctx context.Context, changed func()) {
	for {
		j.mu.Lock()
		seq := j.seq
		j.mu.Unlock()
		j.refresh(ctx)
		j.mu.Lock()
		moved := j.seq != seq
		j.mu.Unlock()
		if moved && changed != nil {
			changed()
		}
		select {
		case <-j.done:
			return
		case <-ctx.Done():
			return
		case <-time.After(pollEvery):
		}
	}
}

// remoteEvents streams a remote handle's status changes: one event per read
// that moved its Seq. Intermediate events between two reads are not replayed.
func (j *Job) remoteEvents(ctx context.Context) <-chan Event {
	out := make(chan Event, 16)
	snapshot := func() Event {
		j.mu.Lock()
		defer j.mu.Unlock()
		ev := Event{Seq: j.seq, Time: j.heartbeat, State: j.state, Error: cloneErrorInfo(j.errInfo)}
		if j.last.Stage != "" || j.last.Message != "" || j.last.Total != 0 || j.last.Done != 0 {
			u := cloneUpdate(j.last)
			ev.Update = &u
		}
		if ev.State.Terminal() {
			ev.Time = j.ended
		}
		return ev
	}
	go func() {
		defer close(out)
		send := func() bool {
			select {
			case out <- snapshot():
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !send() {
			return
		}
		fctx, stop := context.WithCancel(ctx)
		defer stop()
		j.follow(fctx, func() {
			if !send() {
				stop()
			}
		})
	}()
	return out
}

// watchCancel cancels a local job when another instance asks for it through
// the shared store. It returns when the job is done.
func (r *Registry) watchCancel(job *Job) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-job.done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
			asked, err := r.cfg.shared.CancelRequested(ctx, job.id)
			cancel()
			if err == nil && asked {
				job.Cancel()
				return
			}
		}
	}
}

// dispatch records a queued long job in the shared store and hands it to the
// dispatcher. The returned handle follows it wherever it runs.
func (v *View) dispatch(job *Job, input json.RawMessage) (*Job, error) {
	r := v.reg
	job.mu.Lock()
	job.emitLocked(Event{Time: job.started, State: JobQueued})
	rec := job.recordLocked()
	job.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	if err := r.cfg.shared.Put(ctx, rec); err != nil {
		return nil, WrapCode(Unavailable, err)
	}
	task := Task{Job: job.id, Capability: job.capab.Ref(), Input: input, Caller: cloneCaller(job.caller), Parent: job.parent}
	if err := r.cfg.dispatcher.Dispatch(ctx, task); err != nil {
		job.finish(nil, WrapCode(Unavailable, err))
		r.putShared(job.record())
		return nil, WrapCode(Unavailable, err)
	}
	handle := r.remoteJob(job.id)
	if handle == nil {
		return nil, Errorf(Unavailable, "%s: the job store lost job %s", job.capab.Ref(), job.id)
	}
	return handle, nil
}

func (j *Job) record() JobRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.recordLocked()
}

// ErrTaskNotQueued is returned by RunTask for a task whose job already ran,
// is running elsewhere, or was cancelled before it started: the queue should
// treat the delivery as done.
var ErrTaskNotQueued = errors.New("registry: the task's job is not queued")

// RunTask runs a dispatched job to its end, in the calling request. Call it
// from the endpoint the dispatcher delivers to, after authenticating that the
// delivery came from the queue. The job runs under the registry's full view
// with the caller recorded at Start; Start already applied the starting
// view. A task delivered again after its job was claimed returns
// ErrTaskNotQueued without running.
func (r *Registry) RunTask(ctx context.Context, t Task) error {
	if r.cfg.shared == nil {
		return Errorf(Unavailable, "registry: RunTask needs ShareJobs")
	}
	rec, ok, err := r.cfg.shared.Get(ctx, t.Job)
	if err != nil {
		return WrapCode(Unavailable, err)
	}
	if !ok || rec.Status.State != JobQueued {
		return ErrTaskNotQueued
	}
	won, err := r.cfg.shared.Claim(ctx, t.Job)
	if err != nil {
		return WrapCode(Unavailable, err)
	}
	if !won {
		return ErrTaskNotQueued
	}
	ev := r.allView
	_, rc, _, found := ev.resolve(t.Capability)
	if !found || rc.capab.Ref() != t.Capability {
		err := Errorf(NotFound, "capability %q is not on this instance", t.Capability)
		r.failRecord(rec, err)
		return nil
	}
	input, err := normalizeInput(rc, t.Input)
	if err != nil {
		r.failRecord(rec, err)
		return nil
	}
	handler, err := buildChain(r.cfg.middleware, rc.capab, rc.capab.Handler)
	if err != nil {
		r.failRecord(rec, err)
		return nil
	}
	job := &Job{
		id: t.Job, capab: rc.capab, supBy: rec.Status.SupersededBy, caller: cloneCaller(t.Caller), parent: t.Parent,
		done: make(chan struct{}), state: JobQueued, seq: rec.Status.Seq,
		started: rec.Status.Started, hbDur: heartbeatOf(rc.capab), subs: map[chan Event]struct{}{}, persist: r.persistJob,
		history: []Event{{Seq: rec.Status.Seq, Time: rec.Status.Started, State: JobQueued}},
	}
	job.heartbeat = time.Now()
	base := WithCaller(context.WithoutCancel(ctx), job.caller)
	jctx, cancel := context.WithCancel(base)
	job.cancel = cancel
	jctx = context.WithValue(jctx, jobKey, &jobCtx{id: job.id, progress: job.reportProgress, life: jctx})
	jctx = WithView(jctx, ev)
	// The request's own end (the queue's deadline) cancels the job.
	stop := context.AfterFunc(ctx, job.Cancel)
	defer stop()

	r.jobsMu.Lock()
	if r.closed {
		r.jobsMu.Unlock()
		cancel()
		r.failRecord(rec, ReasonErrorf(Unavailable, ReasonClosed, "the registry is closed"))
		return nil
	}
	r.jobs[job.id] = job
	r.runners.add()
	r.jobsMu.Unlock()
	defer r.runners.done()
	r.runJob(jctx, job, rc, handler, input)
	return nil
}

// failRecord ends a claimed job that could not start.
func (r *Registry) failRecord(rec JobRecord, err error) {
	rec.Status.State = JobFailed
	rec.Status.Seq++
	rec.Status.Ended = time.Now()
	rec.Status.Error = Info(err)
	rec.Result = nil
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	if perr := r.cfg.shared.Put(ctx, rec); perr != nil {
		r.jobJournalMu.Lock()
		if r.sharedErr == nil {
			r.sharedErr = perr
		}
		r.jobJournalMu.Unlock()
	}
}

func heartbeatOf(c Capability) time.Duration {
	if c.Heartbeat > 0 {
		return c.Heartbeat
	}
	return 30 * time.Second
}
