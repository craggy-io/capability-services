package httpapi_test

// Regression tests for the 2026-11-04 review findings in httpapi.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/httpapi"
	"github.com/craggy-io/capability-services/registry"
)

func doReq(t *testing.T, method, url, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
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

// A headerless (anonymous) remote request must not take JobFor's
// trusted-local path: jobs from another session are invisible to it.
func TestReviewHeaderlessJobAccessDenied(t *testing.T) {
	_, srv := setup(t)
	resp, b := post(t, srv.URL+"/v1/apk/decompile", `{"id":"a"}`, map[string]string{"Craggy-Session": "alice"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, b)
	}
	var env struct {
		Job string `json:"job"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Job == "" {
		t.Fatalf("envelope = %s err=%v", b, err)
	}
	if resp, _ := doReq(t, http.MethodGet, srv.URL+"/v1/jobs/"+env.Job, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("headerless GET job = %d, want 404", resp.StatusCode)
	}
	if resp, _ := doReq(t, http.MethodDelete, srv.URL+"/v1/jobs/"+env.Job, "", map[string]string{"Craggy-Session": "mallory"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stranger DELETE job = %d, want 404", resp.StatusCode)
	}
	if resp, _ := doReq(t, http.MethodGet, srv.URL+"/v1/jobs/"+env.Job, "", map[string]string{"Craggy-Session": "alice"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("owner GET job = %d, want 200", resp.StatusCode)
	}
}

// The Craggy-Session header must not overwrite a Session an auth wrapper
// already put in the request context.
func TestReviewSessionHeaderDoesNotOverrideCtxCaller(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	slow := registry.Func("work.slow", "slow", func(ctx context.Context, in echoIn) (echoIn, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return in, nil
		case <-ctx.Done():
			return echoIn{}, ctx.Err()
		}
	}, registry.AsLong(time.Second))
	if err := r.Add(slow); err != nil {
		t.Fatal(err)
	}
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := registry.WithCaller(req.Context(), registry.Caller{ID: "user:alice", Session: "alice-session"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	srv := httptest.NewServer(auth(httpapi.Handler(r.View(registry.All))))
	t.Cleanup(srv.Close)
	resp, b := post(t, srv.URL+"/v1/work/slow", `{"id":"a"}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, b)
	}
	var env struct {
		Job string `json:"job"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	// The wrapper's identity owns the job; a spoofed header naming some
	// other session changes nothing either way: the request still acts as
	// alice-session (200), proving the header did not override it.
	if resp, _ := doReq(t, http.MethodGet, srv.URL+"/v1/jobs/"+env.Job, "", map[string]string{"Craggy-Session": "victim"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with spoofed header = %d, want 200 as the wrapper's caller", resp.StatusCode)
	}
}

// Bodies past the 32MiB cap are rejected, not silently truncated: trailing
// garbage beyond the cap must not go unvalidated.
func TestReviewBodyTooLargeRejected(t *testing.T) {
	_, srv := setup(t)
	pad := `{"id":"x"}` + strings.Repeat(" ", 33<<20) + `GARBAGE`
	resp, b := post(t, srv.URL+"/v1/storage/artifact/get", pad, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "too large") {
		t.Fatalf("oversize = %d %s", resp.StatusCode, b)
	}
}

// DELETE on the events URL is not a cancel.
func TestReviewDeleteEventsDoesNotCancel(t *testing.T) {
	_, srv := setup(t)
	hdr := map[string]string{"Craggy-Session": "s1"}
	_, b := post(t, srv.URL+"/v1/apk/decompile", `{"id":"a"}`, hdr)
	var env struct {
		Job string `json:"job"`
	}
	_ = json.Unmarshal(b, &env)
	if resp, _ := doReq(t, http.MethodDelete, srv.URL+"/v1/jobs/"+env.Job+"/events", "", hdr); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE events = %d, want 404", resp.StatusCode)
	}
	// The job was not cancelled: it completes successfully.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, b = doReq(t, http.MethodGet, srv.URL+"/v1/jobs/"+env.Job, "", hdr)
		if strings.Contains(string(b), `"succeeded"`) {
			break
		}
		if strings.Contains(string(b), `"cancelled"`) {
			t.Fatal("DELETE /events cancelled the job")
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never succeeded: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// /v1 is a path segment: /v1health is not the health route.
func TestReviewNoV1Aliases(t *testing.T) {
	_, srv := setup(t)
	for _, p := range []string{"/v1health", "/v1openapi.json"} {
		if resp, _ := doReq(t, http.MethodGet, srv.URL+p, "", nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", p, resp.StatusCode)
		}
	}
}

// The generated document declares the templated id parameter (valid
// OpenAPI 3.1), keeps the session parameter alongside it, and documents
// the statuses the handlers actually return.
func TestReviewOpenAPIDeclaresJobIDAndStatuses(t *testing.T) {
	_, srv := setup(t)
	_, b := doReq(t, http.MethodGet, srv.URL+"/v1/openapi.json", "", nil)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	for _, p := range []string{"/v1/jobs/{id}", "/v1/jobs/{id}/events"} {
		item, ok := paths[p].(map[string]any)
		if !ok {
			t.Fatalf("missing path %s", p)
		}
		for method, rawOp := range item {
			op := rawOp.(map[string]any)
			params, _ := op["parameters"].([]any)
			raw, _ := json.Marshal(params)
			if !strings.Contains(string(raw), "JobId") {
				t.Errorf("%s %s: id parameter not declared: %s", method, p, raw)
			}
			if !strings.Contains(string(raw), "CraggySession") {
				t.Errorf("%s %s: session parameter overwritten: %s", method, p, raw)
			}
		}
	}
	health := paths["/v1/health"].(map[string]any)["get"].(map[string]any)
	if _, ok := health["responses"].(map[string]any)["503"]; !ok {
		t.Error("health op does not document its 503")
	}
	jobGet := paths["/v1/jobs/{id}"].(map[string]any)["get"].(map[string]any)
	if _, ok := jobGet["responses"].(map[string]any)["404"]; !ok {
		t.Error("job get op does not document its 404")
	}
}

// A capability named "health" coexists with the readiness endpoint in the
// document: GET is readiness, POST invokes the capability.
func TestReviewOpenAPIHealthCapabilityCoexists(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	c := registry.Func("health", "Health capability", func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil })
	if err := r.Add(c); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.Handler(r.View(registry.All)))
	t.Cleanup(srv.Close)
	_, b := doReq(t, http.MethodGet, srv.URL+"/v1/openapi.json", "", nil)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	item := doc["paths"].(map[string]any)["/v1/health"].(map[string]any)
	if _, ok := item["get"]; !ok {
		t.Error("health readiness GET missing")
	}
	if _, ok := item["post"]; !ok {
		t.Error("health capability POST overwritten by readiness op")
	}
}

// Prefer: wait that expires exactly as the job finishes reports the real
// outcome, never a spurious 499. (The deterministic part — Wait prefers a
// finished job over an expired wait context — is pinned in registry.)
func TestReviewPreferWaitReportsRealOutcome(t *testing.T) {
	_, srv := setup(t)
	for i := 0; i < 25; i++ {
		resp, b := post(t, srv.URL+"/v1/apk/decompile", `{"id":"a"}`,
			map[string]string{"Craggy-Session": "s", "Prefer": "wait=0.11"})
		if resp.StatusCode == 499 {
			t.Fatalf("iteration %d: spurious 499: %s", i, b)
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			t.Fatalf("iteration %d: status %d: %s", i, resp.StatusCode, b)
		}
	}
}
