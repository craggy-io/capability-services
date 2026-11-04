package mcp

import (
	"encoding/json"
	"testing"

	"github.com/craggy-io/capability-services/tools"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Notes ride in their own text block after the payload, so the first
// block stays the exact serialized output the structured content holds.
func TestResultNotesFollowThePayload(t *testing.T) {
	res := resultOf(tools.Result{
		Text:       `{"text":"hi"}`,
		Structured: json.RawMessage(`{"text":"hi"}`),
		Notes:      []string{"x is superseded by y", "took 12.3s"},
	})
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("result = %+v, want payload and notes blocks", res)
	}
	payload := res.Content[0].(*mcpsdk.TextContent).Text
	notes := res.Content[1].(*mcpsdk.TextContent).Text
	if payload != `{"text":"hi"}` || notes != "note: x is superseded by y\nnote: took 12.3s" {
		t.Fatalf("payload %q notes %q", payload, notes)
	}
	if res := resultOf(tools.Result{Text: `{}`}); len(res.Content) != 1 {
		t.Fatalf("a result without notes has %d blocks", len(res.Content))
	}
}
