// Package conformance is the conformance suite: one fixed capability set
// run through every projection (Invoke, tools, HTTP, MCP, Starlark),
// requiring identical results, error codes, job behaviour and pagination.
// The set runs twice: as Go-registered capabilities (test.*) and as the
// same capabilities defined in Starlark and loaded with starlarkcap.Load
// (star.*, see ScriptFile). Every new projection has to pass it. It also
// exports the standard fake capability set for other packages' tests and
// examples.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"testing/fstest"
	"time"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/capability-services/jobs"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

type echoIn struct {
	Text string `json:"text"`
}
type addIn struct {
	A int `json:"a"`
	B int `json:"b"`
}
type addOut struct {
	Sum int `json:"sum"`
}
type listIn struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}
type slowIn struct {
	Label  string `json:"label"`
	WaitMS int    `json:"wait_ms,omitempty"`
}

// Items is the fixed data set behind test.items.list.
var Items = []string{"item-1", "item-2", "item-3", "item-4", "item-5", "item-6", "item-7"}

// Capabilities returns the standard conformance capability set (without the
// job capabilities; callers add jobs.Capabilities()).
func Capabilities() []registry.Capability {
	return []registry.Capability{
		registry.Func("test.echo", "Echo text",
			func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
			registry.Describe("Returns its input text."),
			registry.WithEffects(registry.ReadOnly|registry.Idempotent), registry.WithTags("agent")),
		registry.Func("test.add", "Add two integers",
			func(ctx context.Context, in addIn) (addOut, error) { return addOut{in.A + in.B}, nil },
			registry.WithEffects(registry.ReadOnly), registry.WithTags("agent")),
		registry.Func("test.fail", "Always fails with conflict",
			func(ctx context.Context, in echoIn) (echoIn, error) {
				return echoIn{}, registry.Errorf(registry.Conflict, "boom")
			}, registry.WithTags("agent")),
		registry.Func("test.deny", "Always fails with denied",
			func(ctx context.Context, in echoIn) (echoIn, error) {
				return echoIn{}, registry.Errorf(registry.Denied, "not for you")
			}, registry.WithTags("agent")),
		registry.Func("test.full", "Always fails unavailable with a reason",
			func(ctx context.Context, in echoIn) (echoIn, error) {
				return echoIn{}, registry.WrapReason(registry.Unavailable, registry.ReasonNoCapacity, errors.New("pool full"))
			}, registry.WithTags("agent")),
		registry.Func("test.admin", "Admin-only echo",
			func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
			registry.WithTags("admin")),
		registry.Func("test.old", "Superseded echo",
			func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
			registry.WithTags("agent")),
		registry.Func("test.new", "Echo that supersedes test.old",
			func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
			registry.WithSupersedes("test.old"), registry.WithTags("agent")),
		registry.Func("test.ver", "Versioned echo v1",
			func(ctx context.Context, in echoIn) (echoIn, error) { return echoIn{Text: "v1:" + in.Text}, nil },
			registry.WithVersion(1), registry.WithTags("agent")),
		registry.Func("test.ver", "Versioned echo v2",
			func(ctx context.Context, in echoIn) (echoIn, error) { return echoIn{Text: "v2:" + in.Text}, nil },
			registry.WithVersion(2), registry.WithTags("agent")),
		registry.Func("test.items.list", "List items (paginated)",
			func(ctx context.Context, in listIn) (registry.Page[string], error) {
				return registry.Paginate(Items, registry.PageIn{Limit: in.Limit, Cursor: in.Cursor})
			}, registry.WithEffects(registry.ReadOnly), registry.WithTags("agent")),
		registry.Func("test.slow", "Slow fake job with staged progress",
			func(ctx context.Context, in slowIn) (echoIn, error) {
				registry.Progress(ctx, registry.Update{Stage: "push", Done: 100, Total: 100, Unit: "bytes"})
				wait := in.WaitMS
				if wait <= 0 {
					wait = 30
				}
				select {
				case <-time.After(time.Duration(wait) * time.Millisecond):
				case <-ctx.Done():
					return echoIn{}, ctx.Err()
				}
				registry.Progress(ctx, registry.Update{Stage: "run", Done: 1, Total: 1, Unit: "files", Data: map[string]any{"tool": "fake"}})
				registry.Progress(ctx, registry.Update{Stage: "finalise"})
				return echoIn{Text: "done-" + in.Label}, nil
			}, registry.AsLong(time.Second), registry.WithTags("agent")),
		registry.Func("test.unready", "Never ready",
			func(ctx context.Context, in echoIn) (echoIn, error) { return in, nil },
			registry.WithCheck(func(ctx context.Context) error { return errors.New("runtime down") })),
	}
}

// NewRegistry builds a registry with the standard set plus job capabilities.
func NewRegistry() (*registry.Registry, error) {
	r := registry.New()
	caps := append(Capabilities(), jobs.Capabilities()...)
	if err := r.Add(caps...); err != nil {
		r.Close()
		return nil, fmt.Errorf("conformance: %w", err)
	}
	return r, nil
}

// ScriptFile mirrors Capabilities in Starlark under the "star" prefix:
// star.echo, star.add, star.fail, star.deny, star.full, star.admin,
// star.old/star.new, star.ver, star.items.list, star.slow and
// star.unready take the same inputs and return the same results as their
// test.* counterparts (several by calling them through ctx.call or
// ctx.start). A script's own failure is internal, so star.fail fails
// internal where test.fail fails conflict; every projection must still
// agree.
const ScriptFile = `
def _echo(ctx, input):
    return {"text": input["text"]}

capability(
    name = "star.echo",
    title = "Echo text (script)",
    description = "Returns its input text.",
    input = {"text": schema.string(required = True)},
    output = {"text": schema.string(required = True)},
    effects = ["read_only", "idempotent"],
    tags = ["agent"],
    run = _echo,
)

capability(
    name = "star.add",
    title = "Add two integers (script)",
    input = {"a": schema.integer(required = True), "b": schema.integer(required = True)},
    output = {"sum": schema.integer(required = True)},
    effects = ["read_only"],
    tags = ["agent"],
    run = lambda ctx, input: {"sum": input["a"] + input["b"]},
)

capability(
    name = "star.fail",
    title = "Always fails (script)",
    input = {"text": schema.string(required = True)},
    tags = ["agent"],
    run = lambda ctx, input: ctx.call("test.fail", text = input["text"]),
)

capability(
    name = "star.deny",
    title = "Always fails with denied (script)",
    input = {"text": schema.string(required = True)},
    tags = ["agent"],
    run = lambda ctx, input: ctx.call("test.deny", text = input["text"]),
)

capability(
    name = "star.full",
    title = "Always fails unavailable with a reason (script)",
    input = {"text": schema.string(required = True)},
    tags = ["agent"],
    run = lambda ctx, input: ctx.call("test.full", text = input["text"]),
)

capability(
    name = "star.admin",
    title = "Admin-only echo (script)",
    input = {"text": schema.string(required = True)},
    tags = ["admin"],
    run = _echo,
)

capability(
    name = "star.old",
    title = "Superseded echo (script)",
    input = {"text": schema.string(required = True)},
    tags = ["agent"],
    run = _echo,
)

capability(
    name = "star.new",
    title = "Echo that supersedes star.old (script)",
    input = {"text": schema.string(required = True)},
    supersedes = ["star.old"],
    tags = ["agent"],
    run = _echo,
)

capability(
    name = "star.ver",
    version = 1,
    title = "Versioned echo v1 (script)",
    input = {"text": schema.string(required = True)},
    tags = ["agent"],
    run = lambda ctx, input: ctx.call("test.ver@1", text = input["text"]),
)

capability(
    name = "star.ver",
    version = 2,
    title = "Versioned echo v2 (script)",
    input = {"text": schema.string(required = True)},
    tags = ["agent"],
    run = lambda ctx, input: ctx.call("test.ver@2", text = input["text"]),
)

capability(
    name = "star.items.list",
    title = "List items, paginated (script)",
    input = {"limit": schema.integer(), "cursor": schema.string()},
    effects = ["read_only"],
    tags = ["agent"],
    run = lambda ctx, input: ctx.call("test.items.list", **input),
)

def _slow(ctx, input):
    ctx.progress(stage = "delegate", done = 0, total = 1, unit = "jobs")
    out = ctx.start("test.slow", **input).wait()["result"]
    ctx.progress(stage = "delegate", done = 1, total = 1, unit = "jobs")
    return {"text": out["text"]}

capability(
    name = "star.slow",
    title = "Slow fake job (script)",
    input = {"label": schema.string(required = True), "wait_ms": schema.integer()},
    long = True,
    heartbeat = 1,
    tags = ["agent"],
    run = _slow,
)

capability(
    name = "star.unready",
    title = "Never ready (script)",
    input = {"text": schema.string(required = True)},
    run = _echo,
    check = lambda ctx: "runtime down",
)
`

// ScriptManifest is the bundle.yaml LoadScript loads ScriptFile with: the
// bundle conformance.script declares conformance.star as the capability
// file for the prefix "star", so installed capabilities have Origin.Ref
// "conformance.script#star".
const ScriptManifest = `apiVersion: craggy.io/bundle/v1
metadata: {id: conformance.script, version: 1.0.0}
contributions:
  - {id: star, kind: capability.script@1, path: conformance.star}
`

// LoadScript installs ScriptFile in r under the "star" prefix, loading it
// with starlarkcap.Load from an in-memory bundle of ScriptManifest and
// ScriptFile.
func LoadScript(ctx context.Context, r *registry.Registry) (starlarkcap.Loaded, error) {
	fsys := fstest.MapFS{
		"bundle.yaml":      {Data: []byte(ScriptManifest)},
		"conformance.star": {Data: []byte(ScriptFile)},
	}
	loaded, err := starlarkcap.Load(ctx, r, []bundle.Source{bundle.Embedded("conformance", fsys)}, starlarkcap.LoadOptions{})
	if err != nil {
		return starlarkcap.Loaded{}, err
	}
	return loaded[0], nil
}
