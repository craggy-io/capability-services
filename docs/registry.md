# Registry

[Project](../README.md) · [Documentation index](README.md) · [Design](design.md)

Package `registry`. Available Go API; standard library plus
`github.com/google/jsonschema-go` only.

## Purpose

Hold one registration shape for a capability and everything projections need to
translate it mechanically: names, versions, schemas, effects, supersession, views,
readiness, jobs and coded errors.

## Features

- Register typed Go functions with derived JSON Schemas (`Func`), or hand-built
  `Capability` values with parsed schemas (`ParseSchema`).
- Change the set at runtime with `Add`, `Replace(prefix, ...)` and `Remove`;
  every change is an atomic snapshot swap that running calls never observe.
- Derive versions and supersession from what is registered; nothing is stored
  separately.
- Filter the registry into live views; a view is both what a projection lists and
  what it may call.
- Validate input once, before the handler, with a message an agent can act on.
- Run every call as a job with stage progress, heartbeat, cancellation and child
  jobs (see [jobs](jobs.md)).
- Report readiness from optional checks; group capability families; project every
  entry as a serializable `Descriptor` with its `Origin`.

## Mental model

A `Registry` holds an atomic pointer to an immutable snapshot of capabilities,
keyed by `name@version`, plus the job table and readiness state. `Add`, `Replace`
and `Remove` build a new snapshot, validate it as a whole (names, schemas,
examples, supersession cycles) and swap it in, or change nothing. A call resolves
its capability once, at `Start`, and keeps that copy even if the snapshot changes
under it.

A `View` is the registry seen through a `Filter`. It holds no copy: every
`List`, `Get` or `Invoke` reads the current snapshot. The registry itself acts as
`View(All)`.

## Responsibilities and boundaries

The registry validates, routes, tracks and classifies. It does not authenticate,
store sessions, persist outputs, enforce physical resource quotas or know any
domain. A `Caller` is carried, never interpreted (except by the default job-access
rule). `Subject` and `Origin` are stored and projected, never interpreted. Those
meanings belong to the host (see [design section 5](design.md#5-callers-sessions-and-middleware)).

## Public interfaces

Registration:

- `Func[In, Out](name, title, fn, opts...) Capability`. Options: `Describe`,
  `WithVersion`, `WithEffects`, `WithTags`, `WithSupersedes`, `WithAccepts`,
  `WithEmits`, `WithExample(note, input)`, `AsLong(heartbeat)`, `WithCheck`,
  `WithInput`/`WithOutput`, `WithOrigin`. `In`/`Out` may be `json.RawMessage`
  (passed through) or `any` (unstructured); `struct{}` input is the empty object
  schema.
- `SchemaFor[T]()`, `ParseSchema(raw)`, `EmptyObjectSchema()`.
- `New(opts...) *Registry`. Options: `Use`, `Limit`, `CheckEvery`, `CheckTimeout`,
  `KeepJobs`, `WithJobAccess`, `ValidateOutput`, `JobDrainTimeout`.
- `(*Registry).Add`, `Replace`, `Remove`, `AddGroup`, `RefreshChecks`,
  `OpenJobJournal`, `JobJournalError`, `Close`.

Views and listings:

- `(*Registry).View(f) *View`; filters `All`, `Match(globs...)`, `Tagged(tags...)`,
  `ReadOnlyOnly`, `And`, `Or`, `Not`; `Select(caps, f)` before `Add`.
- `(*View).List`, `All`, `Get(ref)`, `Descriptors(all)`, `Groups`, `Allows`,
  `Intersect`, `Registry`, `Watch(ctx)`. The `Registry` has the same listing,
  invoke and job methods, acting as `View(All)`.
- `Entry` (a `Capability` plus `Current`, `SupersededBy`, `Group`, `Ready`,
  `CheckError`), `Entry.Superseded`, `Entry.Descriptor`.
- Names: `ValidName`, `ParseRef`, `ToolName`, `FromToolName`, `MaxToolNameLen`,
  `Capability.Ref`.

Calls and context:

- `(*View).Invoke(ctx, ref, in)`, `Start(ctx, ref, in)`; jobs in [jobs](jobs.md).
- `WithCaller`/`CallerFrom`, `WithView`/`ViewFrom`, `JobIDFrom`, `Progress`.
- `Middleware func(next Handler, c Capability) Handler`.

Errors: `Code` and its constants, `Error`, `Errorf`, `ReasonErrorf`, `WrapCode`,
`WrapReason`, `CodeOf`, `ReasonOf`, `Info`, `ErrorInfo`, `ErrorEnvelope`, `Codes`,
`ValidReason`, `Code.Retryable`, `ReasonNoCapacity`, `ReasonBackendDown`,
`ReasonNotReady`.

Pagination: `PageIn`, `Page[T]`, `Paginate`, `NormalizeLimit`,
`DefaultPageLimit` (50), `MaxPageLimit` (500).

Projection helpers: `DescriptionNotes`, `SubjectNote`, `ExampleNote`.

## Versions and supersession

- The highest registered version of a name is current. A bare ref resolves to it;
  `name@N` pins. `List` shows only current, non-superseded entries; `All` and
  `Get` reach everything.
- `Supersedes` names refs this capability replaces: a bare name covers every
  version, `name@N` one version. Superseded entries leave `List` but still answer
  when called by name, with notices in every projection
  ([projections](projections.md)). `Entry.SupersededBy` names the superseders (the
  bare name for a current superseder, else `name@version`).
- A rule may name something not yet registered; it applies when that appears.
  Cycles are rejected by `Add`/`Replace` with `invalid`. Removing the superseder
  (for example unloading its prefix) restores the originals at once.
- Retire a version with `Remove("name@1")`; remove every version with
  `Remove("name")`.

## Views as policy

- One registry, one view per audience: `r.View(registry.Not(registry.Tagged("admin")))`
  for agents, `r.View(registry.All)` for an operator API. Nothing is registered
  twice to restrict it.
- A ref outside the view is `not_found` in `Get`, `Invoke`, `Start`, `Job` and
  every projection.
- Nested calls are clamped. The registry puts the effective view on every handler
  context (`ViewFrom`). `Start` intersects its receiver with `ViewFrom(ctx)`, so a
  handler that calls `r.Invoke` (the whole registry) from inside an agent's call
  still sees only the agent's view.
- `Match` uses `path.Match` globs on the dotted name; `*` crosses dots because dots
  are not path separators. An invalid glob matches nothing.

## Working with other packages

Every other package in this module imports `registry`; it imports none of them.
[jobs](jobs.md) adds the `jobs.*` capabilities over the job table.
[tools, mcp and httpapi](projections.md) consume views and `Descriptor`s and map
`Code`s to their transports. [starlarkcap](starlark.md) builds capabilities from
scripts with parsed schemas and installs them with `Replace`, setting `Origin`.
Domain adapters return `[]Capability` and declare a `Codes` table for their
sentinel errors.

## Examples

The examples in [example_test.go](../registry/example_test.go) are compiled and
their output checked by `go test`:

- `Example`: `Func`, `Add`, `Invoke`, and the `invalid` message for a bad call.
- `Example_versions`: current vs pinned versions, retiring one.
- `Example_supersession`: supersede, still callable, unload restores.
- `Example_views`: a name outside the view, and a nested call clamped to the
  caller's view.
- `ExampleView_Start`: a `Long` job's stage progress and event replay.
- `ExampleRegistry_Close`: `Close` cancels and drains a running job.
- `ExampleReasonErrorf`, `ExampleCodes`: codes, reasons and the envelope.
- `ExamplePaginate`, `ExampleRegistry_AddGroup`.

```go
r := registry.New()
defer r.Close()
err := r.Add(registry.Func("demo.greet", "Greet someone",
	func(ctx context.Context, in greetIn) (greetOut, error) {
		return greetOut{Greeting: "hello " + in.Name}, nil
	},
	registry.WithEffects(registry.ReadOnly|registry.Idempotent),
	registry.WithTags("agent"),
	registry.WithExample("the usual call", greetIn{Name: "ada"})))

out, err := r.Invoke(ctx, "demo.greet", json.RawMessage(`{"name":"ada"}`))
// {"greeting":"hello ada"}

_, err = r.Invoke(ctx, "demo.greet", json.RawMessage(`{"nmae":"ada"}`))
// invalid:
// demo.greet@1: invalid input:
// - name: missing required field
// - nmae: unknown field; accepted fields: name
// Required fields: name.
// Example input: {"name":"ada"}
```

## Semantics, errors and limits

- **Names.** Dotted segments of `[a-z0-9-]`, no empty segment, at most 64
  characters (the tool-name limit). `_` never appears, so `ToolName` is reversible.
- **`Add`** registers nothing and returns `conflict` on a duplicate
  `name@version` (within the call or against the registry) and `invalid` on a bad
  name, negative version, nil handler, malformed supersedes ref, unresolvable
  schema, example without an input schema, example failing its schema, empty
  subject type, or supersession cycle.
- **`Replace(prefix, caps...)`** removes every capability named `prefix` or under
  `prefix.` and installs `caps`, each of which must be under the prefix
  (`invalid` otherwise). `Replace(prefix)` unloads. A failed validation changes
  nothing. Groups survive both.
- **`Remove`** ignores unknown or malformed refs and never fails.
- **`Func` panics** on a nil function or a type whose schema cannot be derived
  (recursive types); supply `WithInput`/`WithOutput` for those. `WithExample`
  panics if its input cannot be marshalled.
- **Input.** A capability with no input schema accepts only an empty body, `null`
  or `{}`. Otherwise an empty body is `{}` and the JSON must validate. The
  `invalid` message lists at most 8 problems and is capped at 1,500 bytes; it ends
  with the required top-level fields and the first example.
- **Output.** A handler's result is JSON-encoded unless it is already a
  `json.RawMessage` (or a `[]byte` holding valid JSON). Encoding failure, invalid
  JSON or a panic in the handler fails the job with `internal`. Output is validated
  against `Output` only with `ValidateOutput(true)` or the `craggy_debug` build tag;
  a mismatch is `internal`.
- **Readiness.** Checks run at `Add`/`Replace` for the new capabilities (before
  they are visible), every `CheckEvery` (default 30s; 0 disables), and on
  `RefreshChecks`, each bounded by `CheckTimeout` (default 10s), with panics
  recovered as failures. A failing capability stays in `List` with `Ready: false`
  and fails `Invoke`/`Start` with `unavailable`/`not_ready`.
- **Watch** fires, coalesced, after every snapshot change, group change and
  readiness change; receivers re-list and diff. The channel closes when its
  context ends.
- **Codes.** `CodeOf` maps context cancellation and deadlines to `cancelled` and
  anything uncoded to `internal`. `Codes.Wrap` leaves nil, already-coded and
  unmatched errors unchanged. An error matching several sentinels (a wrapped
  chain or an `errors.Join`) gets their code when they agree; when they disagree
  the table overlaps, and `Wrap` returns `internal` naming the clashing codes, never
  an arbitrary pick. Keep a table's sentinels disjoint.
- **Close.** `Close` cancels unfinished jobs and drains their runners (see
  [jobs](jobs.md)). After it starts, `Start` and `Invoke` fail with `unavailable`
  and reason `closed`; listings keep working.
- **Groups.** `AddGroup` rejects invalid names and empty or multi-line
  descriptions (`invalid`) and a name re-registered with different fields
  (`conflict`). Membership is the longest registered group name that equals or is
  a dotted prefix of the capability name.
- **Pagination.** `Paginate` takes items in a stable order; cursors are opaque
  offsets. A malformed cursor, or one past the end, is `invalid`. Limits clamp to
  `[1, 500]`, with 0 or less meaning 50.

## Validation and evidence

`go test ./registry` covers registration and validation (`TestAddValidation`,
`TestFuncInvokeAndSchema`, `TestSchemaForRawMessageAcceptsAnyJSON`), versions,
supersession and cycles, `Replace` during in-flight calls and under concurrency,
`Watch` coalescing, filters and views, nested-call clamping, middleware order and
callers, panic recovery and output validation, the invalid-input explainer and its
bounds, groups, descriptors and subjects, reasons through wrapping and jobs, and
the examples above. Run it with `-tags craggy_debug` to validate every output
against its schema.
