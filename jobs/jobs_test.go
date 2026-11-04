package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
)

func mustAdd(t *testing.T, r *registry.Registry, caps ...registry.Capability) {
	t.Helper()
	if err := r.Add(caps...); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

// fakeContainer mimics a long decompile: multi-stage progress with unit
// changes, an optional stall window, publish step at the end.
type fakeIn struct {
	StallMS int  `json:"stall_ms,omitempty"`
	FailPub bool `json:"fail_publish,omitempty"`
	WaitMS  int  `json:"wait_ms,omitempty"`
}

func fakeContainer() registry.Capability {
	return registry.Func("fake.decompile", "Fake long decompile",
		func(ctx context.Context, in fakeIn) (map[string]string, error) {
			registry.Progress(ctx, registry.Update{Stage: "push", Done: 50, Total: 100, Unit: "bytes"})
			registry.Progress(ctx, registry.Update{Stage: "push", Done: 100, Total: 100, Unit: "bytes"})
			if in.StallMS > 0 {
				select {
				case <-time.After(time.Duration(in.StallMS) * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			registry.Progress(ctx, registry.Update{Stage: "run", Done: 1, Total: 3, Unit: "files", Message: "decompiling"})
			if in.WaitMS > 0 {
				select {
				case <-time.After(time.Duration(in.WaitMS) * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			registry.Progress(ctx, registry.Update{Stage: "publish-output", Done: 0, Total: 0})
			if in.FailPub {
				return nil, registry.Errorf(registry.Internal, "publication failed")
			}
			registry.Progress(ctx, registry.Update{Stage: "finalise"})
			return map[string]string{"done": "yes"}, nil
		}, registry.AsLong(80*time.Millisecond), registry.WithTags("agent"))
}

func TestCapabilitiesAndAccess(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, append([]registry.Capability{fakeContainer()}, jobs.Capabilities()...)...)

	user := registry.Caller{ID: "user:phil", Kind: "user", Session: "sess-user"}
	agent := registry.Caller{ID: "agent:triage-7", Kind: "agent", Session: "sess-agent", Parent: &user}
	stranger := registry.Caller{ID: "user:eve", Kind: "user", Session: "sess-eve"}

	actx := registry.WithCaller(context.Background(), agent)
	job, err := r.Start(actx, "fake.decompile", json.RawMessage(`{"wait_ms":50}`))
	if err != nil {
		t.Fatal(err)
	}
	// jobs.wait through the agent's own session returns status+result.
	waitIn := json.RawMessage(`{"id":"` + job.ID() + `","timeout":5}`)
	out, err := r.Invoke(actx, "jobs.wait", waitIn)
	if err != nil {
		t.Fatal(err)
	}
	var wo jobs.WaitOut
	if err := json.Unmarshal(out, &wo); err != nil {
		t.Fatal(err)
	}
	if wo.Status.State != registry.JobSucceeded || string(wo.Result) != `{"done":"yes"}` {
		t.Fatalf("jobs.wait=%s", out)
	}
	// The parent (user who launched the agent) can also see it.
	uctx := registry.WithCaller(context.Background(), user)
	if _, err := r.Invoke(uctx, "jobs.get", json.RawMessage(`{"id":"`+job.ID()+`"}`)); err != nil {
		t.Fatalf("parent access: %v", err)
	}
	// A stranger cannot: not_found, and list hides it.
	sctx := registry.WithCaller(context.Background(), stranger)
	_, err = r.Invoke(sctx, "jobs.get", json.RawMessage(`{"id":"`+job.ID()+`"}`))
	if registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("stranger jobs.get: %v", err)
	}
	lout, _ := r.Invoke(sctx, "jobs.list", json.RawMessage(`{}`))
	var page registry.Page[registry.JobStatus]
	_ = json.Unmarshal(lout, &page)
	for _, st := range page.Items {
		if st.ID == job.ID() {
			t.Fatal("stranger saw the job in jobs.list")
		}
	}
	// A view that excludes the cap hides its jobs even from their owner view-wise.
	noFake := r.View(registry.Not(registry.Match("fake.*")))
	if _, ok := noFake.Job(job.ID()); ok {
		t.Fatal("view leaked job of excluded capability")
	}
	// jobs.cancel via capability works and is idempotent.
	job2, _ := r.Start(actx, "fake.decompile", json.RawMessage(`{"wait_ms":500}`))
	if _, err := r.Invoke(actx, "jobs.cancel", json.RawMessage(`{"id":"`+job2.ID()+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := job2.Wait(context.Background()); registry.CodeOf(err) != registry.Cancelled {
		t.Fatalf("jobs.cancel didn't cancel: %v", err)
	}
	if _, err := r.Invoke(actx, "jobs.cancel", json.RawMessage(`{"id":"`+job2.ID()+`"}`)); err != nil {
		t.Fatalf("second cancel (idempotent): %v", err)
	}
	// Unknown job ref.
	_, err = r.Invoke(actx, "jobs.get", json.RawMessage(`{"id":"job-999999"}`))
	if registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("unknown job: %v", err)
	}
}

func TestCustomJobAccess(t *testing.T) {
	r := registry.New(registry.WithJobAccess(func(c registry.Caller, st registry.JobStatus) bool {
		return c.Attrs["admin"] == "true"
	}))
	defer r.Close()
	mustAdd(t, r, append([]registry.Capability{fakeContainer()}, jobs.Capabilities()...)...)
	job, _ := r.Start(context.Background(), "fake.decompile", json.RawMessage(`{"wait_ms":20}`))
	_, _ = job.Wait(context.Background())
	admin := registry.WithCaller(context.Background(), registry.Caller{Session: "x", Attrs: map[string]string{"admin": "true"}})
	if _, err := r.Invoke(admin, "jobs.get", json.RawMessage(`{"id":"`+job.ID()+`"}`)); err != nil {
		t.Fatalf("custom access (admin): %v", err)
	}
	nonAdmin := registry.WithCaller(context.Background(), registry.Caller{Session: "x"})
	if _, err := r.Invoke(nonAdmin, "jobs.get", json.RawMessage(`{"id":"`+job.ID()+`"}`)); registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("custom access (non-admin): %v", err)
	}
}

func TestGroup(t *testing.T) {
	if !strings.Contains(jobs.Group().Description, "jobs") || !jobs.Group().LoadAlways {
		t.Fatalf("Group = %+v", jobs.Group())
	}
}

// A failed job's error reason survives jobs.wait, as agents and scripts see it.
func TestWaitPreservesReason(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	full := func(ctx context.Context, in struct{}) (struct{}, error) {
		return in, registry.WrapReason(registry.Unavailable, registry.ReasonNoCapacity, errors.New("pool full"))
	}
	mustAdd(t, r, registry.Func("pool.long", "Long", full, registry.AsLong(time.Second)))
	mustAdd(t, r, jobs.Capabilities()...)
	v := r.View(registry.All)
	ctx := context.Background()

	job, err := v.Start(ctx, "pool.long", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(ctx); registry.ReasonOf(err) != registry.ReasonNoCapacity {
		t.Fatalf("Wait: %+v", registry.Info(err))
	}
	out, err := v.Invoke(ctx, "jobs.wait", json.RawMessage(`{"id":"`+job.ID()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var w jobs.WaitOut
	if err := json.Unmarshal(out, &w); err != nil || w.Status.Error == nil || w.Status.Error.Reason != registry.ReasonNoCapacity {
		t.Fatalf("jobs.wait = %s (%v)", out, err)
	}
}
