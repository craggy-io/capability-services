package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/jobs"
	regmcp "github.com/craggy-io/capability-services/mcp"
	"github.com/craggy-io/capability-services/registry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Text string `json:"text"`
}

type harness struct {
	r       *registry.Registry
	cs      *mcpsdk.ClientSession
	stop    func()
	closed  chan registry.Caller
	progMu  sync.Mutex
	prog    []float64
	progMsg []string
	changed chan struct{}
}

func setup(t *testing.T) *harness {
	t.Helper()
	r := registry.New()
	t.Cleanup(r.Close)
	echo := registry.Func("storage.artifact.get", "Get artifact",
		func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
		registry.Describe("Fetch it."), registry.WithEffects(registry.ReadOnly|registry.Idempotent))
	slow := registry.Func("apk.decompile", "Decompile",
		func(ctx context.Context, in echoIn) (echoIn, error) {
			registry.Progress(ctx, registry.Update{Stage: "push", Done: 100, Total: 100, Unit: "bytes"})
			time.Sleep(30 * time.Millisecond)
			registry.Progress(ctx, registry.Update{Stage: "run", Done: 1, Total: 2, Unit: "files"})
			return echoIn{Text: "sources"}, nil
		}, registry.AsLong(time.Second), registry.WithEffects(registry.OpenWorld))
	quick := registry.Func("work.quick", "Quick with progress",
		func(ctx context.Context, in echoIn) (echoIn, error) {
			registry.Progress(ctx, registry.Update{Stage: "push", Done: 10, Total: 10, Unit: "bytes"})
			registry.Progress(ctx, registry.Update{Stage: "run", Done: 1, Total: 5, Unit: "files"})
			return in, nil
		})
	burst := registry.Func("work.burst", "Per-file progress",
		func(ctx context.Context, in echoIn) (echoIn, error) {
			for i := int64(1); i <= 2000; i++ {
				registry.Progress(ctx, registry.Update{Stage: "pull", Done: i, Unit: "files"})
			}
			return in, nil
		})
	admin := registry.Func("admin.wipe", "Wipe",
		func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
		registry.WithTags("admin"), registry.WithEffects(registry.Destructive))
	caps := append([]registry.Capability{echo, slow, quick, burst, admin}, jobs.Capabilities()...)
	if err := r.Add(caps...); err != nil {
		t.Fatal(err)
	}

	h := &harness{r: r, closed: make(chan registry.Caller, 1), changed: make(chan struct{}, 16)}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	agents := r.View(registry.Not(registry.Tagged("admin")))
	h.stop = regmcp.Serve(server, agents,
		regmcp.Caller(func(ss *mcpsdk.ServerSession) registry.Caller {
			return registry.Caller{ID: "agent:tester"}
		}),
		regmcp.OnClose(func(c registry.Caller) { h.closed <- c }))
	t.Cleanup(h.stop)

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "client", Version: "1"}, &mcpsdk.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, req *mcpsdk.ToolListChangedRequest) {
			select {
			case h.changed <- struct{}{}:
			default:
			}
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcpsdk.ProgressNotificationClientRequest) {
			h.progMu.Lock()
			h.prog = append(h.prog, req.Params.Progress)
			h.progMsg = append(h.progMsg, req.Params.Message)
			h.progMu.Unlock()
		},
	})
	st, ct := mcpsdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.cs = cs
	t.Cleanup(func() { _ = cs.Close() })
	return h
}

func toolNames(t *testing.T, h *harness) []string {
	t.Helper()
	res, err := h.cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Name == "storage_artifact_get" {
			a := tool.Annotations
			if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || (a.DestructiveHint != nil && *a.DestructiveHint) {
				t.Errorf("annotations for get: %+v", a)
			}
		}
		if tool.Name == "apk_decompile" {
			a := tool.Annotations
			if a == nil || a.OpenWorldHint == nil || !*a.OpenWorldHint {
				t.Errorf("annotations for decompile: %+v", a)
			}
		}
	}
	return names
}

func callText(t *testing.T, h *harness, params *mcpsdk.CallToolParams) (string, bool) {
	t.Helper()
	res, err := h.cs.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", params.Name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func TestMCPListCallAndAnnotations(t *testing.T) {
	h := setup(t)
	names := toolNames(t, h)
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "storage_artifact_get") || !strings.Contains(joined, "jobs_wait") {
		t.Fatalf("tools=%v", names)
	}
	if strings.Contains(joined, "admin_wipe") {
		t.Fatal("admin tool leaked to agent view")
	}
	text, isErr := callText(t, h, &mcpsdk.CallToolParams{Name: "storage_artifact_get", Arguments: map[string]any{"text": "hi"}})
	if isErr || text != `{"text":"hi"}` {
		t.Fatalf("call=%q err=%v", text, isErr)
	}
	text, isErr = callText(t, h, &mcpsdk.CallToolParams{Name: "storage_artifact_get", Arguments: map[string]any{"text": 5}})
	if !isErr || errorOf(t, text).Code != registry.Invalid {
		t.Fatalf("bad input=%q err=%v", text, isErr)
	}
}

func TestMCPLongJobAndWait(t *testing.T) {
	h := setup(t)
	text, isErr := callText(t, h, &mcpsdk.CallToolParams{Name: "apk_decompile", Arguments: map[string]any{"text": "a.apk"}})
	if isErr {
		t.Fatalf("long call: %s", text)
	}
	var env struct {
		Job string `json:"job"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil || env.Job == "" {
		t.Fatalf("envelope=%s err=%v", text, err)
	}
	text, isErr = callText(t, h, &mcpsdk.CallToolParams{Name: "jobs_wait", Arguments: map[string]any{"id": env.Job, "timeout": 5}})
	if isErr || !strings.Contains(text, "succeeded") || !strings.Contains(text, "sources") {
		t.Fatalf("jobs_wait=%q err=%v", text, isErr)
	}
}

func TestMCPProgressMonotonicAcrossStages(t *testing.T) {
	h := setup(t)
	params := &mcpsdk.CallToolParams{Name: "work_quick", Arguments: map[string]any{"text": "x"}}
	params.SetProgressToken("tok-1")
	text, isErr := callText(t, h, params)
	if isErr || text != `{"text":"x"}` {
		t.Fatalf("quick=%q err=%v", text, isErr)
	}
	// Notifications may land just after the result; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.progMu.Lock()
		n := len(h.prog)
		h.progMu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.progMu.Lock()
	defer h.progMu.Unlock()
	if len(h.prog) < 2 {
		t.Fatalf("progress notifications=%v", h.prog)
	}
	for i := 1; i < len(h.prog); i++ {
		if h.prog[i] <= h.prog[i-1] {
			t.Fatalf("progress not monotonic across stage/unit switch: %v", h.prog)
		}
	}
	joined := strings.Join(h.progMsg, "|")
	if !strings.Contains(joined, "bytes") || !strings.Contains(joined, "files") {
		t.Fatalf("stage counters/unit must ride in the message: %q", joined)
	}
}

func TestMCPLiveToolChanges(t *testing.T) {
	h := setup(t)
	before := strings.Join(toolNames(t, h), ",")
	if strings.Contains(before, "late_cap") {
		t.Fatal("late cap visible too early")
	}
	late := registry.Func("late.cap", "Late", func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil })
	if err := h.r.Add(late); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.changed:
	case <-time.After(3 * time.Second):
		t.Fatal("no tools/list_changed after runtime Add")
	}
	if !strings.Contains(strings.Join(toolNames(t, h), ","), "late_cap") {
		t.Fatal("late cap not in ListTools after change")
	}
	// Superseding removes the original live.
	repl := registry.Func("new.get", "New get", func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
		registry.WithSupersedes("storage.artifact.get"))
	if err := h.r.Add(repl); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.changed:
	case <-time.After(3 * time.Second):
		t.Fatal("no tools/list_changed after supersession")
	}
	after := strings.Join(toolNames(t, h), ",")
	if strings.Contains(after, "storage_artifact_get") || !strings.Contains(after, "new_get") {
		t.Fatalf("after supersession tools=%s", after)
	}
	h.r.Remove("late.cap")
	select {
	case <-h.changed:
	case <-time.After(3 * time.Second):
		t.Fatal("no tools/list_changed after Remove")
	}
	if strings.Contains(strings.Join(toolNames(t, h), ","), "late_cap") {
		t.Fatal("removed cap still listed")
	}
	// stop() removes every tool it added.
	h.stop()
	if got := toolNames(t, h); len(got) != 0 {
		t.Fatalf("after stop tools=%v", got)
	}
}

func TestMCPCallerAndOnClose(t *testing.T) {
	h := setup(t)
	// A job started via MCP carries the session caller; jobs.list from the
	// same session sees it (default access rule).
	text, _ := callText(t, h, &mcpsdk.CallToolParams{Name: "apk_decompile", Arguments: map[string]any{"text": "a"}})
	var env struct {
		Job string `json:"job"`
	}
	_ = json.Unmarshal([]byte(text), &env)
	text, isErr := callText(t, h, &mcpsdk.CallToolParams{Name: "jobs_get", Arguments: map[string]any{"id": env.Job}})
	if isErr || !strings.Contains(text, "agent:tester") {
		t.Fatalf("jobs_get own session=%q err=%v", text, isErr)
	}
	if err := h.cs.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-h.closed:
		if c.ID != "agent:tester" || c.Kind != "agent" || c.Session == "" {
			t.Fatalf("OnClose caller=%+v", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnClose not called on session end")
	}
}

func callRaw(t *testing.T, h *harness, params *mcpsdk.CallToolParams) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := h.cs.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", params.Name, err)
	}
	return res
}

// A tool that advertises an output schema returns its output as
// structured content beside the text (the MCP spec requires both; strict
// clients reject a result without it), by either call path. A Long tool
// returns a job handle, so it advertises no output schema, and an error
// carries no structured content.
func TestMCPStructuredContent(t *testing.T) {
	h := setup(t)
	tools, err := h.cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	advertised := map[string]bool{}
	for _, tool := range tools.Tools {
		advertised[tool.Name] = tool.OutputSchema != nil
	}
	if !advertised["storage_artifact_get"] || !advertised["work_quick"] {
		t.Fatalf("direct tools do not advertise an output schema: %v", advertised)
	}
	if advertised["apk_decompile"] {
		t.Fatal("a Long tool advertises its final output as its result schema, but a call returns a job handle")
	}

	want := `{"text":"hi"}`
	res := callRaw(t, h, &mcpsdk.CallToolParams{Name: "storage_artifact_get", Arguments: map[string]any{"text": "hi"}})
	if got, _ := json.Marshal(res.StructuredContent); res.IsError || string(got) != want {
		t.Fatalf("structured content = %s (error %v), want %s", got, res.IsError, want)
	}
	// With a progress token the call runs as a job; the result is the same.
	params := &mcpsdk.CallToolParams{Name: "work_quick", Arguments: map[string]any{"text": "hi"}}
	params.SetProgressToken("tok-structured")
	res = callRaw(t, h, params)
	if got, _ := json.Marshal(res.StructuredContent); res.IsError || string(got) != want {
		t.Fatalf("structured content via the progress path = %s (error %v), want %s", got, res.IsError, want)
	}
	res = callRaw(t, h, &mcpsdk.CallToolParams{Name: "storage_artifact_get", Arguments: map[string]any{"text": 5}})
	if !res.IsError || res.StructuredContent != nil {
		t.Fatalf("error result = %+v, want text only", res)
	}
	res = callRaw(t, h, &mcpsdk.CallToolParams{Name: "apk_decompile", Arguments: map[string]any{"text": "a"}})
	if res.IsError || res.StructuredContent != nil {
		t.Fatalf("long call result = %+v, want the job handle as text only", res)
	}
}

// errorOf parses a failed call's text: the registry's error envelope.
func errorOf(t *testing.T, text string) registry.ErrorInfo {
	t.Helper()
	var env registry.ErrorEnvelope
	if err := json.Unmarshal([]byte(text), &env); err != nil || env.Error == nil {
		t.Fatalf("error text %q is not an error envelope: %v", text, err)
	}
	return *env.Error
}

// A failed call's text is the error envelope (the HTTP error body), by
// every call path, so the model and the client read code, reason and
// retryable from the same place.
func TestMCPErrorEnvelope(t *testing.T) {
	h := setup(t)
	full := func(ctx context.Context, in echoIn) (echoIn, error) {
		return in, registry.WrapReason(registry.Unavailable, registry.ReasonNoCapacity, errors.New("pool full"))
	}
	if err := h.r.Add(
		registry.Func("pool.acquire", "Acquire", full),
		registry.Func("pool.long", "Acquire long", full, registry.AsLong(time.Second)),
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.changed:
	case <-time.After(3 * time.Second):
		t.Fatal("no tools/list_changed after runtime Add")
	}
	want := `{"error":{"code":"unavailable","reason":"no_capacity","message":"pool full","retryable":true}}`

	// Plain call, and the same call with a progress token (runs as a job).
	text, isErr := callText(t, h, &mcpsdk.CallToolParams{Name: "pool_acquire", Arguments: map[string]any{"text": "x"}})
	if !isErr || text != want {
		t.Fatalf("plain error = %s (error %v), want %s", text, isErr, want)
	}
	params := &mcpsdk.CallToolParams{Name: "pool_acquire", Arguments: map[string]any{"text": "x"}}
	params.SetProgressToken("tok-reason")
	if text, isErr = callText(t, h, params); !isErr || text != want {
		t.Fatalf("progress-path error = %s (error %v), want %s", text, isErr, want)
	}
	// Job-start failure on a Long tool (bad input): not retryable as is.
	text, isErr = callText(t, h, &mcpsdk.CallToolParams{Name: "apk_decompile", Arguments: map[string]any{"text": 5}})
	if e := errorOf(t, text); !isErr || e.Code != registry.Invalid || e.Retryable {
		t.Fatalf("job-start error = %s (error %v)", text, isErr)
	}
	// A failed Long job, seen through jobs_wait: the status error has the
	// reason and says it is retryable.
	text, isErr = callText(t, h, &mcpsdk.CallToolParams{Name: "pool_long", Arguments: map[string]any{"text": "x"}})
	if isErr {
		t.Fatalf("long start: %s", text)
	}
	var env struct{ Job string }
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	text, _ = callText(t, h, &mcpsdk.CallToolParams{Name: "jobs_wait", Arguments: map[string]any{"id": env.Job, "timeout": 5}})
	if !strings.Contains(text, `"reason":"no_capacity"`) || !strings.Contains(text, `"retryable":true`) {
		t.Fatalf("jobs_wait lacks reason or retryable: %s", text)
	}
}

// A per-file progress stream is paced: far fewer notifications than
// updates, and the last one carries the final count.
func TestMCPProgressPacedWithinStage(t *testing.T) {
	h := setup(t)
	params := &mcpsdk.CallToolParams{Name: "work_burst", Arguments: map[string]any{"text": "x"}}
	params.SetProgressToken("tok-burst")
	if text, isErr := callText(t, h, params); isErr {
		t.Fatalf("burst=%q", text)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.progMu.Lock()
		msgs := append([]string(nil), h.progMsg...)
		h.progMu.Unlock()
		if n := len(msgs); n > 0 && strings.Contains(msgs[n-1], "2000") {
			if n > 20 {
				t.Fatalf("%d notifications for one stage burst, want paced", n)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("final count never forwarded: %q", msgs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
