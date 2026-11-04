package mcp_test

// Regression tests for the 2026-11-04 review findings in the MCP
// projection.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	regmcp "github.com/craggy-io/capability-services/mcp"
	"github.com/craggy-io/capability-services/registry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Serve must not panic for capabilities the registry accepts whose input
// schema is not an object schema (RawMessage / scalar Func inputs), and
// adding one at runtime must not crash the host from the Watch goroutine.
func TestReviewServeNonObjectInputNoPanic(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	raw := registry.Func("raw.echo", "raw",
		func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) { return in, nil })
	if err := r.Add(raw); err != nil {
		t.Fatal(err)
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	stop := regmcp.Serve(server, r.View(registry.All))
	defer stop()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "client", Version: "1"}, nil)
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	// Runtime-add a scalar-input capability after Serve: the sync
	// goroutine must survive it.
	str := registry.Func("str.echo", "str",
		func(ctx context.Context, s string) (string, error) { return s, nil })
	if err := r.Add(str); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		res, err := cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, tool := range res.Tools {
			names[tool.Name] = true
		}
		if names["raw_echo"] && names["str_echo"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tools after runtime add = %v", names)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A string-output capability advertises an outputSchema, so its result
// must carry structuredContent (strict clients reject it otherwise).
func TestReviewMCPStructuredStringOutput(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	c := registry.Func("str.out", "str",
		func(ctx context.Context, in echoIn) (string, error) { return "hello " + in.Text, nil })
	if err := r.Add(c); err != nil {
		t.Fatal(err)
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	stop := regmcp.Serve(server, r.View(registry.All))
	defer stop()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "client", Version: "1"}, nil)
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "str_out", Arguments: map[string]any{"text": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("call errored: %+v", res.Content)
	}
	if res.StructuredContent == nil || !strings.Contains(string(asJSON(t, res.StructuredContent)), "hello x") {
		t.Fatalf("structuredContent = %v, want the string output", res.StructuredContent)
	}
}

func asJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
