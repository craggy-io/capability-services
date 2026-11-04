package registry

import "path"

// Filter decides whether a capability belongs in a view. A view is the
// registry filtered by name, tag or effect: policy is a filter, not a fork,
// and nothing is registered twice.
type Filter func(Capability) bool

// All admits every capability. The Registry itself acts as View(All).
func All(Capability) bool { return true }

// Match admits capabilities whose name matches any glob. '*' crosses dots,
// so "storage.*" matches "storage.artifact.get". An invalid glob matches
// nothing.
func Match(globs ...string) Filter {
	return func(c Capability) bool {
		for _, g := range globs {
			if ok, err := path.Match(g, c.Name); err == nil && ok {
				return true
			}
		}
		return false
	}
}

// Tagged admits capabilities carrying any of the given tags.
func Tagged(tags ...string) Filter {
	return func(c Capability) bool {
		for _, t := range tags {
			if c.HasTag(t) {
				return true
			}
		}
		return false
	}
}

// ReadOnlyOnly admits capabilities declared ReadOnly.
func ReadOnlyOnly(c Capability) bool { return c.Effects.Has(ReadOnly) }

// And admits capabilities admitted by every filter. And() admits everything.
func And(fs ...Filter) Filter {
	return func(c Capability) bool {
		for _, f := range fs {
			if f != nil && !f(c) {
				return false
			}
		}
		return true
	}
}

// Or admits capabilities admitted by any filter. Or() admits nothing.
func Or(fs ...Filter) Filter {
	return func(c Capability) bool {
		for _, f := range fs {
			if f != nil && f(c) {
				return true
			}
		}
		return false
	}
}

// Not negates f. Not(nil) admits nothing.
func Not(f Filter) Filter {
	return func(c Capability) bool {
		if f == nil {
			return false
		}
		return !f(c)
	}
}

// Select filters a slice of capabilities before Add, for exposing only part
// of a library. To restrict access per audience, prefer views and keep
// everything registered.
func Select(caps []Capability, f Filter) []Capability {
	if f == nil {
		return append([]Capability(nil), caps...)
	}
	var out []Capability
	for _, c := range caps {
		if f(c) {
			out = append(out, c)
		}
	}
	return out
}
