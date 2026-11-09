package registry

import (
	"context"
	"encoding/json"
	"path"
	"sort"
	"time"
)

// View is a live, filtered view of a registry — what projections consume.
// Filtering and enforcement are the same thing: a name outside the view is
// not_found, even when it exists elsewhere in the registry. Versions,
// supersession and readiness are computed registry-wide, then filtered.
type View struct {
	reg    *Registry
	filter Filter
	pin    *Pinned // non-nil: resolve only within the pinned closure
}

// Registry returns the underlying registry.
func (v *View) Registry() *Registry { return v.reg }

// Allows reports whether c is in this view. The filter receives a clone:
// filters are host code run against snapshot capabilities, and mutating
// what they are handed must not rewrite the snapshot (or race readers).
func (v *View) Allows(c Capability) bool {
	return v.filter == nil || v.filter(cloneCapability(c))
}

// Intersect returns a view admitting only what both views admit. The views
// must belong to the same registry; otherwise it returns v unchanged.
func (v *View) Intersect(other *View) *View {
	if other == nil || other.reg != v.reg {
		return v
	}
	pin := v.pin
	if pin == nil {
		pin = other.pin
	}
	return &View{reg: v.reg, filter: And(v.filter, other.filter), pin: pin}
}

// entry describes rc; c is a clone of rc.capab the entry takes ownership of.
func (v *View) entry(s *snapshot, k capKey, rc resolvedCap, c Capability) Entry {
	ready, errMsg := v.reg.readyOf(k, rc.capab.Check != nil)
	return Entry{
		Capability:   c,
		Current:      s.current[k.name] == k.version,
		SupersededBy: append([]string(nil), s.supBy[k]...),
		Group:        s.groupOf(k.name),
		Ready:        ready,
		CheckError:   errMsg,
	}
}

// List returns what callers should use: current, not superseded, in this
// view, sorted by name. Not-ready entries stay in List (with Ready false)
// so listings and health can show why; tool projections leave them out.
func (v *View) List() []Entry {
	all := v.All()
	out := make([]Entry, 0, len(all))
	for _, e := range all {
		if e.Current && !e.Superseded() {
			out = append(out, e)
		}
	}
	return out
}

// All returns everything in this view, including old versions and
// superseded entries, sorted by name then version.
func (v *View) All() []Entry {
	s := v.reg.snap.Load()
	out := make([]Entry, 0, len(s.caps))
	for k, rc := range s.caps {
		// One clone serves both the filter and the entry: neither can
		// reach the snapshot through it.
		c := cloneCapability(rc.capab)
		if v.filter == nil || v.filter(c) {
			out = append(out, v.entry(s, k, rc, c))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// resolve finds the resolved capability for a ref in this view.
func (v *View) resolve(ref string) (capKey, resolvedCap, *snapshot, bool) {
	name, ver, pinned, ok := ParseRef(ref)
	if !ok {
		return capKey{}, resolvedCap{}, nil, false
	}
	if v.pin != nil {
		return v.pin.resolve(v, name, ver, pinned)
	}
	s := v.reg.snap.Load()
	if !pinned {
		cur, exists := s.current[name]
		if !exists {
			return capKey{}, resolvedCap{}, nil, false
		}
		ver = cur
	}
	k := capKey{name, ver}
	rc, exists := s.caps[k]
	if !exists || !v.Allows(rc.capab) {
		return capKey{}, resolvedCap{}, nil, false
	}
	return k, rc, s, true
}

// Get resolves a ref in this view: a bare name yields the current version,
// "name@1" pins. Superseded and old entries are reachable by Get; only
// List hides them.
func (v *View) Get(ref string) (Entry, bool) {
	k, rc, s, ok := v.resolve(ref)
	if !ok {
		return Entry{}, false
	}
	return v.entry(s, k, rc, cloneCapability(rc.capab)), true
}

// Watch fires (coalesced) whenever the registry's List or readiness may
// have changed. Receivers should re-List and diff. The channel closes when
// ctx ends.
func (v *View) Watch(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 1)
	// Capture the current change channel synchronously so a mutation
	// racing Watch's return can't be lost.
	first := v.reg.changedChan()
	go func() {
		defer close(out)
		ch := first
		for {
			select {
			case <-ch:
				// The closed channel was already replaced by notify();
				// capture its successor before waiting again.
				ch = v.reg.changedChan()
				select {
				case out <- struct{}{}:
				default: // already pending: coalesce
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// --- Jobs through a view (Go API: caller is trusted, view is enforced) ---

// Job returns the job with id if its capability is in this view.
func (v *View) Job(id string) (*Job, bool) {
	v.reg.purgeJobs()
	v.reg.jobsMu.RLock()
	j, ok := v.reg.jobs[id]
	v.reg.jobsMu.RUnlock()
	if !ok {
		if j = v.reg.remoteJob(id); j != nil {
			ok = true
		}
	}
	if !ok || !v.Allows(j.capab) {
		return nil, false
	}
	return j, true
}

// Jobs returns statuses of jobs in this view matching f, oldest first.
func (v *View) Jobs(f JobFilter) []JobStatus {
	return v.jobsFiltered(f, Caller{}, false)
}

// JobFor is Job plus the job-access rule for the caller in ctx. With no
// caller in ctx the local caller is trusted and only the view applies.
// Projections that carry caller identity (HTTP, MCP, job capabilities) use
// this; plain Go code may use Job.
func (v *View) JobFor(ctx context.Context, id string) (*Job, bool) {
	j, ok := v.Job(id)
	if !ok {
		return nil, false
	}
	caller, has := CallerFrom(ctx)
	if has && !v.reg.jobAllowed(caller, j.Status()) {
		return nil, false
	}
	return j, true
}

// JobsFor is Jobs plus the job-access rule for the caller in ctx.
func (v *View) JobsFor(ctx context.Context, f JobFilter) []JobStatus {
	caller, has := CallerFrom(ctx)
	return v.jobsFiltered(f, caller, has)
}

func (v *View) jobsFiltered(f JobFilter, caller Caller, checkAccess bool) []JobStatus {
	v.reg.purgeJobs()
	v.reg.jobsMu.RLock()
	jobs := make([]*Job, 0, len(v.reg.jobs))
	local := make(map[string]bool, len(v.reg.jobs))
	for id, j := range v.reg.jobs {
		jobs = append(jobs, j)
		local[id] = true
	}
	v.reg.jobsMu.RUnlock()
	jobs = append(jobs, v.reg.remoteJobs(local)...)
	out := []JobStatus{}
	for _, j := range jobs {
		if !v.Allows(j.capab) {
			continue
		}
		if f.Capability != "" {
			okName, err := path.Match(f.Capability, j.capab.Name)
			if err != nil || !okName {
				if f.Capability != j.capab.Name && f.Capability != j.capab.Ref() {
					continue
				}
			}
		}
		st := j.Status()
		if f.State != "" && st.State != f.State {
			continue
		}
		if checkAccess && !v.reg.jobAllowed(caller, st) {
			continue
		}
		out = append(out, st)
	}
	sortJobStatuses(out)
	return out
}

func (r *Registry) jobAllowed(caller Caller, st JobStatus) bool {
	if r.cfg.jobAccess != nil {
		return r.cfg.jobAccess(caller, st)
	}
	return defaultJobAccess(caller, st)
}

// defaultJobAccess: a caller sees jobs from its own Session and from
// sessions it started (the job caller's Parent chain names this caller by
// Session or ID).
func defaultJobAccess(caller Caller, st JobStatus) bool {
	if caller.Session != "" && caller.Session == st.Caller.Session {
		return true
	}
	if caller.ID != "" && caller.ID == st.Caller.ID && caller.Session == st.Caller.Session {
		return true
	}
	for p, depth := st.Caller.Parent, 0; p != nil && depth < 32; p, depth = p.Parent, depth+1 {
		if caller.Session != "" && p.Session == caller.Session {
			return true
		}
		if caller.ID != "" && p.ID == caller.ID {
			return true
		}
	}
	return false
}

func (r *Registry) purgeJobs() {
	if r.cfg.keepJobs <= 0 {
		return
	}
	cutoff := time.Now().Add(-r.cfg.keepJobs)
	r.jobsMu.Lock()
	purged := false
	for id, j := range r.jobs {
		j.mu.Lock()
		expired := j.state.Terminal() && !j.ended.IsZero() && j.ended.Before(cutoff)
		j.mu.Unlock()
		if expired {
			delete(r.jobs, id)
			purged = true
		}
	}
	// Compact the journal too, or it would grow for the whole process
	// lifetime and resurrect every expired job at the next restart.
	if !purged || r.jobJournalPath == "" {
		r.jobsMu.Unlock()
		return
	}
	records := r.jobRecordsLocked()
	seqs := make(map[string]int64, len(records))
	for _, rec := range records {
		seqs[rec.Status.ID] = rec.Status.Seq
	}
	// Rewrite only if no job appended a newer record after the snapshot:
	// with jobJournalMu held, later persists append after the rewrite; a
	// job mid-emit (its seq moved, or its lock busy inside persist) makes
	// us skip this round instead of clobbering its record — or deadlocking
	// against persistJob's job.mu -> jobJournalMu order.
	r.jobJournalMu.Lock()
	stale := false
	for id, j := range r.jobs {
		if !j.mu.TryLock() {
			stale = true
			break
		}
		seq := j.seq
		j.mu.Unlock()
		if seq != seqs[id] {
			stale = true
			break
		}
	}
	if !stale {
		if err := writeJobJournalFile(r.jobJournalPath, records); err != nil && r.jobJournalErr == nil {
			r.jobJournalErr = err
		}
	}
	r.jobJournalMu.Unlock()
	r.jobsMu.Unlock()
}

// --- Registry acting as View(All) ---

func (r *Registry) List() []Entry                { return r.allView.List() }
func (r *Registry) All() []Entry                 { return r.allView.All() }
func (r *Registry) Get(ref string) (Entry, bool) { return r.allView.Get(ref) }
func (r *Registry) Job(id string) (*Job, bool)   { return r.allView.Job(id) }
func (r *Registry) Jobs(f JobFilter) []JobStatus { return r.allView.Jobs(f) }
func (r *Registry) Watch(ctx context.Context) <-chan struct{} {
	return r.allView.Watch(ctx)
}
func (r *Registry) Invoke(ctx context.Context, ref string, in json.RawMessage) (json.RawMessage, error) {
	return r.allView.Invoke(ctx, ref, in)
}
func (r *Registry) Start(ctx context.Context, ref string, in json.RawMessage) (*Job, error) {
	return r.allView.Start(ctx, ref, in)
}
