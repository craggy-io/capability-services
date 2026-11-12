// Package registry is the core of capability-services: one registration
// shape for a capability, projected mechanically into MCP tools, HTTP,
// Starlark and in-process tool-calling loops. It depends only on the standard library and
// github.com/google/jsonschema-go. It knows nothing about any domain or
// transport; projections and domain adapters live in sibling packages.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Capability is the single registration shape.
type Capability struct {
	Name         string             // dotted, lower-case: "storage.artifact.get"
	Version      int                // >= 1; 0 is treated as 1
	Title        string             // one line, for listings
	Description  string             // what it does and when to use it (agents read this)
	Input        *jsonschema.Schema // object schema; nil means no input
	Output       *jsonschema.Schema // nil means unstructured
	Examples     []Example          // sample inputs agents read; each must validate against Input
	Effects      Effects
	Tags         []string                        // "agent", "admin", "slow", ...
	Supersedes   []string                        // refs this replaces: "jadx.decompile" (all versions) or "apktool.decode@1"
	Requires     []string                        // refs its handler may call; View.Pin pins them with it (see Pinned)
	Dependencies []Dependency                    // external prerequisites checked before advertising or invoking
	Accepts      []Subject                       // content this capability can run on; empty means it is not a content step
	Emits        []Subject                       // hint about content the output may be; not a result
	Long         bool                            // projections start it as a job instead of waiting for it
	Heartbeat    time.Duration                   // a job with no Progress for this long shows as stalled; 0 = 30s
	Check        func(ctx context.Context) error // optional readiness check
	Handler      Handler
	Origin       Origin // where the definition came from; zero when the host did not say
}

// Origin records where a capability's definition came from, for listings
// and audit: a script file and its content address, a plugin, a remote
// catalogue. The registry stores and projects it and never interprets it.
type Origin struct {
	Kind   string `json:"kind"`             // what defined it: "script", "plugin", ...
	Ref    string `json:"ref,omitempty"`    // which one: a file name, a package path, a URL
	Digest string `json:"digest,omitempty"` // content address of the definition, e.g. "sha256:<hex>"
}

// IsZero reports whether o carries no provenance.
func (o Origin) IsZero() bool { return o == Origin{} }

// Subject is one opaque content type a capability accepts or emits.
// The registry stores and projects it. It does not interpret Type or
// Level; the host decides the vocabulary and whether an artifact qualifies.
type Subject struct {
	Type  string `json:"type" jsonschema:"opaque content type name"`
	Level string `json:"level,omitempty" jsonschema:"opaque evidence level; empty when the caller did not pin one"`
}

// Example is one sample input: what a JSON Schema cannot say — a value
// format, which optional fields go together, the usual call. Add checks
// each one: an example that does not validate against the input schema
// fails registration, so examples never drift from the schema.
type Example struct {
	Note  string          `json:"note,omitempty"` // one line: when to call it this way
	Input json.RawMessage `json:"input"`
}

// DescriptionNotes is the text projections append to a capability's
// description for agents: its subjects (SubjectNote) and examples
// (ExampleNote), blank-line separated. Empty when it has neither.
func DescriptionNotes(c Capability) string {
	var parts []string
	for _, s := range []string{SubjectNote(c), ExampleNote(c)} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ExampleNote is the projection text for Examples: one line per
// example, its note then its compact input JSON. Empty when there are
// none.
func ExampleNote(c Capability) string {
	if len(c.Examples) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Examples:")
	for _, ex := range c.Examples {
		b.WriteString("\n- ")
		if ex.Note != "" {
			b.WriteString(ex.Note + ": ")
		}
		b.Write(ex.Input)
	}
	return b.String()
}

// normalizeExamples checks each example against the resolved input
// schema and copies it with compact JSON and a trimmed note.
func normalizeExamples(name string, in []Example, input *jsonschema.Resolved) ([]Example, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]Example, len(in))
	for i, ex := range in {
		ex.Note = strings.TrimSpace(ex.Note)
		if strings.ContainsAny(ex.Note, "\r\n") {
			return nil, Errorf(Invalid, "%s: examples[%d] needs a single-line note", name, i)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, ex.Input); err != nil {
			return nil, Errorf(Invalid, "%s: examples[%d]: %v", name, i, err)
		}
		if err := validateAgainst(input, buf.Bytes()); err != nil {
			return nil, Errorf(Invalid, "%s: examples[%d] does not fit the input schema: %v", name, i, err)
		}
		ex.Input = buf.Bytes()
		out[i] = ex
	}
	return out, nil
}

// SubjectNote is the projection text for Accepts and Emits. It is empty
// when both lists are empty.
func SubjectNote(c Capability) string {
	var parts []string
	if s := formatSubjects("Accepts", c.Accepts); s != "" {
		parts = append(parts, s)
	}
	if s := formatSubjects("Emits", c.Emits); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}

func formatSubjects(label string, ss []Subject) string {
	if len(ss) == 0 {
		return ""
	}
	parts := make([]string, len(ss))
	for i, s := range ss {
		if s.Level == "" {
			parts[i] = s.Type
		} else {
			parts[i] = s.Type + " (" + s.Level + ")"
		}
	}
	return label + ": " + strings.Join(parts, ", ")
}

// normalizeSubjects trims and copies a subject list. An empty type is invalid.
func normalizeSubjects(name, field string, in []Subject) ([]Subject, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]Subject, len(in))
	for i, s := range in {
		s.Type = strings.TrimSpace(s.Type)
		s.Level = strings.TrimSpace(s.Level)
		if s.Type == "" || strings.ContainsAny(s.Type, "\r\n") || strings.ContainsAny(s.Level, "\r\n") {
			return nil, Errorf(Invalid, "%s: %s[%d] needs a single-line type", name, field, i)
		}
		out[i] = s
	}
	return out, nil
}

// Handler takes and returns JSON. The returned value is JSON-encoded unless
// it is already a json.RawMessage.
type Handler func(ctx context.Context, in json.RawMessage) (any, error)

// Middleware wraps a handler. Cross-cutting concerns (audit, authorization,
// telemetry) live here, never in a handler or a projection.
type Middleware func(next Handler, c Capability) Handler

// Ref returns the exact "name@version" reference (version 0 counts as 1).
func (c Capability) Ref() string { return refOf(c.Name, normVersion(c.Version)) }

// HasTag reports whether the capability carries tag.
func (c Capability) HasTag(tag string) bool {
	for _, t := range c.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

func normVersion(v int) int {
	if v <= 0 {
		return 1
	}
	return v
}

// Effects are hints about what a call does to the world.
type Effects uint8

const (
	ReadOnly    Effects = 1 << iota // changes nothing
	Idempotent                      // repeat calls have no further effect
	Destructive                     // removes or overwrites something
	OpenWorld                       // touches the network or external systems
)

// Has reports whether all bits in e are set.
func (e Effects) Has(other Effects) bool { return e&other == other }

// Names returns the effect names set, in declaration order.
func (e Effects) Names() []string {
	var out []string
	if e.Has(ReadOnly) {
		out = append(out, "read_only")
	}
	if e.Has(Idempotent) {
		out = append(out, "idempotent")
	}
	if e.Has(Destructive) {
		out = append(out, "destructive")
	}
	if e.Has(OpenWorld) {
		out = append(out, "open_world")
	}
	return out
}

func (e Effects) String() string {
	n := e.Names()
	if len(n) == 0 {
		return "none"
	}
	return strings.Join(n, "|")
}

// ParseEffects maps effect names ("read_only", "readonly", "idempotent",
// "destructive", "open_world", "openworld") to Effects. Unknown names are an
// error.
func ParseEffects(names []string) (Effects, error) {
	var out Effects
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "read_only", "readonly", "read-only":
			out |= ReadOnly
		case "idempotent":
			out |= Idempotent
		case "destructive":
			out |= Destructive
		case "open_world", "openworld", "open-world":
			out |= OpenWorld
		case "", "none":
		default:
			return 0, Errorf(Invalid, "unknown effect %q", n)
		}
	}
	return out, nil
}

// Entry is a capability plus its live state.
type Entry struct {
	Capability
	Current      bool     // the newest version of its name
	SupersededBy []string // refs that supersede this one (bare name for a current superseder, else name@version); empty if none
	Group        string   // name of the registered Group it belongs to; empty when none
	Ready        bool     // its last Check passed (true when there's no Check)
	CheckError   string   // message from the last failed Check, if any
}

// Superseded reports whether any registered capability supersedes this entry.
func (e Entry) Superseded() bool { return len(e.SupersededBy) > 0 }
