package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/httpapi"
	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
)

type echoIn struct {
	ID string `json:"id"`
}

func setup(t *testing.T) (*registry.Registry, *httptest.Server) {
	t.Helper()
	r := registry.New()
	t.Cleanup(r.Close)
	get := registry.Func("storage.artifact.get", "Get artifact",
		func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
		registry.WithEffects(registry.ReadOnly))
	getV2 := registry.Func("storage.artifact.get", "Get artifact v2",
		func(ctx context.Context, in echoIn) (map[string]string, error) {
			return map[string]string{"id": in.ID + "-v2"}, nil
		}, registry.WithVersion(2), registry.WithEffects(registry.ReadOnly))
	slow := registry.Func("apk.decompile", "Decompile",
		func(ctx context.Context, in echoIn) (echoIn, error) {
			registry.Progress(ctx, registry.Update{Stage: "push", Done: 100, Total: 100, Unit: "bytes"})
			select {
			case <-time.After(120 * time.Millisecond):
			case <-ctx.Done():
				return echoIn{}, ctx.Err()
			}
			registry.Progress(ctx, registry.Update{Stage: "finalise"})
			return in, nil
		}, registry.AsLong(time.Second),
		registry.WithAccepts(registry.Subject{Type: "android.apk", Level: "validated"}),
		registry.WithEmits(registry.Subject{Type: "text.java"}))
	denied := registry.Func("vault.open", "Open",
		func(ctx context.Context, in echoIn) (echoIn, error) {
			return echoIn{}, registry.Errorf(registry.Denied, "not for you")
		})
	down := registry.Func("down.cap", "Down",
		func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
		registry.WithCheck(func(ctx context.Context) error { return errors.New("no docker") }))
	caps := append([]registry.Capability{get, getV2, slow, denied, down}, jobs.Capabilities()...)
	if err := r.Add(caps...); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.Handler(r.View(registry.All)))
	t.Cleanup(srv.Close)
	return r, srv
}

func post(t *testing.T, url, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestInvokeAndErrorMapping(t *testing.T) {
	_, srv := setup(t)
	resp, b := post(t, srv.URL+"/v1/storage/artifact/get", `{"id":"a1"}`, nil)
	if resp.StatusCode != 200 || string(b) == "" || !strings.Contains(string(b), `"a1-v2"`) {
		t.Fatalf("bare (current v2) = %d %s", resp.StatusCode, b)
	}
	resp, b = post(t, srv.URL+"/v1/storage/artifact/get@1", `{"id":"a1"}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"id":"a1"}`) {
		t.Fatalf("pinned v1 = %d %s", resp.StatusCode, b)
	}
	resp, b = post(t, srv.URL+"/v1/storage/artifact/get", `{"id":42}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(string(b), `"invalid"`) {
		t.Fatalf("bad input = %d %s", resp.StatusCode, b)
	}
	resp, _ = post(t, srv.URL+"/v1/no/such/cap", `{}`, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown = %d", resp.StatusCode)
	}
	resp, b = post(t, srv.URL+"/v1/vault/open", `{"id":"x"}`, nil)
	if resp.StatusCode != 403 || !strings.Contains(string(b), `"denied"`) {
		t.Fatalf("denied = %d %s", resp.StatusCode, b)
	}
	resp, b = post(t, srv.URL+"/v1/down/cap", `{"id":"x"}`, nil)
	if resp.StatusCode != 503 || !strings.Contains(string(b), "no docker") {
		t.Fatalf("unready = %d %s", resp.StatusCode, b)
	}
}

func TestLongJobFlowAndSSE(t *testing.T) {
	_, srv := setup(t)
	resp, b := post(t, srv.URL+"/v1/apk/decompile", `{"id":"app.apk"}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("long POST = %d %s", resp.StatusCode, b)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/v1/jobs/job-") {
		t.Fatalf("Location=%q", loc)
	}
	var env struct {
		Job    string             `json:"job"`
		Status registry.JobStatus `json:"status"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Job == "" {
		t.Fatalf("202 body=%s err=%v", b, err)
	}
	// SSE replay+live until terminal.
	resp2, err := http.Get(srv.URL + loc + "/events")
	if err != nil {
		t.Fatal(err)
	}
	sse, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	s := string(sse)
	if resp2.Header.Get("Content-Type") != "text/event-stream" ||
		!strings.Contains(s, `"stage":"push"`) || !strings.Contains(s, `"unit":"bytes"`) ||
		!strings.Contains(s, "event: succeeded") {
		t.Fatalf("SSE=[%s] ct=%s", s, resp2.Header.Get("Content-Type"))
	}
	// Stage 100% event must not itself be the terminal event: push appears
	// before finalise/succeeded in the stream.
	if strings.Index(s, `"stage":"push"`) > strings.Index(s, "event: succeeded") {
		t.Fatal("SSE terminal came before later stages")
	}
	resp3, err := http.Get(srv.URL + loc)
	if err != nil {
		t.Fatal(err)
	}
	gb, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if !strings.Contains(string(gb), `"state":"succeeded"`) || !strings.Contains(string(gb), `"result"`) {
		t.Fatalf("GET job=%s", gb)
	}
	// Prefer: wait returns the finished result directly.
	resp, b = post(t, srv.URL+"/v1/apk/decompile", `{"id":"b.apk"}`, map[string]string{"Prefer": "wait=5"})
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"b.apk"`) {
		t.Fatalf("prefer wait = %d %s", resp.StatusCode, b)
	}
	// DELETE cancels a fresh job.
	resp, b = post(t, srv.URL+"/v1/apk/decompile", `{"id":"c.apk"}`, nil)
	loc = resp.Header.Get("Location")
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+loc, nil)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != 200 {
		t.Fatalf("DELETE job=%d", dresp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gr, _ := http.Get(srv.URL + loc)
		bb, _ := io.ReadAll(gr.Body)
		gr.Body.Close()
		if strings.Contains(string(bb), `"state":"cancelled"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancelled job never reached cancelled state")
}

func TestListHealthOpenAPI(t *testing.T) {
	_, srv := setup(t)
	resp, err := http.Get(srv.URL + "/v1/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var lst struct {
		Capabilities []map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(b, &lst); err != nil {
		t.Fatalf("list parse: %v %s", err, b)
	}
	// List: current only (get@v1 hidden), down included with ready=false.
	seen := map[string]map[string]any{}
	for _, c := range lst.Capabilities {
		seen[c["ref"].(string)] = c
	}
	if seen["storage.artifact.get@1"] != nil || seen["storage.artifact.get@2"] == nil {
		t.Fatalf("list refs=%v", keys(seen))
	}
	if seen["down.cap@1"]["ready"] != false || seen["down.cap@1"]["checkError"] != "no docker" {
		t.Fatalf("down entry=%v", seen["down.cap@1"])
	}
	accepts, _ := seen["apk.decompile@1"]["accepts"].([]any)
	if len(accepts) != 1 {
		t.Fatalf("apk.decompile accepts = %v", seen["apk.decompile@1"]["accepts"])
	}
	emits, _ := seen["apk.decompile@1"]["emits"].([]any)
	if len(emits) != 1 {
		t.Fatalf("apk.decompile emits = %v", seen["apk.decompile@1"]["emits"])
	}
	resp, _ = http.Get(srv.URL + "/v1/?all=1")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"storage.artifact.get@1"`) {
		t.Fatal("all=1 missing pinned v1")
	}
	resp, _ = http.Get(srv.URL + "/v1/health")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(string(b), `"ok":false`) {
		t.Fatalf("health=%d %s", resp.StatusCode, b)
	}
	resp, _ = http.Get(srv.URL + "/v1/openapi.json")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi ver=%v", doc["openapi"])
	}
	paths := doc["paths"].(map[string]any)
	for _, want := range []string{"/v1/storage/artifact/get", "/v1/storage/artifact/get@1", "/v1/storage/artifact/get@2", "/v1/jobs/{id}/events"} {
		if paths[want] == nil {
			t.Fatalf("openapi missing path %s (has %v)", want, keysOf(paths))
		}
	}
}

func keys(m map[string]map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
func keysOf(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSupersededHeaderAndSessionHeader(t *testing.T) {
	r, srv := setup(t)
	var gotSession string
	repl := registry.Func("new.get", "replacement",
		func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
		registry.WithSupersedes("storage.artifact.get"))
	orig := registry.Func("probe.session", "probe",
		func(ctx context.Context, in echoIn) (echoIn, error) {
			if c, ok := registry.CallerFrom(ctx); ok {
				gotSession = c.Session
			}
			return in, nil
		})
	if err := r.Add(repl, orig); err != nil {
		t.Fatal(err)
	}
	resp, _ := post(t, srv.URL+"/v1/storage/artifact/get", `{"id":"a"}`, nil)
	if got := resp.Header.Get("Craggy-Superseded-By"); got != "new.get" {
		t.Fatalf("superseded header=%q status=%d", got, resp.StatusCode)
	}
	post(t, srv.URL+"/v1/probe/session", `{"id":"a"}`, map[string]string{"Craggy-Session": "ui-investigation-3"})
	if gotSession != "ui-investigation-3" {
		t.Fatalf("session header not mapped to Caller.Session: %q", gotSession)
	}
	// POST jobs/list is a capability call, not the job route.
	resp, b := post(t, srv.URL+"/v1/jobs/list", `{}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"items"`) {
		t.Fatalf("jobs.list over HTTP = %d %s", resp.StatusCode, b)
	}
	// GET on an invoke path is 404 via error envelope.
	resp2, _ := http.Get(srv.URL + "/v1/storage/artifact/get")
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("GET invoke path=%d", resp2.StatusCode)
	}
}

func TestCallerOptionAndContextWrapper(t *testing.T) {
	r := registry.New()
	defer r.Close()
	var gotID string
	c := registry.Func("probe.who", "who", func(ctx context.Context, in echoIn) (echoIn, error) {
		cal, _ := registry.CallerFrom(ctx)
		gotID = cal.ID
		return in, nil
	})
	if err := r.Add(c); err != nil {
		t.Fatal(err)
	}
	h := httpapi.Handler(r.View(registry.All), httpapi.Caller(func(r *http.Request) registry.Caller {
		return registry.Caller{ID: "user:" + r.Header.Get("X-User"), Kind: "user"}
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	post(t, srv.URL+"/v1/probe/who", `{"id":"1"}`, map[string]string{"X-User": "phil"})
	if gotID != "user:phil" {
		t.Fatalf("caller option: %q", gotID)
	}
	// An auth wrapper putting Caller in the context wins over the option.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := registry.WithCaller(r.Context(), registry.Caller{ID: "user:wrapper"})
		h.ServeHTTP(w, r.WithContext(ctx))
	})
	srv2 := httptest.NewServer(wrapped)
	defer srv2.Close()
	post(t, srv2.URL+"/v1/probe/who", `{"id":"1"}`, map[string]string{"X-User": "phil"})
	if gotID != "user:wrapper" {
		t.Fatalf("wrapper caller: %q", gotID)
	}
}

func TestErrorEnvelopeCarriesReason(t *testing.T) {
	r, srv := setup(t)
	full := func(ctx context.Context, in echoIn) (echoIn, error) {
		return in, registry.WrapReason(registry.Unavailable, registry.ReasonNoCapacity, errors.New("pool full"))
	}
	if err := r.Add(
		registry.Func("pool.acquire", "Acquire", full),
		registry.Func("pool.long", "Acquire long", full, registry.AsLong(time.Second)),
	); err != nil {
		t.Fatal(err)
	}
	want := `{"error":{"code":"unavailable","reason":"no_capacity","message":"pool full","retryable":true}}`
	resp, b := post(t, srv.URL+"/v1/pool/acquire", `{"id":"x"}`, nil)
	if resp.StatusCode != 503 || strings.TrimSpace(string(b)) != want {
		t.Fatalf("sync error = %d %s", resp.StatusCode, b)
	}
	// A Long call waited on via Prefer reports the job's failure the same way.
	resp, b = post(t, srv.URL+"/v1/pool/long", `{"id":"x"}`, map[string]string{"Prefer": "wait=5"})
	if resp.StatusCode != 503 || strings.TrimSpace(string(b)) != want {
		t.Fatalf("long waited error = %d %s", resp.StatusCode, b)
	}
	// Job status for a failed Long call carries the reason too.
	resp, b = post(t, srv.URL+"/v1/pool/long", `{"id":"x"}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("long POST = %d %s", resp.StatusCode, b)
	}
	loc := resp.Header.Get("Location")
	deadline := time.Now().Add(5 * time.Second)
	for {
		gr, err := http.Get(srv.URL + loc)
		if err != nil {
			t.Fatal(err)
		}
		gb, _ := io.ReadAll(gr.Body)
		gr.Body.Close()
		if strings.Contains(string(gb), `"state":"failed"`) {
			if !strings.Contains(string(gb), `"error":{"code":"unavailable","reason":"no_capacity","message":"pool full","retryable":true}`) {
				t.Fatalf("job status lacks reason: %s", gb)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never failed: %s", gb)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The registry's own not-ready refusal has its reason.
	resp, b = post(t, srv.URL+"/v1/down/cap", `{"id":"x"}`, nil)
	if resp.StatusCode != 503 || !strings.Contains(string(b), `"reason":"not_ready"`) {
		t.Fatalf("unready = %d %s", resp.StatusCode, b)
	}
	// Reason-less errors omit reason; retryable is always spelled out.
	resp, b = post(t, srv.URL+"/v1/vault/open", `{"id":"x"}`, nil)
	if resp.StatusCode != 403 || strings.TrimSpace(string(b)) != `{"error":{"code":"denied","message":"not for you","retryable":false}}` {
		t.Fatalf("denied = %d %s", resp.StatusCode, b)
	}
	// OpenAPI documents the field.
	resp, _ = http.Get(srv.URL + "/v1/openapi.json")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var doc struct {
		Components struct {
			Schemas struct {
				Error struct {
					Properties struct {
						Error struct {
							Properties map[string]any `json:"properties"`
						} `json:"error"`
					} `json:"properties"`
				} `json:"Error"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Components.Schemas.Error.Properties.Error.Properties["reason"] == nil {
		t.Fatalf("openapi Error schema lacks reason: %s", b)
	}
}

// The OpenAPI document describes the Craggy-Session request header on every
// operation and the Craggy-Superseded-By response header on capability
// results.
func TestOpenAPIDescribesCraggyHeaders(t *testing.T) {
	_, srv := setup(t)
	resp, err := http.Get(srv.URL + "/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Paths      map[string]map[string]map[string]any `json:"paths"`
		Components struct {
			Parameters map[string]map[string]any `json:"parameters"`
			Headers    map[string]map[string]any `json:"headers"`
		} `json:"components"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if p := doc.Components.Parameters["CraggySession"]; p["name"] != "Craggy-Session" || p["in"] != "header" {
		t.Fatalf("components.parameters.CraggySession = %v", p)
	}
	if h := doc.Components.Headers["CraggySupersededBy"]; h["schema"] == nil {
		t.Fatalf("components.headers.CraggySupersededBy = %v", h)
	}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			b, _ := json.Marshal(op["parameters"])
			if !strings.Contains(string(b), `"#/components/parameters/CraggySession"`) {
				t.Fatalf("%s %s lacks the Craggy-Session parameter: %s", method, path, b)
			}
		}
	}
	for _, path := range []string{"/v1/storage/artifact/get", "/v1/storage/artifact/get@1", "/v1/apk/decompile"} {
		responses, _ := doc.Paths[path]["post"]["responses"].(map[string]any)
		for _, status := range []string{"200", "202"} {
			r, ok := responses[status].(map[string]any)
			if !ok {
				continue
			}
			b, _ := json.Marshal(r["headers"])
			if !strings.Contains(string(b), `"Craggy-Superseded-By":{"$ref":"#/components/headers/CraggySupersededBy"}`) {
				t.Fatalf("POST %s %s lacks Craggy-Superseded-By: %s", path, status, b)
			}
		}
	}
}
