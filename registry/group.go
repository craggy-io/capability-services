package registry

import (
	"sort"
	"strings"
)

// Group names a family of capabilities so a catalogue can describe
// them in one line instead of one tool definition each. Membership is
// by name: a capability belongs to the registered group with the
// longest Name that is the capability's name or a dotted prefix of it
// ("platform.scripts" wins over "platform" for platform.scripts.run).
// A capability under no registered group has no group.
type Group struct {
	Name        string `json:"name"`                 // a capability-name prefix: "web", "platform.scripts"
	Description string `json:"description"`          // one line: what the group's capabilities are for
	LoadAlways  bool   `json:"loadAlways,omitempty"` // hosts that load tools on demand advertise it from the start
}

// AddGroup registers groups. It returns an error (and registers
// nothing) for an invalid name, an empty or multi-line description
// (Invalid), or a name already registered with different fields
// (Conflict). Re-registering an identical group is a no-op, so a host
// that reloads an adapter can register its groups again. Groups
// survive Replace and Remove of their capabilities.
func (r *Registry) AddGroup(gs ...Group) error {
	norm := make(map[string]Group, len(gs))
	for _, g := range gs {
		g.Description = strings.TrimSpace(g.Description)
		if !ValidName(g.Name) {
			return Errorf(Invalid, "invalid group name %q: segments of [a-z0-9-] joined by dots", g.Name)
		}
		if g.Description == "" || strings.ContainsAny(g.Description, "\r\n") {
			return Errorf(Invalid, "group %s: needs a single-line description", g.Name)
		}
		if prev, dup := norm[g.Name]; dup && prev != g {
			return Errorf(Conflict, "group %s registered twice with different fields", g.Name)
		}
		norm[g.Name] = g
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.snap.Load()
	changed := false
	for name, g := range norm {
		prev, exists := cur.groups[name]
		if exists && prev != g {
			return Errorf(Conflict, "group %s already registered with different fields", name)
		}
		changed = changed || !exists
	}
	if !changed {
		return nil
	}
	next := *cur
	next.groups = make(map[string]Group, len(cur.groups)+len(norm))
	for k, v := range cur.groups {
		next.groups[k] = v
	}
	for k, v := range norm {
		next.groups[k] = v
	}
	r.snap.Store(&next)
	r.notify()
	return nil
}

// groupOf returns the name of the group name belongs to: the longest
// registered group name that is name or a dotted prefix of it.
func (s *snapshot) groupOf(name string) string {
	for prefix := name; ; {
		if _, ok := s.groups[prefix]; ok {
			return prefix
		}
		i := strings.LastIndexByte(prefix, '.')
		if i < 0 {
			return ""
		}
		prefix = prefix[:i]
	}
}

// Groups returns the registered groups with at least one List()
// entry in this view, sorted by name.
func (v *View) Groups() []Group {
	s := v.reg.snap.Load()
	seen := map[string]bool{}
	var out []Group
	for _, e := range v.List() {
		if e.Group != "" && !seen[e.Group] {
			seen[e.Group] = true
			out = append(out, s.groups[e.Group])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Groups is View(All).Groups on the whole registry.
func (r *Registry) Groups() []Group { return r.allView.Groups() }
