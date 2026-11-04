package registry

import "encoding/json"

// Descriptor is the full, serializable projection of one Entry: every
// field of the registered Capability that can cross a boundary, plus
// the entry's live state. It is the one shape catalogues, system
// panels and documentation generators should consume — projections
// (HTTP, MCP, tools) each showed a subset before it existed, and no
// caller could get, say, heartbeat or check presence without the Go
// Entry in hand.
//
// The only Capability fields absent are Handler and Check themselves:
// functions cannot cross a boundary. HasCheck records that a Check
// exists, and Ready/CheckError carry its latest outcome.
type Descriptor struct {
	Name         string          `json:"name"`
	Version      int             `json:"version"`
	Ref          string          `json:"ref"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	Input        json.RawMessage `json:"input"`
	Output       json.RawMessage `json:"output,omitempty"`
	Examples     []Example       `json:"examples,omitempty"`
	Effects      []string        `json:"effects"`
	Tags         []string        `json:"tags,omitempty"`
	Supersedes   []string        `json:"supersedes,omitempty"`
	Accepts      []Subject       `json:"accepts,omitempty"`
	Emits        []Subject       `json:"emits,omitempty"`
	Long         bool            `json:"long,omitempty"`
	HeartbeatMs  int64           `json:"heartbeatMs,omitempty"`
	HasCheck     bool            `json:"hasCheck,omitempty"`
	Current      bool            `json:"current"`
	SupersededBy []string        `json:"supersededBy,omitempty"`
	Group        string          `json:"group,omitempty"`
	Ready        bool            `json:"ready"`
	CheckError   string          `json:"checkError,omitempty"`
	Origin       *Origin         `json:"origin,omitempty"` // nil when the capability has no Origin
}

// Descriptor projects e into its full serializable form. A nil
// input schema renders as the empty-object schema projections
// present for no-input capabilities; a nil output schema stays
// absent (unstructured output). (A method, not a Describe func:
// Describe is already the registration Option that sets the
// description text.)
func (e Entry) Descriptor() Descriptor {
	var in, out json.RawMessage
	if e.Input != nil {
		in = marshalSchemaJSON(e.Input)
	} else {
		in = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if e.Output != nil {
		out = marshalSchemaJSON(e.Output)
	}
	effects := e.Effects.Names()
	if effects == nil {
		effects = []string{}
	}
	var origin *Origin
	if !e.Origin.IsZero() {
		o := e.Origin
		origin = &o
	}
	return Descriptor{
		Origin: origin,
		Name:   e.Name, Version: e.Version, Ref: e.Capability.Ref(),
		Title: e.Title, Description: e.Description, Input: in, Output: out,
		Examples: e.Examples, Effects: effects, Tags: e.Tags, Supersedes: e.Supersedes,
		Accepts: e.Accepts, Emits: e.Emits, Long: e.Long,
		HeartbeatMs: e.Heartbeat.Milliseconds(), HasCheck: e.Check != nil,
		Current: e.Current, SupersededBy: e.SupersededBy, Group: e.Group,
		Ready: e.Ready, CheckError: e.CheckError,
	}
}

func marshalSchemaJSON(s interface {
	MarshalJSON() ([]byte, error)
}) json.RawMessage {
	if s == nil {
		return nil
	}
	b, err := s.MarshalJSON()
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// Descriptors returns the full projection of this view: List()
// entries, or All() (old versions and superseded included) when all
// is true. Order matches the underlying listing (name, then version).
func (v *View) Descriptors(all bool) []Descriptor {
	var entries []Entry
	if all {
		entries = v.All()
	} else {
		entries = v.List()
	}
	out := make([]Descriptor, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Descriptor())
	}
	return out
}

// Descriptors is View(All).Descriptors on the whole registry.
func (r *Registry) Descriptors(all bool) []Descriptor { return r.allView.Descriptors(all) }
