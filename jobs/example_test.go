package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
)

// The job controls are ordinary capabilities. jobs.wait reports a job's
// status and, once it succeeded, its result; the default access rule lets a
// caller see only jobs from its own session (or sessions it started).
func Example() {
	r := registry.New()
	defer r.Close()
	build := registry.Func("demo.build", "Build something",
		func(ctx context.Context, _ struct{}) (map[string]string, error) {
			registry.Progress(ctx, registry.Update{Stage: "compile", Done: 2, Total: 2, Unit: "files"})
			return map[string]string{"artifact": "out.bin"}, nil
		}, registry.AsLong(0))
	if err := r.Add(append(jobs.Capabilities(), build)...); err != nil {
		panic(err)
	}
	if err := r.AddGroup(jobs.Group()); err != nil {
		panic(err)
	}

	alice := registry.WithCaller(context.Background(), registry.Caller{ID: "agent:a", Kind: "agent", Session: "s-alice"})
	job, err := r.Start(alice, "demo.build", nil)
	if err != nil {
		panic(err)
	}
	in := json.RawMessage(fmt.Sprintf(`{"id":%q,"timeout":5}`, job.ID()))
	raw, err := r.Invoke(alice, "jobs.wait", in)
	if err != nil {
		panic(err)
	}
	var w jobs.WaitOut
	if err := json.Unmarshal(raw, &w); err != nil {
		panic(err)
	}
	fmt.Println(w.Status.State, w.Status.Last.Stage, string(w.Result))

	bob := registry.WithCaller(context.Background(), registry.Caller{ID: "agent:b", Kind: "agent", Session: "s-bob"})
	_, err = r.Invoke(bob, "jobs.get", json.RawMessage(fmt.Sprintf(`{"id":%q}`, job.ID())))
	fmt.Println("other session:", registry.CodeOf(err))

	g := r.Groups()[0]
	fmt.Println(g.Name, g.LoadAlways)
	// Output:
	// succeeded compile {"artifact":"out.bin"}
	// other session: not_found
	// jobs true
}
