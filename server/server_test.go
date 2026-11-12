package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/foundation-kit/feed"

	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/server"
)

type echo struct {
	Message string `json:"message"`
}

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r := registry.New()
	t.Cleanup(r.Close)
	caps := []registry.Capability{
		registry.Func("test.echo", "Echo a message",
			func(_ context.Context, in echo) (echo, error) { return in, nil },
			registry.WithEffects(registry.ReadOnly|registry.Idempotent)),
		registry.Func("test.whoami", "Return the caller id",
			func(ctx context.Context, _ struct{}) (echo, error) {
				c, _ := registry.CallerFrom(ctx)
				return echo{Message: c.ID}, nil
			}),
	}
	if err := r.Add(append(caps, jobs.Capabilities()...)...); err != nil {
		t.Fatal(err)
	}
	return r
}

func do(t *testing.T, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestInvokeAndAuthPolicy(t *testing.T) {
	r := testRegistry(t)
	auth := func(req *http.Request) (registry.Caller, error) {
		if req.Header.Get("Authorization") != "Bearer secret" {
			return registry.Caller{}, errors.New("missing or invalid token")
		}
		return registry.Caller{ID: "user:local", Kind: "user"}, nil
	}
	s, err := server.New(server.Config{API: r.View(registry.All), Authenticate: auth, Name: "t", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp := do(t, http.MethodPost, ts.URL+"/v1/test/echo", `{"message":"hi"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp = do(t, http.MethodPost, ts.URL+"/v1/test/whoami", `{}`, map[string]string{"Authorization": "Bearer secret"})
	var out struct{ Result echo }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Result.Message != "user:local" {
		t.Fatalf("caller from Authenticate not applied: %+v", out)
	}
}

func TestNonLoopbackNeedsAuth(t *testing.T) {
	v := testRegistry(t).View(registry.All)
	if _, err := server.New(server.Config{Addr: "0.0.0.0:0", API: v}); err == nil {
		t.Fatal("want refusal without Authenticate")
	}
	if _, err := server.New(server.Config{Addr: "127.0.0.1:0", API: v}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.New(server.Config{}); err == nil {
		t.Fatal("want error without API view")
	}
}

// A server with no Authenticate trusts its loopback address, which every web
// page the user opens can reach too.
func TestAnonymousServerRefusesWhatAWebPageCanSend(t *testing.T) {
	s, err := server.New(server.Config{API: testRegistry(t).View(registry.All)})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	post := func(headers map[string]string, host string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/test/echo", strings.NewReader(`{"message":"hi"}`))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(nil, ""); code != http.StatusOK {
		t.Fatalf("a local client: %d", code)
	}
	if code := post(map[string]string{"Sec-Fetch-Site": "same-origin"}, ""); code != http.StatusOK {
		t.Fatalf("the server's own page: %d", code)
	}
	refused := map[string]int{
		"a page on another site":      post(map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"}, ""),
		"another origin, old browser": post(map[string]string{"Origin": "https://evil.example"}, ""),
		"a rebound host name":         post(nil, "evil.example:8787"),
	}
	for name, code := range refused {
		if code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
}

func TestEventsReplayAndReset(t *testing.T) {
	hub := feed.New[string](feed.Options{})
	hub.Publish("one")
	hub.Publish("two")
	s, _ := server.New(server.Config{API: testRegistry(t).View(registry.All),
		Routes: map[string]http.Handler{"/v1/events": server.Events(hub)}})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	read := func(hdr map[string]string, want ...string) {
		t.Helper()
		resp := do(t, http.MethodGet, ts.URL+"/v1/events", "", hdr)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		deadline := time.AfterFunc(5*time.Second, func() { resp.Body.Close() })
		defer deadline.Stop()
		for _, w := range want {
			found := false
			for sc.Scan() {
				if strings.Contains(sc.Text(), w) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("stream lacks %q", w)
			}
		}
	}
	read(map[string]string{"Last-Event-ID": "1"}, "event: ready", `"value":"two"`)
	read(map[string]string{"Last-Event-ID": "99"}, "event: reset", "event: ready")
}

func TestListenerAnswers503UntilServe(t *testing.T) {
	l, err := server.Listen("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + l.Addr() + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("starting: %d", resp.StatusCode)
	}
	s, _ := server.New(server.Config{API: testRegistry(t).View(registry.All)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Serve(ctx, s) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + l.Addr() + "/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("never served")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMCPMounted(t *testing.T) {
	r := testRegistry(t)
	s, _ := server.New(server.Config{API: r.View(registry.All), MCP: r.View(registry.Match("test.*")),
		MCPCaller: registry.Caller{ID: "agent:mcp"}, Name: "t", Version: "1"})
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"test_whoami","arguments":{}}}`
	resp := do(t, http.MethodPost, ts.URL+server.MCPPath, body, map[string]string{
		"Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "agent:mcp") {
		t.Fatalf("mcp call did not run as MCPCaller: %d %s", resp.StatusCode, b)
	}
}
