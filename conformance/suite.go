package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	sl "go.starlark.net/starlark"

	"github.com/craggy-io/capability-services/httpapi"
	regmcp "github.com/craggy-io/capability-services/mcp"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
	"github.com/craggy-io/capability-services/tools"
)

// httpStatus is the status the HTTP projection uses per code.
var httpStatus = map[registry.Code]int{
	registry.Invalid: http.StatusBadRequest, registry.Denied: http.StatusForbidden,
	registry.NotFound: http.StatusNotFound, registry.Conflict: http.StatusConflict,
	registry.Cancelled: 499, registry.Unavailable: http.StatusServiceUnavailable,
	registry.Internal: http.StatusInternalServerError,
}

// canonical normalizes JSON bytes for cross-projection comparison.
func canonical(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// Suite runs the standard capability set through every projection and
// requires identical results, error codes, job behaviour and pagination:
// once Go-registered (test.*), once Starlark-defined through
// starlarkcap.Load (star.*).
func Suite(t *testing.T) {
	t.Helper()
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	loaded, err := LoadScript(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	view := r.View(registry.All)
	ctx := registry.WithCaller(context.Background(), registry.Caller{ID: "agent:conf", Kind: "agent", Session: "conf-1"})

	t.Run("script origin", func(t *testing.T) {
		srv := httptest.NewServer(httpapi.Handler(view))
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/v1/?all=1") // all: List hides star.old and star.ver@1
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var listing struct {
			Capabilities []registry.Descriptor `json:"capabilities"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
			t.Fatal(err)
		}
		want := registry.Origin{Kind: "script", Ref: "conformance.star", Digest: loaded.Digest}
		seen := 0
		for _, d := range listing.Capabilities {
			switch {
			case strings.HasPrefix(d.Name, "star."):
				seen++
				if d.Origin == nil || *d.Origin != want {
					t.Fatalf("%s origin = %+v, want %+v", d.Name, d.Origin, want)
				}
			case d.Origin != nil:
				t.Fatalf("Go capability %s has origin %+v", d.Name, d.Origin)
			}
		}
		if seen != len(loaded.Capabilities) {
			t.Fatalf("listing shows %d star.* capabilities, Load installed %v", seen, loaded.Capabilities)
		}
	})
	for _, ns := range []string{"test", "star"} {
		t.Run(ns, func(t *testing.T) { projections(t, r, view, ctx, ns) })
	}
	t.Run("restricted view", func(t *testing.T) { restrictedView(t, r, ctx) })
}

// restrictedView requires every projection to enforce a view that
// excludes the admin-tagged capabilities, and the job-access rule for a
// second caller: filtering and enforcement are the same thing.
func restrictedView(t *testing.T, r *registry.Registry, ctx context.Context) {
	restricted := r.View(registry.Not(registry.Tagged("admin")))
	if _, ok := restricted.Get("test.admin"); ok {
		t.Fatal("test.admin visible in the restricted view")
	}
	// tools: absent from Defs, and a call is not_found/unknown_tool.
	for _, d := range tools.Defs(restricted) {
		if d.Name == "test_admin" || d.Name == "star_admin" {
			t.Fatalf("admin tool in restricted Defs: %s", d.Name)
		}
	}
	if res := tools.Do(ctx, restricted, "test_admin", json.RawMessage(`{"text":"x"}`)); !res.IsError || res.Code != registry.NotFound || res.Reason != "unknown_tool" {
		t.Fatalf("restricted tools.Do = %+v", res)
	}
	// HTTP: the admin route 404s with the error envelope.
	srv := httptest.NewServer(httpapi.Handler(restricted))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/test/admin", "application/json", strings.NewReader(`{"text":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(b), `"code":"not_found"`) {
		t.Fatalf("restricted http = %d %s", resp.StatusCode, b)
	}
	// MCP: not in the tool list.
	cs, stop := mcpClient(t, r, restricted)
	defer stop()
	lt, err := cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range lt.Tools {
		if tool.Name == "test_admin" || tool.Name == "star_admin" {
			t.Fatalf("admin tool in restricted MCP list: %s", tool.Name)
		}
	}
	// Starlark: the namespace has no admin, and call() by ref is not_found.
	sctx := registry.WithView(ctx, restricted)
	thread := &sl.Thread{Name: "conformance-restricted"}
	thread.SetMaxExecutionSteps(1_000_000)
	if _, err := sl.ExecFile(thread, "r.star", `x = call("test.admin", text = "x")`, starlarkcap.Globals(sctx)); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("restricted starlark call err = %v", err)
	}
	// Job access: a second session cannot read the first session's job.
	job, err := r.View(registry.All).Start(ctx, "test.slow", json.RawMessage(`{"label":"own"}`))
	if err != nil {
		t.Fatal(err)
	}
	other := registry.WithCaller(context.Background(), registry.Caller{ID: "agent:other", Kind: "agent", Session: "other-1"})
	if res := tools.Do(other, r.View(registry.All), "jobs_get", json.RawMessage(fmt.Sprintf(`{"id":%q}`, job.ID()))); !res.IsError || res.Code != registry.NotFound {
		t.Fatalf("other session jobs_get = %+v, want not_found", res)
	}
	if res := tools.Do(ctx, r.View(registry.All), "jobs_get", json.RawMessage(fmt.Sprintf(`{"id":%q}`, job.ID()))); res.IsError {
		t.Fatalf("owner jobs_get = %+v", res)
	}
}

// projections runs every projection over the capability set under ns.
func projections(t *testing.T, r *registry.Registry, view *registry.View, ctx context.Context, ns string) {
	name := func(n string) string { return ns + "." + n }
	tool := func(n string) string { return ns + "_" + strings.ReplaceAll(n, ".", "_") }
	path := func(n string) string { return "/v1/" + ns + "/" + strings.ReplaceAll(n, ".", "/") }

	const echoInJSON = `{"text":"hello"}`
	const echoWant = `{"text":"hello"}`
	// The fail capability's error, as Invoke reports it, is what every
	// projection must report: as the error envelope in tool results and
	// HTTP bodies, as the coded message in Starlark.
	_, failErr := view.Invoke(ctx, name("fail"), json.RawMessage(echoInJSON))
	if failErr == nil {
		t.Fatalf("%s succeeded", name("fail"))
	}
	failEnv, _ := json.Marshal(registry.ErrorEnvelope{Error: registry.Info(failErr)})
	failWant := string(failEnv)
	switch ns {
	case "test":
		if want := `{"error":{"code":"conflict","message":"boom","retryable":false}}`; failWant != want {
			t.Fatalf("test.fail envelope = %s, want %s", failWant, want)
		}
	case "star":
		// A script's own failure is internal; the cause stays visible.
		if registry.CodeOf(failErr) != registry.Internal || !strings.Contains(failErr.Error(), "conflict: boom") {
			t.Fatalf("star.fail error = %v", failErr)
		}
	}
	// deny/full pin the denied code and the reason+retryable fields the
	// same way: whatever Invoke reports, verbatim, on every projection.
	_, denyErr := view.Invoke(ctx, name("deny"), json.RawMessage(echoInJSON))
	_, fullErr := view.Invoke(ctx, name("full"), json.RawMessage(echoInJSON))
	if denyErr == nil || fullErr == nil {
		t.Fatalf("deny/full succeeded (%v, %v)", denyErr, fullErr)
	}
	denyEnv, _ := json.Marshal(registry.ErrorEnvelope{Error: registry.Info(denyErr)})
	fullEnv, _ := json.Marshal(registry.ErrorEnvelope{Error: registry.Info(fullErr)})
	denyWant, fullWant := string(denyEnv), string(fullEnv)
	if ns == "test" {
		if registry.CodeOf(denyErr) != registry.Denied {
			t.Fatalf("test.deny code = %v", registry.CodeOf(denyErr))
		}
		if want := `{"error":{"code":"unavailable","reason":"no_capacity","message":"pool full","retryable":true}}`; fullWant != want {
			t.Fatalf("test.full envelope = %s, want %s", fullWant, want)
		}
	}

	t.Run("invoke", func(t *testing.T) {
		out, err := view.Invoke(ctx, name("echo"), json.RawMessage(echoInJSON))
		if err != nil || canonical(out) != echoWant {
			t.Fatalf("echo=%s %v", out, err)
		}
		out, err = view.Invoke(ctx, name("add"), json.RawMessage(`{"a":19,"b":23}`))
		if err != nil || canonical(out) != `{"sum":42}` {
			t.Fatalf("add=%s %v", out, err)
		}
		if _, err := view.Invoke(ctx, name("fail"), json.RawMessage(echoInJSON)); registry.CodeOf(err) != registry.CodeOf(failErr) {
			t.Fatalf("fail code=%v", registry.CodeOf(err))
		}
		if _, err := view.Invoke(ctx, name("echo"), json.RawMessage(`{"text":1}`)); registry.CodeOf(err) != registry.Invalid {
			t.Fatalf("invalid code=%v", registry.CodeOf(err))
		}
		if _, err := view.Invoke(ctx, name("unready"), json.RawMessage(echoInJSON)); registry.CodeOf(err) != registry.Unavailable {
			t.Fatalf("unready code=%v", registry.CodeOf(err))
		}
		if _, err := view.Invoke(ctx, name("ghost"), nil); registry.CodeOf(err) != registry.NotFound {
			t.Fatalf("ghost code=%v", registry.CodeOf(err))
		}
		checkPagination(t, func(cursor string) (registry.Page[string], error) {
			in := fmt.Sprintf(`{"limit":3,"cursor":%q}`, cursor)
			raw, err := view.Invoke(ctx, name("items.list"), json.RawMessage(in))
			if err != nil {
				return registry.Page[string]{}, err
			}
			var p registry.Page[string]
			err = json.Unmarshal(raw, &p)
			return p, err
		})
		checkLongViaJobs(t, func() (string, error) {
			job, err := view.Start(ctx, name("slow"), json.RawMessage(`{"label":"go"}`))
			if err != nil {
				return "", err
			}
			raw, err := view.Invoke(ctx, "jobs.wait", json.RawMessage(fmt.Sprintf(`{"id":%q,"timeout":10}`, job.ID())))
			return string(raw), err
		}, name("slow")+"@1", lastStage(ns), "done-go")
	})

	t.Run("tools", func(t *testing.T) {
		defs := tools.Defs(view)
		var names []string
		for _, d := range defs {
			names = append(names, d.Name)
		}
		// Unready capabilities stay out of tool lists, Go-registered and
		// script-defined alike.
		if !slices.Contains(names, tool("echo")) || slices.Contains(names, "test_unready") || slices.Contains(names, "star_unready") {
			t.Fatalf("defs=%v", names)
		}
		text, isErr := tools.Call(ctx, view, tool("echo"), json.RawMessage(echoInJSON))
		if isErr || canonical([]byte(text)) != echoWant {
			t.Fatalf("echo=%q err=%v", text, isErr)
		}
		text, isErr = tools.Call(ctx, view, tool("add"), json.RawMessage(`{"a":19,"b":23}`))
		if isErr || canonical([]byte(text)) != `{"sum":42}` {
			t.Fatalf("add=%q err=%v", text, isErr)
		}
		text, isErr = tools.Call(ctx, view, tool("fail"), json.RawMessage(echoInJSON))
		if !isErr || text != failWant {
			t.Fatalf("fail=%q err=%v", text, isErr)
		}
		for toolName, want := range map[string]string{tool("deny"): denyWant, tool("full"): fullWant} {
			text, isErr = tools.Call(ctx, view, toolName, json.RawMessage(echoInJSON))
			if !isErr || text != want {
				t.Fatalf("%s=%q err=%v, want %s", toolName, text, isErr, want)
			}
		}
		// Structured output and supersession facts ride beside the text.
		if res := tools.Do(ctx, view, tool("echo"), json.RawMessage(echoInJSON)); res.IsError || canonical(res.Structured) != echoWant {
			t.Fatalf("structured echo = %+v", res)
		}
		if slices.Contains(names, tool("old")) || !slices.Contains(names, tool("new")) {
			t.Fatalf("superseded tool listed (or its superseder missing): %v", names)
		}
		if res := tools.Do(ctx, view, tool("old"), json.RawMessage(echoInJSON)); res.IsError || len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "superseded by "+name("new")) {
			t.Fatalf("superseded call = %+v", res)
		}
		checkPagination(t, func(cursor string) (registry.Page[string], error) {
			text, isErr := tools.Call(ctx, view, tool("items.list"), json.RawMessage(fmt.Sprintf(`{"limit":3,"cursor":%q}`, cursor)))
			if isErr {
				return registry.Page[string]{}, fmt.Errorf("%s", text)
			}
			var p registry.Page[string]
			err := json.Unmarshal([]byte(text), &p)
			return p, err
		})
		checkLongViaJobs(t, func() (string, error) {
			text, isErr := tools.Call(ctx, view, tool("slow"), json.RawMessage(`{"label":"tools"}`))
			if isErr {
				return "", fmt.Errorf("%s", text)
			}
			var env tools.JobEnvelope
			if err := json.Unmarshal([]byte(text), &env); err != nil {
				return "", err
			}
			text, isErr = tools.Call(ctx, view, "jobs_wait", json.RawMessage(fmt.Sprintf(`{"id":%q,"timeout":10}`, env.Job)))
			if isErr {
				return "", fmt.Errorf("%s", text)
			}
			return text, nil
		}, name("slow")+"@1", lastStage(ns), "done-tools")
	})

	t.Run("http", func(t *testing.T) {
		srv := httptest.NewServer(httpapi.Handler(view))
		defer srv.Close()
		// One session owns every job this subtest starts.
		session := map[string]string{"Craggy-Session": "conf-1"}
		do := func(method, path, body string, headers map[string]string) (int, http.Header, []byte) {
			req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, resp.Header, b
		}
		post := func(path, body string) (int, http.Header, []byte) {
			return do(http.MethodPost, path, body, session)
		}
		code, _, b := post(path("echo"), echoInJSON)
		var wrapped struct {
			Result json.RawMessage `json:"result"`
		}
		if code != 200 || json.Unmarshal(b, &wrapped) != nil || canonical(wrapped.Result) != echoWant {
			t.Fatalf("echo http=%d %s", code, b)
		}
		code, _, b = post(path("fail"), echoInJSON)
		if code != httpStatus[registry.CodeOf(failErr)] || canonical(b) != canonical([]byte(failWant)) {
			t.Fatalf("fail http=%d %s, want %d %s", code, b, httpStatus[registry.CodeOf(failErr)], failWant)
		}
		for p, want := range map[string]string{path("deny"): denyWant, path("full"): fullWant} {
			code, _, b = post(p, echoInJSON)
			var probe struct {
				Error *registry.ErrorInfo `json:"error"`
			}
			if err := json.Unmarshal([]byte(want), &probe); err != nil {
				t.Fatal(err)
			}
			if code != httpStatus[probe.Error.Code] || canonical(b) != canonical([]byte(want)) {
				t.Fatalf("%s http=%d %s, want %d %s", p, code, b, httpStatus[probe.Error.Code], want)
			}
		}
		code, _, _ = post(path("echo"), `{"text":1}`)
		if code != 400 {
			t.Fatalf("invalid http=%d", code)
		}
		// Supersession and pinning cross the boundary intact.
		code, hdr, b := post(path("old"), echoInJSON)
		if code != 200 || hdr.Get("Craggy-Superseded-By") != name("new") {
			t.Fatalf("superseded http=%d superseded-by=%q %s", code, hdr.Get("Craggy-Superseded-By"), b)
		}
		code, _, b = post(path("ver")+"@1", echoInJSON)
		if code != 200 || !strings.Contains(string(b), `"v1:hello"`) {
			t.Fatalf("pinned http=%d %s", code, b)
		}
		// Prefer: wait returns the finished Long result directly.
		code, _, b = do(http.MethodPost, path("slow"), `{"label":"prefer"}`, map[string]string{"Craggy-Session": "conf-1", "Prefer": "wait=5"})
		if code != 200 || !strings.Contains(string(b), "done-prefer") {
			t.Fatalf("prefer wait http=%d %s", code, b)
		}
		// OpenAPI follows the live set, and health reports the unready cap.
		if code, _, b = do(http.MethodGet, "/v1/openapi.json", "", nil); code != 200 ||
			!strings.Contains(string(b), `"`+path("ver")+`@2"`) || !strings.Contains(string(b), `"deprecated":true`) {
			t.Fatalf("openapi=%d ...%s", code, b[max(0, len(b)-200):])
		}
		if code, _, b = do(http.MethodGet, "/v1/health", "", nil); code != 503 || !strings.Contains(string(b), name("unready")) {
			t.Fatalf("health=%d %s", code, b)
		}
		code, hdr, b = post(path("slow"), `{"label":"http"}`)
		if code != http.StatusAccepted || hdr.Get("Location") == "" {
			t.Fatalf("slow http=%d %s loc=%q", code, b, hdr.Get("Location"))
		}
		loc := hdr.Get("Location")
		var env struct {
			Job string `json:"job"`
		}
		_ = json.Unmarshal(b, &env)
		// The job routes themselves: SSE to the terminal event, GET for the
		// owner only.
		code, hdr, b = do(http.MethodGet, loc+"/events", "", session)
		if code != 200 || hdr.Get("Content-Type") != "text/event-stream" || !strings.Contains(string(b), "event: succeeded") {
			t.Fatalf("sse=%d ct=%q %s", code, hdr.Get("Content-Type"), b)
		}
		if ns == "test" && !strings.Contains(string(b), `"tool":"fake"`) {
			t.Fatalf("sse lost progress data: %s", b)
		}
		if code, _, _ = do(http.MethodGet, loc, "", map[string]string{"Craggy-Session": "other"}); code != 404 {
			t.Fatalf("stranger GET job = %d, want 404", code)
		}
		if code, _, b = do(http.MethodGet, loc, "", session); code != 200 || !strings.Contains(string(b), `"state":"succeeded"`) {
			t.Fatalf("owner GET job = %d %s", code, b)
		}
		// Wait via jobs.wait capability over HTTP for identical semantics.
		checkLongViaJobs(t, func() (string, error) {
			_, _, wb := post("/v1/jobs/wait", fmt.Sprintf(`{"id":%q,"timeout":10}`, env.Job))
			var w struct {
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(wb, &w); err != nil {
				return "", err
			}
			return string(w.Result), nil
		}, name("slow")+"@1", lastStage(ns), "done-http")
		// DELETE cancels a fresh job, which then reports cancelled.
		code, hdr, _ = post(path("slow"), `{"label":"cancel","wait_ms":5000}`)
		if code != http.StatusAccepted {
			t.Fatalf("cancel-target start=%d", code)
		}
		loc = hdr.Get("Location")
		if code, _, _ = do(http.MethodDelete, loc, "", session); code != 200 {
			t.Fatalf("DELETE job = %d", code)
		}
		deadline := 0
		for {
			_, _, b = do(http.MethodGet, loc, "", session)
			if strings.Contains(string(b), `"state":"cancelled"`) {
				break
			}
			deadline++
			if deadline > 200 {
				t.Fatalf("job never cancelled: %s", b)
			}
			time.Sleep(10 * time.Millisecond)
		}
		checkPagination(t, func(cursor string) (registry.Page[string], error) {
			_, _, pb := post(path("items.list"), fmt.Sprintf(`{"limit":3,"cursor":%q}`, cursor))
			var w struct {
				Result registry.Page[string] `json:"result"`
			}
			err := json.Unmarshal(pb, &w)
			return w.Result, err
		})
	})

	t.Run("mcp", func(t *testing.T) {
		cs, stop := mcpClient(t, r, view)
		defer stop()
		call := func(name string, args map[string]any) (string, bool) {
			res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				t.Fatalf("mcp call %s: %v", name, err)
			}
			var sb strings.Builder
			for _, c := range res.Content {
				if tc, ok := c.(*mcpsdk.TextContent); ok {
					sb.WriteString(tc.Text)
				}
			}
			return sb.String(), res.IsError
		}
		text, isErr := call(tool("echo"), map[string]any{"text": "hello"})
		if isErr || canonical([]byte(text)) != echoWant {
			t.Fatalf("mcp echo=%q err=%v", text, isErr)
		}
		text, isErr = call(tool("fail"), map[string]any{"text": "hello"})
		if !isErr || text != failWant {
			t.Fatalf("mcp fail=%q err=%v", text, isErr)
		}
		for toolName, want := range map[string]string{tool("deny"): denyWant, tool("full"): fullWant} {
			text, isErr = call(toolName, map[string]any{"text": "hello"})
			if !isErr || text != want {
				t.Fatalf("mcp %s=%q err=%v, want %s", toolName, text, isErr, want)
			}
		}
		// Structured content rides beside the text; superseded tools are
		// not listed; a progress token exercises the forwarding branch.
		res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tool("echo"), Arguments: map[string]any{"text": "hello"}})
		if err != nil || res.IsError || res.StructuredContent == nil {
			t.Fatalf("mcp structured echo = %v %+v", err, res)
		}
		if sb, _ := json.Marshal(res.StructuredContent); canonical(sb) != echoWant {
			t.Fatalf("mcp structuredContent = %s", sb)
		}
		params := &mcpsdk.CallToolParams{Name: tool("echo"), Arguments: map[string]any{"text": "hello"}}
		params.SetProgressToken("conf-tok")
		if res, err := cs.CallTool(context.Background(), params); err != nil || res.IsError {
			t.Fatalf("mcp progress-token call = %v %+v", err, res)
		}
		lt, err := cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range lt.Tools {
			if tool.Name == "test_old" || tool.Name == "star_old" {
				t.Fatalf("superseded tool listed over MCP: %s", tool.Name)
			}
		}
		checkLongViaJobs(t, func() (string, error) {
			text, isErr := call(tool("slow"), map[string]any{"label": "mcp"})
			if isErr {
				return "", fmt.Errorf("%s", text)
			}
			var env struct {
				Job string `json:"job"`
			}
			if err := json.Unmarshal([]byte(text), &env); err != nil {
				return "", err
			}
			text, isErr = call("jobs_wait", map[string]any{"id": env.Job, "timeout": 10})
			if isErr {
				return "", fmt.Errorf("%s", text)
			}
			return text, nil
		}, name("slow")+"@1", lastStage(ns), "done-mcp")
		checkPagination(t, func(cursor string) (registry.Page[string], error) {
			text, isErr := call(tool("items.list"), map[string]any{"limit": 3, "cursor": cursor})
			if isErr {
				return registry.Page[string]{}, fmt.Errorf("%s", text)
			}
			var p registry.Page[string]
			err := json.Unmarshal([]byte(text), &p)
			return p, err
		})
	})

	t.Run("starlark", func(t *testing.T) {
		sctx := registry.WithView(ctx, view)
		thread := &sl.Thread{Name: "conformance"}
		thread.SetMaxExecutionSteps(1_000_000)
		src := strings.ReplaceAll(`
echoed = test.echo(text = "hello")["text"]
summed = test.add(a = 19, b = 23)["sum"]
def _all_items():
    acc = []
    cursor = ""
    for _ in range(10):
        page = test.items.list(limit = 3, cursor = cursor)
        acc.extend(page["items"])
        cursor = page.get("next", "")
        if cursor == "":
            break
    return acc

items = _all_items()
slow = test.slow(label = "star")["text"]
started = jobs.start("test.slow", label = "js").wait()["result"]["text"]
pinned = call("test.ver@1", text = "p")["text"]
current = test.ver(text = "p")["text"]
`, "test.", ns+".")
		out, err := sl.ExecFile(thread, "conf.star", src, starlarkcap.Globals(sctx))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(out["echoed"]); got != `"hello"` {
			t.Fatalf("starlark echo=%s", got)
		}
		if got := fmt.Sprint(out["summed"]); got != "42" {
			t.Fatalf("starlark add=%s", got)
		}
		items, ok := out["items"].(*sl.List)
		if !ok || items.Len() != len(Items) {
			t.Fatalf("starlark items=%v", out["items"])
		}
		for i := 0; i < items.Len(); i++ {
			if s, _ := items.Index(i).(sl.String); string(s) != Items[i] {
				t.Fatalf("item %d = %q want %q", i, s, Items[i])
			}
		}
		if got := fmt.Sprint(out["slow"]); got != `"done-star"` {
			t.Fatalf("starlark slow=%s", got)
		}
		if got := fmt.Sprint(out["started"]); got != `"done-js"` {
			t.Fatalf("starlark jobs.start=%s", got)
		}
		if got := fmt.Sprint(out["pinned"]); got != `"v1:p"` {
			t.Fatalf("starlark pinned ver=%s", got)
		}
		if got := fmt.Sprint(out["current"]); got != `"v2:p"` {
			t.Fatalf("starlark current ver=%s", got)
		}
		// Failure crosses with its code.
		_, err = sl.ExecFile(thread, "conf2.star", "x = "+ns+`.fail(text="hello")`, starlarkcap.Globals(sctx))
		if err == nil || !strings.Contains(err.Error(), failErr.Error()) {
			t.Fatalf("starlark fail err=%v, want %v", err, failErr)
		}
	})
}

func checkPagination(t *testing.T, fetch func(cursor string) (registry.Page[string], error)) {
	t.Helper()
	seen := []string{}
	cursor := ""
	for i := 0; i < 10; i++ {
		p, err := fetch(cursor)
		if err != nil {
			t.Fatalf("page fetch: %v", err)
		}
		seen = append(seen, p.Items...)
		if p.Next == "" {
			break
		}
		cursor = p.Next
	}
	if strings.Join(seen, ",") != strings.Join(Items, ",") {
		t.Fatalf("pagination visited %v, want each of %v exactly once in order", seen, Items)
	}
}

// lastStage is the stage a finished ns.slow job last reported: the Go
// capability's own "finalise", or the script wrapper's "delegate".
func lastStage(ns string) string {
	if ns == "star" {
		return "delegate"
	}
	return "finalise"
}

// checkLongViaJobs: run returns a jobs.wait-shaped JSON text (or a result
// envelope containing it); require the decoded status to agree, field by
// field, in every projection — not just to contain the result text.
func checkLongViaJobs(t *testing.T, run func() (string, error), wantCap, wantLastStage, wantText string) {
	t.Helper()
	raw, err := run()
	if err != nil {
		t.Fatalf("long run: %v", err)
	}
	if !strings.Contains(raw, wantText) {
		t.Fatalf("long result missing %q in %s", wantText, raw)
	}
	var probe struct {
		Status *registry.JobStatus `json:"status"`
		Result json.RawMessage     `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil || probe.Status == nil {
		t.Fatalf("long result carries no decodable status: err=%v raw=%s", err, raw)
	}
	st := probe.Status
	// Stage progress at 100% alone never counts as completion.
	if st.State != registry.JobSucceeded {
		t.Fatalf("long status=%s want succeeded (%s)", st.State, raw)
	}
	if st.Capability != wantCap {
		t.Fatalf("long capability=%q want %q", st.Capability, wantCap)
	}
	if st.ID == "" || st.Seq < 2 || st.Started.IsZero() || st.Ended.IsZero() {
		t.Fatalf("long status missing id/seq/timestamps: %+v", st)
	}
	if st.Last.Stage != wantLastStage {
		t.Fatalf("long last stage=%q want %q", st.Last.Stage, wantLastStage)
	}
	if st.Error != nil {
		t.Fatalf("long status error=%+v on success", st.Error)
	}
	if st.Caller.ID == "" && st.Caller.Session == "" && st.Caller.Kind == "" {
		t.Fatalf("long status carries no caller identity: %+v", st)
	}
	if len(probe.Result) > 0 && !strings.Contains(string(probe.Result), wantText) {
		t.Fatalf("long result field=%s, want it to contain %q", probe.Result, wantText)
	}
}

func mcpClient(t *testing.T, r *registry.Registry, view *registry.View) (*mcpsdk.ClientSession, func()) {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "conformance", Version: "1"}, nil)
	stop := regmcp.Serve(server, view)
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "conformance-client", Version: "1"}, nil)
	st, ct := mcpsdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cs, func() {
		_ = cs.Close()
		_ = ss.Close()
		stop()
	}
}
