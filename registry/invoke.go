package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"
)

// effectiveView clamps the receiver view to any view the call arrived on:
// a nested call never gets wider access than its caller's view.
func (v *View) effective(ctx context.Context) *View {
	if ctx == nil {
		return v
	}
	if from := ViewFrom(ctx); from != nil && from != v {
		return v.Intersect(from)
	}
	return v
}

func normalizeInput(rc resolvedCap, in json.RawMessage) (json.RawMessage, error) {
	if rc.capab.Input == nil {
		// No input: accept an empty body, null or {} — semantically, so
		// whitespace a client added is fine.
		switch s := string(bytes.TrimSpace(in)); s {
		case "", "null":
			return json.RawMessage("{}"), nil
		default:
			var obj map[string]json.RawMessage
			if json.Unmarshal(in, &obj) == nil && len(obj) == 0 {
				return json.RawMessage("{}"), nil
			}
			return nil, Errorf(Invalid, "%s takes no input", rc.capab.Ref())
		}
	}
	if len(in) == 0 {
		in = json.RawMessage("{}")
	}
	if !json.Valid(in) {
		return nil, Errorf(Invalid, "%s: input is not valid JSON", rc.capab.Ref())
	}
	if err := validateAgainst(rc.inRes, in); err != nil {
		return nil, &Error{Code: Invalid, Message: invalidInputMessage(rc.capab, in, err), Err: err}
	}
	// The runner reads the input later, on its own goroutine: it must own
	// the bytes, not alias a buffer the caller may reuse or mutate.
	return append(json.RawMessage(nil), in...), nil
}

func unwrapMessage(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Message
	}
	return err.Error()
}

func encodeResult(rc resolvedCap, out any, validate bool) (json.RawMessage, error) {
	var raw json.RawMessage
	switch t := out.(type) {
	case nil:
		raw = json.RawMessage("null")
	case json.RawMessage:
		raw = t
	case []byte:
		// A []byte result would marshal as base64; treat it as JSON text
		// only if it parses, otherwise fall through to normal encoding.
		if json.Valid(t) {
			raw = json.RawMessage(t)
		}
	}
	if raw == nil {
		b, err := json.Marshal(out)
		if err != nil {
			return nil, Errorf(Internal, "%s: encoding result: %v", rc.capab.Ref(), err)
		}
		raw = b
	}
	if !json.Valid(raw) {
		return nil, Errorf(Internal, "%s: handler returned invalid JSON", rc.capab.Ref())
	}
	if validate && rc.outRes != nil {
		if err := validateAgainst(rc.outRes, raw); err != nil {
			return nil, Errorf(Internal, "%s: result does not match output schema: %v", rc.capab.Ref(), unwrapMessage(err))
		}
	}
	return raw, nil
}

// Start validates the call, creates a job and runs the handler chain in the
// background. The job context keeps the caller and view but not the
// request's cancellation: jobs outlive the request that started them and
// only Job.Cancel stops them. Input validation, readiness and view
// enforcement all happen here, whichever projection the call came from.
//
// A closed registry (see Registry.Close) starts nothing: Start fails with
// unavailable and reason ReasonClosed.
//
// A job started from inside another job's handler is that job's child:
// JobStatus.Parent names the parent, and cancelling the parent cancels the
// child (and so on down). A parent that finishes normally leaves its
// children running.
// buildChain wraps handler with the middleware (first Use'd outermost).
// Middleware constructors are host code run synchronously in Start, so a
// panic here is recovered as an internal error — the same treatment a
// panicking handler gets — instead of crashing Start's caller.
func buildChain(mw []Middleware, c Capability, handler Handler) (h Handler, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			h, err = nil, Errorf(Internal, "%s: middleware panicked: %v", c.Ref(), rec)
		}
	}()
	c = cloneCapability(c) // middleware inspects a copy, never the snapshot
	for i := len(mw) - 1; i >= 0; i-- {
		if mw[i] != nil {
			handler = mw[i](handler, c)
		}
	}
	return handler, nil
}

func (v *View) Start(ctx context.Context, ref string, in json.RawMessage) (*Job, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Closed first: after Close every Start fails unavailable/closed,
	// whatever the ref or input would otherwise have failed with. The
	// check repeats under jobsMu below for the Close race.
	v.reg.jobsMu.RLock()
	closed := v.reg.closed
	v.reg.jobsMu.RUnlock()
	if closed {
		return nil, ReasonErrorf(Unavailable, ReasonClosed, "%s: the registry is closed", ref)
	}
	ev := v.effective(ctx)
	k, rc, s, ok := ev.resolve(ref)
	if !ok {
		return nil, Errorf(NotFound, "capability %q not found", ref)
	}
	ready, errMsg := v.reg.readyOf(k, rc.capab.Check != nil)
	if !ready {
		if errMsg == "" {
			errMsg = "not ready"
		}
		return nil, ReasonErrorf(Unavailable, ReasonNotReady, "%s: %s", rc.capab.Ref(), errMsg)
	}
	input, err := normalizeInput(rc, in)
	if err != nil {
		return nil, err
	}
	// Capture entry facts now; the running job keeps this capability even
	// if Replace swaps it out mid-flight.
	supBy := append([]string(nil), s.supBy[k]...)
	_ = s

	caller, _ := CallerFrom(ctx)
	caller = cloneCaller(caller) // own it: detach from the caller's maps/chain (and break any cycle)
	hb := rc.capab.Heartbeat
	if hb <= 0 {
		hb = 30 * time.Second
	}
	parent, _ := ctx.Value(jobKey).(*jobCtx)
	job := &Job{
		id: newJobID(), capab: rc.capab, supBy: supBy, caller: caller,
		done: make(chan struct{}), state: JobQueued,
		started: time.Now(), hbDur: hb, subs: map[chan Event]struct{}{}, persist: v.reg.persistJob,
	}
	if parent != nil {
		job.parent = parent.id
	}
	job.heartbeat = job.started

	base := context.WithoutCancel(ctx)
	jctx, cancel := context.WithCancel(base)
	job.cancel = cancel
	jctx = context.WithValue(jctx, jobKey, &jobCtx{id: job.id, progress: job.reportProgress, life: jctx})
	// The handler's view is the effective (clamped) view.
	jctx = WithView(jctx, ev)

	handler, err := buildChain(v.reg.cfg.middleware, rc.capab, rc.capab.Handler)
	if err != nil {
		cancel()
		return nil, err
	}

	// Insert the job and count its runner in one step under jobsMu, the
	// lock Close takes to set closed and snapshot the table: either Close
	// sees this job (and cancels and waits for it) or this Start sees
	// closed and creates nothing.
	v.reg.jobsMu.Lock()
	if v.reg.closed {
		v.reg.jobsMu.Unlock()
		cancel()
		return nil, ReasonErrorf(Unavailable, ReasonClosed, "%s: the registry is closed", rc.capab.Ref())
	}
	v.reg.jobs[job.id] = job
	v.reg.runners.add()
	v.reg.jobsMu.Unlock()

	job.mu.Lock()
	job.emitLocked(Event{Time: job.started, State: JobQueued})
	job.mu.Unlock()
	if parent != nil && parent.life != nil {
		stop := context.AfterFunc(parent.life, job.Cancel)
		go func() {
			<-job.done
			stop()
		}()
	}
	go func() {
		defer v.reg.runners.done()
		v.reg.runJob(jctx, job, rc, handler, input)
	}()
	return job, nil
}

// Invoke is Start + Wait. If ctx ends first, Invoke returns Cancelled but
// the job keeps running (find it via Jobs).
func (v *View) Invoke(ctx context.Context, ref string, in json.RawMessage) (json.RawMessage, error) {
	job, err := v.Start(ctx, ref, in)
	if err != nil {
		return nil, err
	}
	return job.Wait(ctx)
}

func (r *Registry) runJob(ctx context.Context, job *Job, rc resolvedCap, handler Handler, input json.RawMessage) {
	// Acquire per-tag limit semaphores (sorted: no lock-order deadlock).
	tags := make([]string, 0, len(r.sems))
	for _, t := range rc.capab.Tags {
		if _, ok := r.sems[t]; ok {
			tags = append(tags, t)
		}
	}
	sort.Strings(tags)
	tags = slices.Compact(tags) // belt and braces: never acquire one semaphore twice
	acquired := make([]chan struct{}, 0, len(tags))
	release := func() {
		for _, sem := range acquired {
			<-sem
		}
	}
	for _, t := range tags {
		sem := r.sems[t]
		select {
		case sem <- struct{}{}:
			acquired = append(acquired, sem)
		case <-ctx.Done():
			release()
			job.finish(nil, Errorf(Cancelled, "cancelled while queued"))
			return
		}
	}
	defer release()

	job.markRunning()
	var (
		out any
		err error
	)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = Errorf(Internal, "%s: handler panicked: %v", rc.capab.Ref(), rec)
				out = nil
			}
		}()
		out, err = handler(ctx, input)
	}()
	if err != nil {
		job.finish(nil, err)
		return
	}
	raw, encErr := encodeResult(rc, out, r.cfg.validateOutput)
	if encErr != nil {
		job.finish(nil, encErr)
		return
	}
	job.finish(raw, nil)
}
