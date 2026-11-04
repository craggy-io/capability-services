package tools_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/tools"
)

type getIn struct {
	ID string `json:"id" jsonschema:"artifact id"`
}

type artifact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// One turn of an in-process agent loop: list the tools the view allows,
// then dispatch the model's tool calls. The view is the agent's whole world.
func Example() {
	r := registry.New()
	defer r.Close()
	get := registry.Func("storage.artifact.get", "Get an artifact",
		func(ctx context.Context, in getIn) (artifact, error) {
			return artifact{ID: in.ID, Name: "app.apk"}, nil
		}, registry.WithEffects(registry.ReadOnly), registry.WithTags("agent"))
	legacy := registry.Func("jadx.decompile", "Decompile (legacy)",
		func(ctx context.Context, in getIn) (string, error) { return "jadx output", nil },
		registry.WithTags("agent"))
	modern := registry.Func("apk.decompile", "Decompile",
		func(ctx context.Context, in getIn) (string, error) { return "sources", nil },
		registry.WithSupersedes("jadx.decompile"), registry.AsLong(0), registry.WithTags("agent"))
	wipe := registry.Func("admin.wipe", "Wipe", func(ctx context.Context, _ struct{}) (string, error) { return "", nil },
		registry.WithTags("admin"))
	if err := r.Add(append(jobs.Capabilities(), get, legacy, modern, wipe)...); err != nil {
		panic(err)
	}
	agents := r.View(registry.And(
		registry.Or(registry.Tagged("agent"), registry.Match("jobs.*")),
		registry.Not(registry.Tagged("admin"))))
	ctx := registry.WithCaller(context.Background(), registry.Caller{ID: "agent:triage", Kind: "agent", Session: "conv-1"})

	for _, d := range tools.Defs(agents) { // re-list every turn
		fmt.Println(d.Name)
	}

	res := tools.Do(ctx, agents, "storage_artifact_get", json.RawMessage(`{"id":"art-1"}`))
	fmt.Println(res.Text, res.IsError, string(res.Structured))

	res = tools.Do(ctx, agents, "admin_wipe", nil) // outside the view
	fmt.Println(res.Text)
	fmt.Println(res.Code, res.Reason)

	text, isErr := tools.Call(ctx, agents, "jadx_decompile", json.RawMessage(`{"id":"art-1"}`))
	fmt.Println(text, isErr)

	res = tools.Do(ctx, agents, "apk_decompile", json.RawMessage(`{"id":"art-1"}`))
	fmt.Println("long call returns a job handle:", res.Job != "")
	// Output:
	// apk_decompile
	// jobs_cancel
	// jobs_get
	// jobs_list
	// jobs_wait
	// storage_artifact_get
	// {"id":"art-1","name":"app.apk"} false {"id":"art-1","name":"app.apk"}
	// {"error":{"code":"not_found","reason":"unknown_tool","message":"no tool \"admin_wipe\" in this view; call only the tools you were given","retryable":false}}
	// not_found unknown_tool
	// "jadx output"
	// note: jadx.decompile is superseded by apk.decompile false
	// long call returns a job handle: true
}
