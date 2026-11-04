package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/tools"
)

type in struct {
	Text string `json:"text"`
}

func setup(t *testing.T) *registry.Registry {
	t.Helper()
	r := registry.New()
	t.Cleanup(r.Close)
	echo := registry.Func("storage.artifact.get", "Get artifact",
		func(ctx context.Context, i in) (in, error) { return i, nil },
		registry.Describe("Fetch metadata."), registry.WithEffects(registry.ReadOnly))
	secret := registry.Func("admin.wipe", "Wipe",
		func(ctx context.Context, i in) (in, error) { return i, nil },
		registry.WithTags("admin"))
	slow := registry.Func("apk.decompile", "Decompile",
		func(ctx context.Context, i in) (in, error) {
			registry.Progress(ctx, registry.Update{Stage: "run", Done: 1, Total: 1, Unit: "files"})
			return in{Text: "sources"}, nil
		}, registry.AsLong(time.Second))
	down := registry.Func("down.cap", "Down",
		func(ctx context.Context, i in) (in, error) { return i, nil },
		registry.WithCheck(func(ctx context.Context) error { return errors.New("no docker") }))
	if err := r.Add(append([]registry.Capability{echo, secret, slow, down}, jobs.Capabilities()...)...); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDefsIncludeSubjectsAndExamples(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	c := registry.Func("apk.decompile", "Decompile",
		func(context.Context, in) (in, error) { return in{}, nil },
		registry.Describe("Turn an APK into sources."),
		registry.WithAccepts(registry.Subject{Type: "android.apk", Level: "validated"}),
		registry.WithEmits(registry.Subject{Type: "text.java"}),
		registry.WithExample("one APK", in{Text: "a1"}),
	)
	if err := r.Add(c); err != nil {
		t.Fatal(err)
	}
	defs := tools.Defs(r.View(registry.All))
	if len(defs) != 1 {
		t.Fatalf("defs = %d", len(defs))
	}
	want := "Decompile\n\nTurn an APK into sources.\n\nAccepts: android.apk (validated)\n\nEmits: text.java\n\nExamples:\n- one APK: {\"text\":\"a1\"}"
	if defs[0].Description != want {
		t.Fatalf("description=%q", defs[0].Description)
	}
	if defs[0].Entry.Accepts[0].Type != "android.apk" {
		t.Fatalf("entry accepts = %+v", defs[0].Entry.Accepts)
	}
}

func TestDefsAndCall(t *testing.T) {
	r := setup(t)
	agents := r.View(registry.Not(registry.Tagged("admin")))
	defs := tools.Defs(agents)
	names := []string{}
	for _, d := range defs {
		names = append(names, d.Name)
		if d.InputSchema == nil {
			t.Fatalf("nil schema for %s", d.Name)
		}
		if d.Name == "storage_artifact_get" && d.Description != "Get artifact\n\nFetch metadata." {
			t.Fatalf("description=%q", d.Description)
		}
	}
	joined := strings.Join(names, ",")
	if strings.Contains(joined, "admin_wipe") {
		t.Fatal("admin tool leaked into agent Defs")
	}
	if strings.Contains(joined, "down_cap") {
		t.Fatal("unready tool in Defs")
	}
	// Sorted by tool name.
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Defs not sorted: %v", names)
		}
	}
	text, isErr := tools.Call(context.Background(), agents, "storage_artifact_get", json.RawMessage(`{"text":"hi"}`))
	if isErr || text != `{"text":"hi"}` {
		t.Fatalf("call=%q err=%v", text, isErr)
	}
	// Invented tool: not_found as model-visible error text.
	text, isErr = tools.Call(context.Background(), agents, "hallucinated_tool", nil)
	if !isErr || !strings.HasPrefix(text, `{"error":{"code":"not_found","reason":"unknown_tool"`) {
		t.Fatalf("hallucinated=%q %v", text, isErr)
	}
	// Outside the view: same not_found (filtering is enforcement).
	text, isErr = tools.Call(context.Background(), agents, "admin_wipe", json.RawMessage(`{"text":"x"}`))
	if !isErr || !strings.HasPrefix(text, `{"error":{"code":"not_found"`) {
		t.Fatalf("admin via agent view=%q %v", text, isErr)
	}
	// Bad input: invalid, model sees why.
	text, isErr = tools.Call(context.Background(), agents, "storage_artifact_get", json.RawMessage(`{"text":7}`))
	if !isErr || !strings.HasPrefix(text, `{"error":{"code":"invalid"`) || !strings.Contains(text, "Required fields: text.") {
		t.Fatalf("bad input=%q %v", text, isErr)
	}
}

func TestCallLongReturnsJobThenWait(t *testing.T) {
	r := setup(t)
	agents := r.View(registry.All)
	text, isErr := tools.Call(context.Background(), agents, "apk_decompile", json.RawMessage(`{"text":"a.apk"}`))
	if isErr {
		t.Fatalf("long call errored: %s", text)
	}
	var env tools.JobEnvelope
	if err := json.Unmarshal([]byte(text), &env); err != nil || env.Job == "" {
		t.Fatalf("long envelope=%s err=%v", text, err)
	}
	waitText, isErr := tools.Call(context.Background(), agents, "jobs_wait",
		json.RawMessage(`{"id":"`+env.Job+`","timeout":5}`))
	if isErr {
		t.Fatalf("jobs_wait errored: %s", waitText)
	}
	if !strings.Contains(waitText, `"state":"succeeded"`) || !strings.Contains(waitText, `sources`) {
		t.Fatalf("jobs_wait=%s", waitText)
	}
}

func TestCallSupersededStillRunsWithNote(t *testing.T) {
	r := setup(t)
	repl := registry.Func("new.get", "New get",
		func(ctx context.Context, i in) (in, error) { return i, nil },
		registry.WithSupersedes("storage.artifact.get"))
	if err := r.Add(repl); err != nil {
		t.Fatal(err)
	}
	for _, d := range tools.Defs(r.View(registry.All)) {
		if d.Name == "storage_artifact_get" {
			t.Fatal("superseded tool still in Defs")
		}
	}
	text, isErr := tools.Call(context.Background(), r.View(registry.All), "storage_artifact_get", json.RawMessage(`{"text":"x"}`))
	if isErr || !strings.HasPrefix(text, `{"text":"x"}`) || !strings.Contains(text, "superseded by new.get") {
		t.Fatalf("superseded call=%q err=%v", text, isErr)
	}
}

func TestCallerFlowsThrough(t *testing.T) {
	r := registry.New()
	defer r.Close()
	var got registry.Caller
	c := registry.Func("who.am.i", "who",
		func(ctx context.Context, i in) (in, error) {
			got, _ = registry.CallerFrom(ctx)
			return i, nil
		})
	if err := r.Add(c); err != nil {
		t.Fatal(err)
	}
	ctx := registry.WithCaller(context.Background(), registry.Caller{ID: "agent:x", Kind: "agent", Session: "conv-1"})
	if _, isErr := tools.Call(ctx, r.View(registry.All), "who_am_i", json.RawMessage(`{"text":""}`)); isErr {
		t.Fatal("call failed")
	}
	if got.ID != "agent:x" || got.Session != "conv-1" {
		t.Fatalf("caller=%+v", got)
	}
}

// Do carries a capability's output as structured content when it declares
// an output schema and returns it in the call; a Long capability returns a
// job handle, an error returns text, and neither is structured.
func TestDoStructuredOutput(t *testing.T) {
	r := setup(t)
	agents := r.View(registry.All)
	ctx := context.Background()

	res := tools.Do(ctx, agents, "storage_artifact_get", json.RawMessage(`{"text":"hi"}`))
	if res.IsError || res.Text != `{"text":"hi"}` || string(res.Structured) != `{"text":"hi"}` {
		t.Fatalf("structured call = %+v", res)
	}
	if res := tools.Do(ctx, agents, "storage_artifact_get", json.RawMessage(`{"text":7}`)); !res.IsError || res.Structured != nil {
		t.Fatalf("error call = %+v, want text only", res)
	}
	if res := tools.Do(ctx, agents, "apk_decompile", json.RawMessage(`{"text":"a.apk"}`)); res.IsError || res.Structured != nil {
		t.Fatalf("long call = %+v, want the job handle as text only", res)
	}
	text, isErr := tools.Call(ctx, agents, "storage_artifact_get", json.RawMessage(`{"text":"hi"}`))
	if isErr || text != res.Text {
		t.Fatalf("Call = %q %v, want Do's text", text, isErr)
	}

	e, _ := agents.Get("storage.artifact.get")
	// Any valid JSON is structured content when an output schema is
	// advertised — a tool whose outputSchema is a string or array must
	// still return structuredContent (docs/projections.md).
	for name, out := range map[string]string{"array": `[1]`, "string": `"x"`} {
		if got := tools.StructuredOutput(e, []byte(out)); string(got) != out {
			t.Errorf("%s output %q not structured: %s", name, out, got)
		}
	}
	for name, out := range map[string]string{"empty": ``, "torn": `{"a":`} {
		if got := tools.StructuredOutput(e, []byte(out)); got != nil {
			t.Errorf("%s output %q was structured: %s", name, out, got)
		}
	}
	e.Output = nil
	if got := tools.StructuredOutput(e, []byte(`{"a":1}`)); got != nil {
		t.Errorf("a capability without an output schema was structured: %s", got)
	}
}

func TestDoCarriesCodeAndReason(t *testing.T) {
	r := setup(t)
	full := func(ctx context.Context, i in) (in, error) {
		return i, registry.WrapReason(registry.Unavailable, registry.ReasonNoCapacity, errors.New("pool full"))
	}
	if err := r.Add(
		registry.Func("pool.acquire", "Acquire", full),
		registry.Func("down.long", "Down long", func(ctx context.Context, i in) (in, error) { return i, nil },
			registry.AsLong(time.Second),
			registry.WithCheck(func(ctx context.Context) error { return errors.New("no docker") })),
	); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v := r.View(registry.Not(registry.Tagged("admin")))
	cases := []struct {
		name, tool, input string
		code              registry.Code
		reason, text      string
	}{
		{"unknown tool", "nope_gone", `{}`, registry.NotFound, "unknown_tool", `{"error":{"code":"not_found","reason":"unknown_tool","message":"no tool \"nope_gone\" in this view; call only the tools you were given","retryable":false}}`},
		{"outside view", "admin_wipe", `{}`, registry.NotFound, "unknown_tool", `{"error":{"code":"not_found","reason":"unknown_tool","message":"no tool \"admin_wipe\" in this view; call only the tools you were given","retryable":false}}`},
		{"invoke failure", "pool_acquire", `{"text":"x"}`, registry.Unavailable, registry.ReasonNoCapacity, `{"error":{"code":"unavailable","reason":"no_capacity","message":"pool full","retryable":true}}`},
		{"invalid input", "storage_artifact_get", `{"text":7}`, registry.Invalid, "", ""},
		{"not ready", "down_cap", `{}`, registry.Unavailable, registry.ReasonNotReady, ""},
		{"job start not ready", "down_long", `{}`, registry.Unavailable, registry.ReasonNotReady, ""},
		{"job start invalid", "apk_decompile", `{"text":7}`, registry.Invalid, "", ""},
	}
	for _, tc := range cases {
		res := tools.Do(ctx, v, tc.tool, json.RawMessage(tc.input))
		if !res.IsError || res.Code != tc.code || res.Reason != tc.reason {
			t.Errorf("%s: IsError=%v Code=%q Reason=%q, want %q/%q (%s)", tc.name, res.IsError, res.Code, res.Reason, tc.code, tc.reason, res.Text)
		}
		if !strings.HasPrefix(res.Text, `{"error":{"code":"`+string(tc.code)+`"`) || (tc.text != "" && res.Text != tc.text) {
			t.Errorf("%s: Text = %q", tc.name, res.Text)
		}
		if text, isErr := tools.Call(ctx, v, tc.tool, json.RawMessage(tc.input)); text != res.Text || !isErr {
			t.Errorf("%s: Call = %q/%v, Do = %q", tc.name, text, isErr, res.Text)
		}
	}
	// Success leaves Code and Reason empty, synchronous and Long alike.
	for _, tool := range []string{"storage_artifact_get", "apk_decompile"} {
		if res := tools.Do(ctx, v, tool, json.RawMessage(`{"text":"a"}`)); res.IsError || res.Code != "" || res.Reason != "" {
			t.Errorf("%s: success result %+v", tool, res)
		}
	}
}

// Notes stay apart from the payload in Result and follow it, one line
// each, in what the model reads; a call is timed only when it was slow.
func TestNotesAndElapsed(t *testing.T) {
	r := tools.Result{Text: `{"a":1}`, Notes: []string{"x is superseded by y"}}
	if got := r.Render(); got != `{"a":1}`+"\nnote: x is superseded by y" {
		t.Fatalf("Render = %q", got)
	}
	if got := r.Elapsed(tools.SlowCall - time.Millisecond); len(got.Notes) != 1 {
		t.Fatalf("a quick call got notes %q", got.Notes)
	}
	slow := r.Elapsed(12345 * time.Millisecond)
	if slow.Render() != `{"a":1}`+"\nnote: x is superseded by y\nnote: took 12.3s" || len(r.Notes) != 1 {
		t.Fatalf("slow Render = %q (original notes %q)", slow.Render(), r.Notes)
	}
	if slow.Text != r.Text {
		t.Fatalf("Elapsed changed the payload: %q", slow.Text)
	}
}

// A Long call's job ID is a field, so a superseded Long capability's
// handle (payload plus a note) still yields it without parsing text.
func TestDoLongCarriesJobBesideNotes(t *testing.T) {
	r := setup(t)
	if err := r.Add(registry.Func("apk.decompile2", "Decompile", func(ctx context.Context, i in) (in, error) { return i, nil },
		registry.AsLong(time.Second), registry.WithSupersedes("apk.decompile"))); err != nil {
		t.Fatal(err)
	}
	res := tools.Do(context.Background(), r.View(registry.All), "apk_decompile", json.RawMessage(`{"text":"a"}`))
	if res.IsError || res.Job == "" || len(res.Notes) != 1 {
		t.Fatalf("Do = %+v", res)
	}
	var env tools.JobEnvelope
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil || env.Job != res.Job {
		t.Fatalf("payload %q is not the job handle for %s: %v", res.Text, res.Job, err)
	}
}
