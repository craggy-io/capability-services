package mcp_test

import (
	"context"
	"fmt"

	regmcp "github.com/craggy-io/capability-services/mcp"
	"github.com/craggy-io/capability-services/registry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Serve projects a view onto an MCP server and keeps it in sync. Effects
// become tool annotations, the output schema becomes the tool's
// outputSchema, and a successful call carries the output both as the
// first text block and as structured content.
func ExampleServe() {
	r := registry.New()
	defer r.Close()
	get := registry.Func("storage.artifact.get", "Get an artifact",
		func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
		registry.WithEffects(registry.ReadOnly|registry.Idempotent))
	if err := r.Add(get); err != nil {
		panic(err)
	}

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "example", Version: "1"}, nil)
	stop := regmcp.Serve(server, r.View(registry.All),
		regmcp.OnClose(func(c registry.Caller) { /* end the host's session for c */ }))
	defer stop()

	ctx := context.Background()
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		panic(err)
	}
	defer ss.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "client", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		panic(err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		panic(err)
	}
	for _, t := range list.Tools {
		fmt.Println(t.Name, "readOnly:", t.Annotations.ReadOnlyHint, "outputSchema:", t.OutputSchema != nil)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "storage_artifact_get", Arguments: map[string]any{"text": "art-1"}})
	if err != nil {
		panic(err)
	}
	fmt.Println(res.IsError, res.Content[0].(*mcpsdk.TextContent).Text)
	// Output:
	// storage_artifact_get readOnly: true outputSchema: true
	// false {"text":"art-1"}
}
