// Package tools is the projection for in-process tool-calling loops: an
// agent you run yourself against a model API directly rather than through
// MCP. It handles only the registry side of the loop and depends on no
// model provider's SDK. The Def fields map one-to-one onto provider tool
// definitions (name, description, input schema).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/craggy-io/capability-services/registry"
	"github.com/google/jsonschema-go/jsonschema"
)

// Def is one LLM tool definition.
type Def struct {
	Name        string             // ToolName(c.Name)
	Description string             // Title, Description and registry.DescriptionNotes, blank-line separated
	InputSchema *jsonschema.Schema // never nil: an empty object schema when c.Input is nil

	// Entry carries registry facts providers don't take but the MCP
	// projection and hosts need (effects, tags, Long, exact ref).
	Entry registry.Entry
}

// Defs returns one Def per ready entry in v.List(), sorted by tool name.
// Agents only ever see current, non-superseded, ready capabilities. Re-list
// every turn: runtime changes show up immediately.
func Defs(v *registry.View) []Def {
	entries := v.List()
	out := make([]Def, 0, len(entries))
	for _, e := range entries {
		if !e.Ready {
			continue // don't make agents waste turns on it
		}
		out = append(out, Def{
			Name:        registry.ToolName(e.Name),
			Description: describe(e),
			InputSchema: inputSchema(e),
			Entry:       e,
		})
	}
	// Tool-name order, not capability-name order: mapping '.' (0x2E) to
	// '_' (0x5F) reorders names ("a.z" before "a1.x", but a1_x < a_z).
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func describe(e registry.Entry) string {
	var parts []string
	switch {
	case e.Title != "" && e.Description != "":
		parts = append(parts, e.Title, e.Description)
	case e.Title != "":
		parts = append(parts, e.Title)
	case e.Description != "":
		parts = append(parts, e.Description)
	}
	if note := registry.DescriptionNotes(e.Capability); note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, "\n\n")
}

// inputSchema is the tool-facing input schema: always an object schema.
// Tool protocols (MCP above all) take arguments as a JSON object, and the
// MCP SDK panics on any other shape — but the registry accepts
// capabilities whose input schema is not an object (a RawMessage Func's
// typeless schema, a scalar Func's string schema). An object-flavoured
// schema keeps its properties with the type made explicit; anything else
// degrades to a permissive object schema, and registry input validation
// still rejects arguments the capability itself would not accept.
func inputSchema(e registry.Entry) *jsonschema.Schema {
	if e.Input == nil {
		return registry.EmptyObjectSchema()
	}
	s := e.Input.CloneSchemas()
	switch {
	case s.Type == "object":
		return s
	case s.Type == "" && (len(s.Properties) > 0 || len(s.Required) > 0 || s.AdditionalProperties != nil):
		s.Type = "object"
		return s
	default:
		return &jsonschema.Schema{Type: "object"}
	}
}

// JobEnvelope is the text payload returned for a Long capability: its job
// handle. The model follows up with jobs_wait.
type JobEnvelope struct {
	Job    string             `json:"job"`
	Status registry.JobStatus `json:"status"`
}

// Result is one tool call's outcome. What the model reads is Render: the
// payload, then the notes.
type Result struct {
	// Text is the payload: the output JSON, a Long capability's job
	// handle (JobEnvelope), or a registry.ErrorEnvelope when IsError. It
	// never carries notes, so a host can parse it.
	Text    string
	IsError bool
	// Notes are facts about the call itself that the model should weigh
	// beside the payload: the capability is superseded, the call was slow
	// (see Elapsed). Render puts each on its own "note: " line after Text.
	Notes []string
	// Structured is the capability's raw JSON output, set only when the
	// tool advertises an output schema and this call returned that output
	// (see StructuredOutput). A projection that advertises the schema
	// sends it beside Text, as the MCP spec requires.
	Structured json.RawMessage
	// Job is the started job's ID when Text is a Long capability's handle.
	Job string
	// Code and Reason classify a failure (set only when IsError) so hosts
	// can record it without parsing Text: the registry code and its
	// optional reason (see registry.ReasonOf).
	Code   registry.Code
	Reason string
}

// Render is the tool result text the model reads: Text, then one
// "note: <note>" line per note.
func (r Result) Render() string { return AppendNotes(r.Text, r.Notes...) }

// AppendNotes is text followed by one "note: <note>" line per note: how
// every projection renders a result's notes for the model.
func AppendNotes(text string, notes ...string) string {
	for _, n := range notes {
		text += "\nnote: " + n
	}
	return text
}

// SlowCall is how long a call must take before its result tells the model
// so (Elapsed). Quicker calls stay silent: the note would be noise.
const SlowCall = 5 * time.Second

// ElapsedNote is the note for a call that took d, e.g. "took 12.3s", or ""
// when d is under SlowCall.
func ElapsedNote(d time.Duration) string {
	if d < SlowCall {
		return ""
	}
	return "took " + d.Round(100*time.Millisecond).String()
}

// Elapsed is r with an ElapsedNote for a call that took d, when it was
// slow. The caller measures what the model waited for.
func (r Result) Elapsed(d time.Duration) Result {
	if n := ElapsedNote(d); n != "" {
		r.Notes = append(append([]string(nil), r.Notes...), n)
	}
	return r
}

// ErrorResult is the Result for a failed call: IsError, the error's Code
// and Reason, and Text its registry.ErrorEnvelope — the same body the HTTP
// projection returns, so the model can tell a call to fix from one to
// retry later without parsing prose.
func ErrorResult(err error) Result {
	info := registry.Info(err)
	text, _ := json.Marshal(registry.ErrorEnvelope{Error: info})
	return Result{
		Text:    string(text),
		IsError: true,
		Code:    info.Code,
		Reason:  info.Reason,
	}
}

// Outcome is the Result for a call of e that ran to completion with out or
// err: the output with its structured form and any supersession note, or
// the failure.
func Outcome(e registry.Entry, out []byte, err error) Result {
	if err != nil {
		return ErrorResult(err)
	}
	return Result{Text: string(out), Structured: StructuredOutput(e, out), Notes: supersededNotes(e)}
}

// Handle is the Result for a Long capability e's started job: its
// JobEnvelope, the job's ID in Job, and any supersession note.
func Handle(e registry.Entry, job *registry.Job) Result {
	env, _ := json.Marshal(JobEnvelope{Job: job.ID(), Status: job.Status()})
	return Result{Text: string(env), Job: job.ID(), Notes: supersededNotes(e)}
}

// StructuredOutput is a capability's output as structured content: out
// itself when the capability declares an output schema and runs to
// completion in the call (a Long capability returns a job handle, not its
// output). Any JSON value counts, not only objects: a tool that
// advertises e.g. a string outputSchema must still return
// structuredContent, or strict clients reject the result. Nil otherwise.
func StructuredOutput(e registry.Entry, out []byte) json.RawMessage {
	if e.Output == nil || e.Long {
		return nil
	}
	if trimmed := bytes.TrimSpace(out); len(trimmed) > 0 && json.Valid(trimmed) {
		return json.RawMessage(trimmed)
	}
	return nil
}

// Call maps the tool name back, invokes it, and returns the text to send
// back as the tool result (Result.Render). If the call fails, isError is
// true and the text is a registry.ErrorEnvelope, so the model can see what
// went wrong and whether to retry. A Long capability returns its job handle
// immediately. A superseded capability the model still names runs, with a
// note after the result. A tool name outside the view — even one the model
// invents — is not_found.
func Call(ctx context.Context, v *registry.View, tool string, input json.RawMessage) (text string, isError bool) {
	r := Do(ctx, v, tool, input)
	return r.Render(), r.IsError
}

// Do is Call returning the whole Result: the payload apart from its notes,
// the structured output for projections that carry it, a Long call's job
// ID and, on failure, the error's Code and Reason. It does not time the
// call; a host that wants the slow-call note adds it with Elapsed.
func Do(ctx context.Context, v *registry.View, tool string, input json.RawMessage) Result {
	name := registry.FromToolName(tool)
	entry, ok := v.Get(name)
	if !ok {
		return ErrorResult(registry.ReasonErrorf(registry.NotFound, "unknown_tool",
			"no tool %q in this view; call only the tools you were given", tool))
	}
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	// Act on the entry just resolved, pinned by ref: re-resolving the
	// bare name could land on a concurrently Replaced version whose
	// Long/output facts differ from the ones this Result is built from.
	if entry.Long {
		job, err := v.Start(ctx, entry.Ref(), input)
		if err != nil {
			return ErrorResult(err)
		}
		return Handle(entry, job)
	}
	out, err := v.Invoke(ctx, entry.Ref(), input)
	return Outcome(entry, out, err)
}

func supersededNotes(e registry.Entry) []string {
	if !e.Superseded() {
		return nil
	}
	return []string{e.Name + " is superseded by " + strings.Join(e.SupersededBy, ", ")}
}
