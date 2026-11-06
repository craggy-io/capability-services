package registry

import (
	"context"
	"sort"
	"sync"
)

// ReasonUndeclared is the reason on the Denied error for a call through a
// pinned view to a capability outside its pinned closure.
const ReasonUndeclared = "undeclared"

// Identity names one capability implementation: what a pinned evaluation
// resolved or called. Origin.Digest may be empty when the provider did not
// say; certifying hosts refuse that themselves.
type Identity struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Origin  Origin `json:"origin"`
}

// Ref is the identity's "name@version".
func (i Identity) Ref() string { return refOf(i.Name, i.Version) }

// Pinned is a fixed closure of capabilities: the refs given to View.Pin and,
// transitively, everything each one Requires, resolved once. Calls through
// its View run exactly those implementations, even after the registry
// replaces or removes them, and any other ref fails Denied with reason
// ReasonUndeclared. Nested calls made by a pinned capability's handler run
// under the same closure, because the handler's view carries the pin.
type Pinned struct {
	reg   *Registry
	snap  *snapshot // supersession facts at pin time
	caps  map[capKey]resolvedCap
	names map[string]int // name -> the one pinned version
	view  *View

	mu    sync.Mutex
	calls map[capKey]bool
}

// Pin resolves refs in this view, then each resolved capability's Requires,
// until the closure is complete. A ref that does not resolve fails NotFound
// naming it and the capability that required it. A closure may hold one
// version per name.
func (v *View) Pin(refs ...string) (*Pinned, error) {
	p := &Pinned{reg: v.reg, snap: v.reg.snap.Load(), caps: map[capKey]resolvedCap{}, names: map[string]int{}, calls: map[capKey]bool{}}
	type want struct{ ref, by string }
	queue := make([]want, 0, len(refs))
	for _, r := range refs {
		queue = append(queue, want{r, ""})
	}
	base := &View{reg: v.reg, filter: v.filter}
	for len(queue) > 0 {
		w := queue[0]
		queue = queue[1:]
		k, rc, _, ok := base.resolveIn(p.snap, w.ref)
		if !ok {
			if w.by != "" {
				return nil, Errorf(NotFound, "pin: %s requires %q, which does not resolve", w.by, w.ref)
			}
			return nil, Errorf(NotFound, "pin: capability %q not found", w.ref)
		}
		if have, dup := p.names[k.name]; dup {
			if have != k.version {
				return nil, Errorf(Conflict, "pin: %s@%d and %s@%d are both required; a closure pins one version per name", k.name, have, k.name, k.version)
			}
			continue
		}
		p.names[k.name] = k.version
		p.caps[k] = rc
		for _, r := range rc.capab.Requires {
			queue = append(queue, want{r, rc.capab.Ref()})
		}
	}
	p.view = &View{reg: v.reg, filter: v.filter, pin: p}
	return p, nil
}

// resolveIn is resolve against a given snapshot, ignoring any pin.
func (v *View) resolveIn(s *snapshot, ref string) (capKey, resolvedCap, *snapshot, bool) {
	name, ver, pinned, ok := ParseRef(ref)
	if !ok {
		return capKey{}, resolvedCap{}, nil, false
	}
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

// View is the view calls under this closure go through: the pinning view's
// filter, restricted to the closure. Intersecting it with another view
// keeps the pin.
func (p *Pinned) View() *View { return p.view }

// Identities lists the whole closure, sorted by name.
func (p *Pinned) Identities() []Identity {
	out := make([]Identity, 0, len(p.caps))
	for _, rc := range p.caps {
		out = append(out, identityOf(rc.capab))
	}
	sortIdentities(out)
	return out
}

// Calls lists the closure entries actually started through the pin so far,
// sorted by name.
func (p *Pinned) Calls() []Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Identity, 0, len(p.calls))
	for k := range p.calls {
		out = append(out, identityOf(p.caps[k].capab))
	}
	sortIdentities(out)
	return out
}

func identityOf(c Capability) Identity {
	return Identity{Name: c.Name, Version: normVersion(c.Version), Origin: c.Origin}
}

func sortIdentities(ids []Identity) {
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Name != ids[j].Name {
			return ids[i].Name < ids[j].Name
		}
		return ids[i].Version < ids[j].Version
	})
}

// resolve finds name in the closure under v's filter. A bare name resolves
// to the pinned version; a versioned ref must name it exactly.
func (p *Pinned) resolve(v *View, name string, ver int, versioned bool) (capKey, resolvedCap, *snapshot, bool) {
	have, ok := p.names[name]
	if !ok || versioned && ver != have {
		return capKey{}, resolvedCap{}, nil, false
	}
	k := capKey{name, have}
	rc := p.caps[k]
	if !v.Allows(rc.capab) {
		return capKey{}, resolvedCap{}, nil, false
	}
	return k, rc, p.snap, true
}

// declares reports whether ref names a closure entry, whatever the view.
func (p *Pinned) declares(ref string) bool {
	name, ver, versioned, ok := ParseRef(ref)
	if !ok {
		return false
	}
	have, in := p.names[name]
	return in && (!versioned || ver == have)
}

func (p *Pinned) record(k capKey) {
	p.mu.Lock()
	p.calls[k] = true
	p.mu.Unlock()
}

// readyOf reports readiness for a resolved capability. A pinned entry the
// registry no longer holds as-is runs its own Check now: pinning fixes the
// implementation, never skips readiness.
func (v *View) readyOf(ctx context.Context, k capKey, rc resolvedCap) (bool, string) {
	if v.pin != nil && rc.capab.Check != nil {
		if cur, ok := v.reg.snap.Load().caps[k]; !ok || !sameHandler(cur, rc) {
			if err := rc.capab.Check(ctx); err != nil {
				return false, err.Error()
			}
			return true, ""
		}
	}
	return v.reg.readyOf(k, rc.capab.Check != nil)
}

// sameHandler reports whether two resolved entries are the same registration.
func sameHandler(a, b resolvedCap) bool { return a.seq == b.seq }
