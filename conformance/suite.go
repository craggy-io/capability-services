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
		resp, err := http.Get(srv.URL + "/v1/")
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
			return p, json.Unmarshal(raw, &p)
		})
		checkLongViaJobs(t, func() (string, error) {
			job, err := view.Start(ctx, name("slow"), json.RawMessage(`{"label":"go"}`))
			if err != nil {
				return "", err
			}
			raw, err := view.Invoke(ctx, "jobs.wait", json.RawMessage(fmt.Sprintf(`{"id":%q,"timeout":10}`, job.ID())))
			return string(raw), err
		}, "done-go")
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
		checkPagination(t, func(cursor string) (registry.Page[string], error) {
			text, isErr := tools.Call(ctx, view, tool("items.list"), json.RawMessage(fmt.Sprintf(`{"limit":3,"cursor":%q}`, cursor)))
			if isErr {
				return registry.Page[string]{}, fmt.Errorf("%s", text)
			}
			var p registry.Page[string]
			return p, json.Unmarshal([]byte(text), &p)
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
		}, "done-tools")
	})

	t.Run("http", func(t *testing.T) {
		srv := httptest.NewServer(httpapi.Handler(view))
		defer srv.Close()
		post := func(path, body string) (int, http.Header, []byte) {
			resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, resp.Header, b
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
		code, _, _ = post(path("echo"), `{"text":1}`)
		if code != 400 {
			t.Fatalf("invalid http=%d", code)
		}
		code, hdr, b := post(path("slow"), `{"label":"http"}`)
		if code != http.StatusAccepted || hdr.Get("Location") == "" {
			t.Fatalf("slow http=%d %s loc=%q", code, b, hdr.Get("Location"))
		}
		// Wait via jobs.wait capability over HTTP for identical semantics.
		var env struct {
			Job string `json:"job"`
		}
		_ = json.Unmarshal(b, &env)
		checkLongViaJobs(t, func() (string, error) {
			_, _, wb := post("/v1/jobs/wait", fmt.Sprintf(`{"id":%q,"timeout":10}`, env.Job))
			var w struct {
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(wb, &w); err != nil {
				return "", err
			}
			return string(w.Result), nil
		}, "done-http")
		checkPagination(t, func(cursor string) (registry.Page[string], error) {
			_, _, pb := post(path("items.list"), fmt.Sprintf(`{"limit":3,"cursor":%q}`, cursor))
			var w struct {
				Result registry.Page[string] `json:"result"`
			}
			return w.Result, json.Unmarshal(pb, &w)
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
		}, "done-mcp")
		checkPagination(t, func(cursor string) (registry.Page[string], error) {
			text, isErr := call(tool("items.list"), map[string]any{"limit": 3, "cursor": cursor})
			if isErr {
				return registry.Page[string]{}, fmt.Errorf("%s", text)
			}
			var p registry.Page[string]
			return p, json.Unmarshal([]byte(text), &p)
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

// checkLongViaJobs: run returns a jobs.wait-shaped JSON text (or a result
// envelope containing it); require succeeded state and the expected result.
func checkLongViaJobs(t *testing.T, run func() (string, error), wantText string) {
	t.Helper()
	raw, err := run()
	if err != nil {
		t.Fatalf("long run: %v", err)
	}
	if !strings.Contains(raw, wantText) {
		t.Fatalf("long result missing %q in %s", wantText, raw)
	}
	// Where a status is present it must say succeeded (stage progress at
	// 100% alone never counts as completion).
	var probe struct {
		Status *registry.JobStatus `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err == nil && probe.Status != nil {
		if probe.Status.State != registry.JobSucceeded {
			t.Fatalf("long status=%s want succeeded (%s)", probe.Status.State, raw)
		}
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
