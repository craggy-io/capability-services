package registry

import (
	"context"
	"fmt"
)

// runCheck executes one capability's Check with panic recovery and timeout.
// No Check means always ready.
func runCheck(cfg config, c Capability) (st readyState) {
	if c.Check == nil {
		return readyState{ready: true}
	}
	defer func() {
		if rec := recover(); rec != nil {
			st = readyState{ready: false, err: fmt.Sprintf("check panicked: %v", rec)}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.checkTimeout)
	defer cancel()
	if err := c.Check(ctx); err != nil {
		return readyState{ready: false, err: err.Error()}
	}
	return readyState{ready: true}
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
func (r *Registry) RefreshChecks(ctx context.Context) {
	s := r.snap.Load()
	type result struct {
		k  capKey
		st readyState
	}
	results := make([]result, 0, len(s.caps))
	for k, rc := range s.caps {
		if rc.capab.Check == nil {
			continue
		}
		if ctx != nil && ctx.Err() != nil {
			return
		}
		results = append(results, result{k, runCheck(r.cfg, rc.capab)})
	}
	changed := false
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
