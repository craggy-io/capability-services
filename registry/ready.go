package registry

import (
	"context"
	"fmt"
)

// runCheck executes one capability's Check with panic recovery and a hard
// timeout: the Check runs on its own goroutine, so one that ignores its
// context is still bounded by CheckTimeout — treated as a failed check
// while the abandoned goroutine runs on (it can never be killed; that is
// the Check author's leak, not a wedge for registration or Close).
// No Check means always ready.
func (r *Registry) runCheck(c Capability) readyState {
	cfg := r.cfg
	if !hasCheck(c) {
		return readyState{ready: true}
	}
	// Derived from stopCtx: Close cancels the wait below, which is what
	// lets Close-from-inside-a-Check complete.
	ctx, cancel := context.WithTimeout(r.stopCtx, cfg.checkTimeout)
	defer cancel()
	done := make(chan readyState, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- readyState{ready: false, err: fmt.Sprintf("check panicked: %v", rec)}
			}
		}()
		if err := r.checkCapability(ctx, c); err != nil {
			done <- readyState{ready: false, err: err.Error()}
			return
		}
		done <- readyState{ready: true}
	}()
	select {
	case st := <-done:
		return st
	case <-ctx.Done():
		return readyState{ready: false, err: fmt.Sprintf("readiness check did not return within %s", cfg.checkTimeout)}
	}
}

func (r *Registry) readyOf(k capKey, hasCheck bool) (bool, string) {
	if !hasCheck {
		return true, ""
	}
	r.readyMu.RLock()
	defer r.readyMu.RUnlock()
	st, ok := r.ready[k]
	if !ok {
		// Check hasn't completed yet (shouldn't happen: Add runs it
		// synchronously); treat as not ready rather than guessing.
		return false, "readiness check has not run yet"
	}
	return st.ready, st.err
}

// RefreshChecks re-runs every capability's Check now and notifies watchers
// if any readiness changed. The background loop calls it every CheckEvery.
// Results are applied only while the snapshot they were computed from is
// still current: a concurrent Replace already ran fresh checks for its
// capabilities, and a slow refresh must not overwrite them (or resurrect
// removed capabilities) with stale results keyed only by name@version.
func (r *Registry) RefreshChecks(ctx context.Context) {
	r.refreshChecks(ctx, func(Capability) bool { return true })
}

// RefreshDependency rechecks capabilities that declare the named external
// dependency. Hosts call this after changing a setting or resource state so
// tool catalogues update without waiting for the periodic refresh.
func (r *Registry) RefreshDependency(ctx context.Context, kind, name string) {
	r.refreshChecks(ctx, func(c Capability) bool {
		for _, d := range c.Dependencies {
			if d.Kind == kind && d.Name == name {
				return true
			}
		}
		return false
	})
}

func (r *Registry) refreshChecks(ctx context.Context, include func(Capability) bool) {
	s := r.snap.Load()
	type result struct {
		k  capKey
		st readyState
	}
	results := make([]result, 0, len(s.caps))
	for k, rc := range s.caps {
		if !hasCheck(rc.capab) || !include(rc.capab) {
			continue
		}
		if ctx != nil && ctx.Err() != nil {
			return
		}
		results = append(results, result{k, r.runCheck(rc.capab)})
	}
	if r.snap.Load() != s {
		return // superseded by a publish that ran its own checks
	}
	changed := false
	// Serialise with publishLocked (which installs its own results under
	// writeMu), and re-check the snapshot under that lock.
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if r.snap.Load() != s {
		return
	}
	r.readyMu.Lock()
	for _, res := range results {
		if old, ok := r.ready[res.k]; !ok || old != res.st {
			r.ready[res.k] = res.st
			changed = true
		}
	}
	r.readyMu.Unlock()
	if changed {
		r.notify()
	}
}
