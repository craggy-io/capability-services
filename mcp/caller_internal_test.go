package mcp

import (
	"testing"

	"github.com/craggy-io/capability-services/registry"
)

func TestActingCallerWrapsAUserAndKeepsANamedAgent(t *testing.T) {
	session := registry.Caller{ID: "agent:mcp", Kind: "agent", Session: "s1"}
	user := registry.Caller{ID: "user:u1", Kind: "user"}
	got := actingCaller(session, user)
	if got.ID != "agent:mcp" || got.Parent == nil || got.Parent.ID != "user:u1" {
		t.Fatalf("user request: %+v", got)
	}
	named := registry.Caller{ID: "agent:mcp:c1", Kind: "agent", Parent: &user}
	if got := actingCaller(session, named); got.ID != "agent:mcp:c1" || got.Parent.ID != "user:u1" {
		t.Fatalf("named agent: %+v", got)
	}
	// An agent with no one to act for is not trusted as named: it is wrapped like any caller.
	lone := registry.Caller{ID: "agent:x", Kind: "agent"}
	if got := actingCaller(session, lone); got.ID != "agent:mcp" || got.Parent.ID != "agent:x" {
		t.Fatalf("lone agent: %+v", got)
	}
}
