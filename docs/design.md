# Capability Services: design

[Project](../README.md) · [Documentation index](README.md) · [Agent guide](../AGENTS.md)

capability-services is a small Go library. You register each capability once, and
it is projected into every surface that needs it: MCP tools for agents, an HTTP
API, Starlark built-ins, in-process tool-calling loops, and later a CLI or anything
else. Capabilities can be added, replaced, versioned and superseded while the
program runs, and every projection follows those changes. Any capability can run
as a long-running job that reports its status, progress and liveness.

It has no knowledge of storage, classification, data, execution environments or
security tooling. Those libraries never import it; a small adapter package beside
each one does. Throughout this document, the **host** is the application that
imports this module: it opens its domain libraries, builds the registry, chooses
views, wires projections and owns authentication, sessions and audit.

This document describes the current implementation. [Section 8](#8-changes-since-the-first-design)
lists what changed since the design this module was first built from.

## 1. Rules

These rules keep the system from turning into a rat's nest.

1. **One shape at the boundary.** A capability is a name, a version, a
   description, a JSON Schema for its input and output, effect hints, and a handler
   that takes and returns JSON. Every projection is a mechanical translation of
   that shape. Nothing is registered twice.
2. **Domain libraries don't know about the registry.** Domain libraries keep their
   Go APIs. Each gets an optional `*cap` sub-package that turns its API into
   capabilities. The domain core never imports this module.
3. **The registry doesn't know about the domains or the transports.** The core
   `registry` package depends only on the standard library and a JSON Schema
   library. MCP, HTTP and Starlark live in their own packages.
4. **Adapters own domain surfaces; the host supplies cross-domain ports.** A
   capability that needs two domains (classify a stored artifact, run a tool and
   publish its output) belongs to the host, or to an adapter given an injected
   interface the host implements. No adapter imports another domain's
   implementation.
5. **Go calls Go.** In-process Go code calls the library API directly. The
   registry is only for crossing a boundary: to an agent, a script, a remote
   client, or a person.
6. **Validate once.** Input is checked against the schema in the registry, before
   the handler runs, whichever projection the call came from.
7. **Policy is a filter, not a fork.** Each projection is given a *view*, which is
   the registry filtered by name, tag or effect. Agents can get read-only tools
   while the HTTP API gets everything, without anything being registered twice. A
   call never gets wider access than the view it came in on, including calls a
   capability makes to other capabilities.
8. **Every call can be a job.** A long-running capability is a normal handler that
   reports progress. The registry, not each projection, provides job tracking,
   status, progress, liveness and cancellation.
9. **Projections say who is calling; the host says what that means.** A projection
   identifies the caller. The host maps the caller to its own session, tenancy or
   audit concepts, and the active job ID to execution ownership. Adapters only ask
   the host for what they need, through dependencies the host injects.

```
                 ┌──────────────────────── host application ─────────────────────────┐
                 │ opens domain libraries, builds the registry, picks views,         │
                 │ registers cross-domain capabilities, maps callers to sessions,     │
                 │ sets script policy, owns auth and audit                            │
                 └───────┬─────────────────────┬─────────────────────┬────────────────┘
                         │                     │                     │
          *cap adapters (one per domain)       │      tools  mcp  httpapi  starlarkcap  jobs
            │           │          │           │        │     │      │         │        │
            ▼           ▼          ▼           ▼        ▼     ▼      ▼         ▼        ▼
         domain A    domain B   domain C   ────────►      registry (core)       ◄────────
            (domain packages: no registry import)       (no domain, no transport)
```

Arrows point only downward and inward. The adapters and the projections both
depend on `registry`, and never on each other, except that `mcp` is built on
`tools`. `starlarkcap` additionally depends on the separate module
`github.com/craggy-io/script-services`, the generic Starlark runtime.

## 2. The core API

```go
package registry

// Capability is the single registration shape.
type Capability struct {
    Name        string             // dotted, lower-case: "storage.artifact.get"
    Version     int                // >= 1; 0 is treated as 1
    Title       string             // one line, for listings
    Description string             // what it does and when to use it (agents read this)
    Input       *jsonschema.Schema // object schema; nil means no input
    Output      *jsonschema.Schema // nil means unstructured
    Examples    []Example          // sample inputs agents read; each must validate against Input
    Effects     Effects
    Tags        []string           // "agent", "admin", "slow", ...
    Supersedes  []string           // refs this replaces: "jadx.decompile" (all versions) or "apktool.decode@1"
    Accepts     []Subject          // opaque content types it can run on; empty means it is not a content step
    Emits       []Subject          // opaque hint about content the output may be; not a result
    Long        bool               // projections start it as a job instead of waiting for it (2.6)
    Heartbeat   time.Duration      // a job with no Progress for this long shows as stalled; 0 = 30s
    Check       func(ctx context.Context) error // optional readiness check (2.7)
    Handler     Handler
    Origin      Origin             // where the definition came from; zero when the host did not say
}

// Subject is an opaque content type. The registry stores and projects it
// and does not interpret Type or Level.
type Subject struct {
    Type  string // e.g. "android.apk"
    Level string // e.g. an evidence level, "validated"; empty when unpinned
}

// Example is one sample input: what a schema cannot say. Add rejects an
// example that does not validate against Input. Projections append examples
// to the description agents read (DescriptionNotes), one compact JSON line each.
type Example struct {
    Note  string
    Input json.RawMessage
}

// Origin records where a definition came from (a script file and its content
// address, a plugin, a remote catalogue). Stored and projected, never interpreted.
type Origin struct {
    Kind   string // "script", "plugin", ...
    Ref    string // a file name, a package path, a URL
    Digest string // e.g. "sha256:<hex>"
}

type Handler func(ctx context.Context, in json.RawMessage) (any, error)

type Effects uint8

const (
    ReadOnly    Effects = 1 << iota // changes nothing
    Idempotent                      // repeat calls have no further effect
    Destructive                     // removes or overwrites something
    OpenWorld                       // touches the network or external systems
)

// Func builds a Capability from a typed Go function. The schemas are derived
// from In and Out (json tags, plus `jsonschema:"..."` descriptions).
func Func[In, Out any](name, title string, fn func(context.Context, In) (Out, error), opts ...Option) Capability
// Capability options: Describe, WithVersion, WithEffects, WithTags, WithSupersedes,
// WithAccepts, WithEmits, WithExample(note, input), AsLong(heartbeat), WithCheck,
// WithInput/WithOutput (override schemas), WithOrigin.

type Registry struct{ /* atomic pointer to an immutable snapshot, plus the job table */ }

func New(opts ...Option) *Registry
// Registry options: Use(middleware...), Limit(tag, n), CheckEvery(d), CheckTimeout(d),
// KeepJobs(d), WithJobAccess(func(Caller, JobStatus) bool), ValidateOutput(on),
// JobDrainTimeout(d).

func (r *Registry) Add(caps ...Capability) error                    // duplicate name@version, bad name/schema/example, supersession cycle
func (r *Registry) Replace(prefix string, caps ...Capability) error // atomically swaps everything under prefix
func (r *Registry) Remove(refs ...string)                           // "name" (all versions) or "name@2"
func (r *Registry) AddGroup(gs ...Group) error                      // 2.8
func (r *Registry) View(f Filter) *View                             // a live, filtered view; the Registry itself acts as View(All)
func (r *Registry) OpenJobJournal(path string) error                // 2.6
func (r *Registry) RefreshChecks(ctx context.Context)               // 2.7
func (r *Registry) Close()                                          // cancel jobs, wait up to JobDrainTimeout

// Entry is a capability plus its live state.
type Entry struct {
    Capability
    Current      bool     // the newest version of its name
    SupersededBy []string // refs of capabilities that supersede this one; empty if none
    Group        string   // the registered Group it belongs to; empty when none
    Ready        bool     // its last Check passed (true when there's no Check)
    CheckError   string   // message from the last failed Check
}
func (e Entry) Descriptor() Descriptor // the full serializable projection, plus live state

// View is what projections consume.
func (v *View) List() []Entry                // current, not superseded, sorted by name
func (v *View) All() []Entry                 // also old versions and superseded entries
func (v *View) Get(ref string) (Entry, bool) // "name" resolves to the current version; "name@1" pins
func (v *View) Descriptors(all bool) []Descriptor
func (v *View) Groups() []Group
func (v *View) Invoke(ctx context.Context, ref string, in json.RawMessage) (json.RawMessage, error) // Start + Wait
func (v *View) Start(ctx context.Context, ref string, in json.RawMessage) (*Job, error)
func (v *View) Job(id string) (*Job, bool)                              // view only (trusted Go code)
func (v *View) JobFor(ctx context.Context, id string) (*Job, bool)      // view plus job-access rule
func (v *View) Jobs(f JobFilter) []JobStatus
func (v *View) JobsFor(ctx context.Context, f JobFilter) []JobStatus
func (v *View) Intersect(other *View) *View
func (v *View) Watch(ctx context.Context) <-chan struct{} // fires (coalesced) whenever List or readiness changes

// Inside a handler.
func Progress(ctx context.Context, u Update)       // also a heartbeat; does nothing outside a job
func CallerFrom(ctx context.Context) (Caller, bool)
func JobIDFrom(ctx context.Context) (string, bool) // trusted active job ID for host glue
func ViewFrom(ctx context.Context) *View           // the view this call came in on
func WithView(ctx context.Context, v *View) context.Context

type Filter func(Capability) bool
func All(Capability) bool
func Match(globs ...string) Filter // "storage.*", "data.query"; '*' crosses dots
func Tagged(tags ...string) Filter
func ReadOnlyOnly(c Capability) bool
func And(fs ...Filter) Filter; func Or(fs ...Filter) Filter; func Not(f Filter) Filter
func Select(caps []Capability, f Filter) []Capability // filter a slice before Add

type Middleware func(next Handler, c Capability) Handler

// Caller identity, set by a projection (section 5).
type Caller struct {
    ID      string            // "user:ada", "agent:triage-7", "script:report.star"
    Kind    string            // "user", "agent", "script", "service"
    Session string            // the conversation or connection this call belongs to
    Parent  *Caller           // who started this caller, e.g. the user behind an agent
    Attrs   map[string]string
}
func WithCaller(ctx context.Context, c Caller) context.Context

// Tool names for LLM tool calling (MCP and in-process loops share these).
func ToolName(name string) string     // "storage.artifact.get" -> "storage_artifact_get"
func FromToolName(tool string) string // the inverse

// Errors that projections map: HTTP status, MCP isError, a Starlark error.
type Code string // invalid, not_found, denied, conflict, unavailable, cancelled, internal
func Errorf(code Code, format string, args ...any) error
func WrapCode(code Code, err error) error
func CodeOf(err error) Code
func (c Code) Retryable() bool // only unavailable
type Codes map[error]Code      // per-adapter sentinel -> Code table; sentinels must not overlap
func (c Codes) Wrap(err error) error // clashing matches give internal, never an arbitrary pick

// An optional reason refines a code with a stable cause.
const ReasonNoCapacity = "no_capacity"   // unavailable: healthy but full (e.g. a pool)
const ReasonBackendDown = "backend_down" // unavailable: the backend crashed or is unreachable
const ReasonNotReady = "not_ready"       // unavailable: refused because the readiness check fails
const ReasonClosed = "closed"            // unavailable: the registry is closed and starts no jobs
func ReasonErrorf(code Code, reason, format string, args ...any) error
func WrapReason(code Code, reason string, err error) error
func ReasonOf(err error) string
func ValidReason(reason string) bool

type ErrorInfo struct { // JSON form, in JobStatus, events and error envelopes
    Code      Code   `json:"code"`
    Reason    string `json:"reason,omitempty"`
    Message   string `json:"message"`
    Retryable bool   `json:"retryable"`
}
type ErrorEnvelope struct{ Error *ErrorInfo `json:"error"` }
func Info(err error) *ErrorInfo
```

**Reasons.** The `Code` set is closed and deliberately coarse, so one code can
span causes a consumer must tell apart: `unavailable` covers both a full pool (the
system working as designed) and a crashed backend. A reason is an optional,
machine-readable refinement within a code. It is a lower_snake_case word
(`ValidReason`), never free text; the message carries the human detail, and
`Error()` text is unchanged by it. The well-known reasons above are shared across
libraries; an adapter may add its own in the same form. `ReasonErrorf`/`WrapReason`
panic on an invalid reason (reasons are constants, so that is a programming
error). `WrapReason` always applies its code and reason, unlike `WrapCode`, which
leaves an already-coded error alone. `CodeOf`, `ReasonOf` and `Info` read the
outermost `*Error` in the chain, so code and reason always come from the same
classification and survive `%w` wrapping. A job keeps the reason in
`JobStatus.Error`, its terminal event and the error `Wait` returns; HTTP error
envelopes, `jobs.*` output, `tools.Result`, MCP error results and
`starlarkcap.Error` all carry it.

`Invoke` does the following:

1. Resolve the ref in this view (clamped to the view the call arrived on, for a
   nested call). A name outside the view fails with `not_found`, even when the
   capability exists elsewhere in the registry.
2. Fail fast with `unavailable`/`not_ready` if the capability isn't ready.
3. Check the input against `Input`. A failure is `invalid` (no reason) with a
   message written for the caller to fix the call, usually an agent: one line per
   offending field path in plain words (missing required field; wrong type,
   expected X, got Y; unknown field, with the accepted fields; value not in the
   enum, with the allowed values; out of range; too short/long), then the
   capability's required top-level fields and its first `Examples` entry as
   compact JSON. At most 8 problems are listed and the message is capped at
   1,500 bytes. It is built once here, so HTTP, MCP, tools and Starlark all carry
   the same text.
4. Run the middleware chain and then the handler, as a job (2.6).
5. Encode the result, and check it against `Output` when output validation is on
   (builds with the `craggy_debug` tag, or `ValidateOutput(true)`).

There are no plugins, providers, lifecycles or dependency-injection frameworks.

### 2.1 Naming

Names are dotted `namespace.noun.verb` (`storage.artifact.get`,
`classify.types.list`, `data.records.query`). The first segment is the adapter's
namespace, and `Replace(prefix, ...)` works on namespaces. A **ref** is a name, or a
name with a pinned version (`classify.artifact@2`).

Each segment may use only `[a-z0-9-]`, and `Add` rejects any name whose tool form
would be longer than 64 characters. Since `_` can't appear in a name, `ToolName`
(dots to underscores) can always be reversed by `FromToolName`. The 64-character
limit and the `^[a-zA-Z0-9_-]{1,64}$` pattern come from LLM tool-calling APIs and
many MCP clients. Checking them at `Add` means a capability that registers
successfully works in every projection.

| Projection | `storage.artifact.get` | pinned `@1` |
|---|---|---|
| LLM tool (MCP or in-process) | `storage_artifact_get` | not offered: agents always get the current version |
| HTTP | `POST /v1/storage/artifact/get` | `POST /v1/storage/artifact/get@1` |
| Starlark | `storage.artifact.get(id = "…")` | `call("storage.artifact.get@1", id = "…")` |
| CLI (later) | `<host> storage artifact get --id …` | `--version 1` |

### 2.2 Typed registration

Most capabilities are plain Go functions:

```go
type GetArtifactIn struct {
    ID string `json:"id" jsonschema:"artifact id"`
}

registry.Func("storage.artifact.get", "Get an artifact's metadata",
    func(ctx context.Context, in GetArtifactIn) (storage.Artifact, error) { … },
    registry.WithEffects(registry.ReadOnly|registry.Idempotent),
    registry.WithTags("agent"))
```

Schemas come from `github.com/google/jsonschema-go`, which the official MCP Go SDK
also uses, so a schema built by the registry goes straight into the MCP projection
without conversion. `SchemaFor` describes `json.RawMessage` fields as any JSON and
`[]byte` fields as base64 strings, the JSON they marshal to.

### 2.3 Runtime registration

A capability defined at runtime is just a `Capability` whose schema was parsed
rather than derived (`ParseSchema`), and whose `Handler` was written by hand. The
registry treats it like any other. A source that owns a group of runtime
capabilities (a Starlark file, a remote MCP server, a data source set) puts them
under one prefix. It then calls `Replace(prefix, caps...)` each time it reloads and
`Replace(prefix)` with nothing when it unloads. That single primitive replaces any
plugin lifecycle. `starlarkcap.Load` and `starlarkcap.Watch` are built on it
([Starlark guide](starlark.md)).

Snapshots are immutable, so a call that is already running keeps the capability it
started with.

### 2.4 Versions and supersession

Both are worked out from what is currently registered. Nothing about them is
stored separately, so adding or removing a capability updates them immediately.

**Versions.** A name can be registered at several versions at once
(`classify.artifact@1`, `classify.artifact@2`). The highest version is *current*.
A bare name always resolves to the current version, and `@N` pins an older one.
Agents only ever see the current version. HTTP clients and scripts can pin a
version so they keep working across upgrades. A job records the exact
`name@version` that ran, so audit records stay exact. A breaking change is a new
version, never an edit to an existing one. To retire an old version,
`Remove("name@1")`.

**Supersession.** A capability can declare that it replaces others, including ones
from a different namespace and ones registered by someone else:

```python
# apk.star, loaded at runtime long after the jadx and apktool adapters registered
capability(
    name = "apk.decompile",
    description = "Decompile an APK to sources and resources in an isolated container",
    supersedes = ["jadx.decompile", "apktool.decode"],
    long = True,
    input = {"artifact": schema.string(required = True)},
    run = _decompile,
)
```

- When `apk.decompile` is added, `jadx.decompile` and `apktool.decode` drop out of
  `List()`, so agents and listings stop offering them. They stay in `All()` with
  `SupersededBy: ["apk.decompile"]`.
- **Superseded capabilities still work** when called by name. Old scripts and
  pinned HTTP clients don't break. Each projection passes the notice on: HTTP sets
  a `Craggy-Superseded-By` header, tool results carry a `note:` line, Starlark
  prints a warning, `JobStatus.SupersededBy` records it for audit, and OpenAPI
  marks the operation deprecated.
- **Remove the replacement and the originals come back.** Unloading `apk.star`
  (`Replace("apk")`) puts `jadx.decompile` and `apktool.decode` back in `List()`
  straight away.
- A bare name supersedes every version, and `name@N` supersedes only that
  version. A newer version of the same name implicitly hides the older ones from
  `List()` (only the current version is listed).
- `Add` and `Replace` reject cycles (`a` supersedes `b`, which supersedes `a`). A
  rule may name something that isn't registered yet. It takes effect when that
  capability appears.
- **Who may supersede** is the host's decision for runtime sources. A Starlark
  file may supersede refs outside its own prefix only when the host's
  `LoadOptions.MaySupersede` filter admits them (2.10, open question 3).
- If something you don't control has to supersede (an imported MCP server's tool,
  say), the import sets `Supersedes` as it maps the tool. There is still only one
  mechanism.

### 2.5 Pagination

Every capability that lists or queries uses one convention:

```go
type PageIn struct {
    Limit  int    `json:"limit,omitempty"  jsonschema:"max items; default 50, max 500"`
    Cursor string `json:"cursor,omitempty" jsonschema:"opaque, from a previous next"`
}
type Page[T any] struct {
    Items []T    `json:"items"`
    Next  string `json:"next,omitempty"` // empty on the last page
}
func Paginate[T any](items []T, in PageIn) (Page[T], error)
```

Inputs embed `PageIn`, and outputs are `Page[T]`. `Paginate` slices a stably
ordered list with an opaque offset cursor. The conformance suite checks that
paginated capabilities return a `Page` and that following `next` visits each item
exactly once, through every projection.

### 2.6 Long-running jobs

Decompiling an APK in a container takes minutes. Every call runs as a job in the
registry's job table. For a quick capability the job lasts milliseconds and nobody
notices. For a `Long` one it's the main interface.

**The handler stays the same shape.** It reports as it goes and respects `ctx` for
cancellation. An adapter whose domain reports its own progress translates it in one
helper beside the adapter:

```go
func reportProgress(ctx context.Context, p domain.Progress) {
    registry.Progress(ctx, registry.Update{
        Stage: p.Stage, Message: p.Message,
        Done: p.Done, Total: p.Total, Unit: p.Unit,
        Data: map[string]string{"operation": p.Op},
    })
}
```

The registry imports no domain progress type; the helper belongs in the adapter.

```go
type Update struct {
    Stage   string // "pull-image", "decompile", "store-output"
    Message string
    Done    int64  // completed units in this stage, never whole-job progress
    Total   int64  // stage total; 0 if unknown
    Unit    string // "bytes", "files", "steps", ""; shared by Done and Total
    Data    any    // optional structured detail
}

type JobState string // queued, running, stalled, succeeded, failed, cancelled, lost

type JobStatus struct {
    ID           string
    Capability   string   // exact name@version
    Caller       Caller
    Parent       string   // job whose handler started this one
    State        JobState
    Last         Update   // latest progress
    Seq          int64    // monotonic per-job event sequence
    SupersededBy []string // notice at start time, for audit
    Started      time.Time
    Heartbeat    time.Time // time of the last Progress
    Ended        time.Time
    Error        *ErrorInfo // code, reason + message once failed or cancelled
}

func (j *Job) Status() JobStatus
func (j *Job) Events(ctx context.Context) <-chan Event // state changes and Updates, replaying from the start
func (j *Job) Wait(ctx context.Context) (json.RawMessage, error)
func (j *Job) Result() (json.RawMessage, bool)
func (j *Job) Cancel()
```

**Stage progress and job completion are separate contracts.** `Done` and `Total`
describe only the named stage, in `Unit`. Counters are non-negative; a known total
must be at least `Done`. With `Total == 0`, `Done` may still report observed work,
but no percentage is available (`Update.Percent` returns -1). A stage change may
reset the counters or change their unit. No consumer may add unlike units, average
stage percentages, or treat `Done == Total` as whole-job completion. There is no
overall percentage field; show the current stage and job state.

Only the job runner sets terminal state, after the handler and the full middleware
chain return and result encoding (and validation, when on) succeeds. A host whose
capabilities produce durable outputs must finish publishing them, and finalise its
audit record, before that chain returns success; report stages such as
`publish-output` and `finalise` while those steps run. A zero exit code, a
completed transfer or a released resource cannot mark the job succeeded.
Publication or finalisation failure fails the job; cancellation or interruption
cannot be converted to success by a late progress callback. Terminal jobs ignore
late progress and cannot return to running. A progress update is never a terminal
event or proof of persistence.

Every consumer preserves these semantics: `JobStatus.Last`, event replay,
`jobs.get/list/wait`, HTTP status and SSE, MCP and tools, Starlark `ctx.progress`
and any UI. They carry stage, counters and unit together. Where a transport
requires monotonic whole-request progress (MCP progress notifications), the
projection sends a monotonic notification sequence with no total and puts the
stage counters and unit in the message; it never sends resetting stage counters as
whole-request progress. Progress may arrive per item (a 200k-file transfer), so
replay keeps only the latest of consecutive updates for one stage (event `Seq`s can
skip there), and MCP forwarding sends a new stage at once but paces repeats within
a stage to one notification per 250ms, reading the latest from status. The
structured job status remains authoritative. A UI renders unknown totals as
indeterminate and displays success only from terminal job state.

Acceptance properties (tested in `registry`, `mcp`, `httpapi` and conformance):

- A stage reaches 100% and later stages remain active: the job stays running and
  every projection shows only stage completion.
- A stage switches from bytes to files, or has an unknown total: structured
  consumers retain units and show no fabricated overall percentage; MCP's
  notification sequence never decreases across the switch.
- The handler's work succeeds but publication or result encoding fails: the job
  fails and no successful result is emitted.
- Cancellation, restart (`lost`) and late callbacks never produce a false success
  or revive a terminal job; replay preserves the terminal state.

Further:

- **Status, progress and liveness come for free.** Every job has a state, a latest
  `Update`, and a heartbeat time. A running job with no `Progress` for longer than
  `Heartbeat` becomes `stalled`. That's a warning, not a kill: it changes back to
  `running` on the next update. Whether to cancel a stalled job is up to the caller
  or the host's policy.
- **Jobs outlive the request that started them.** The job context keeps the caller
  and view but not the request's cancellation (`context.WithoutCancel`). Only
  `Cancel()` stops a job, and it cancels the handler's `ctx`. A job started from
  inside another job's handler is its child (`JobStatus.Parent`): cancelling the
  parent cancels the child, and so on down, while a parent that finishes normally
  leaves its children running. Handlers propagate cancellation to whatever they
  drive; releasing resources a job holds on every exit is the handler's and the
  host's responsibility.
- **Concurrency.** `Limit(tag, n)` bounds concurrent capability calls, such as
  `Limit("analysis", 4)`. It is application backpressure, not physical capacity
  accounting: admission of RAM, CPU or other physical resources, and quotas, belong
  to the host or the domain that owns those resources, including for direct Go
  calls. A call waiting for a slot stays `queued`.
- **The job controls are themselves capabilities.** The `jobs` package's
  `jobs.Capabilities()` returns `jobs.list`, `jobs.get`, `jobs.wait` and
  `jobs.cancel`, and `jobs.Group()` their group. The host adds them like any other
  capabilities, and every projection gets them. `jobs.wait` blocks for up to
  `timeout` seconds (60 at most) and returns the status plus the result if the job
  finished; agents use it to poll without a busy loop. Each job capability acts
  only on jobs whose capability is in the caller's view and which the access rule
  allows. By default, a caller sees jobs from its own `Session` and from sessions
  it started (`Parent`). The host can replace that rule (`WithJobAccess`).
- **Durability.** The job table is in memory and keeps finished jobs for a while
  (`KeepJobs`, default 1h). `OpenJobJournal(path)` appends each job's latest status
  record as events happen; on the next start it restores terminal jobs (their
  results remain waitable) and marks unfinished ones `lost` with reason
  `process_restarted`. It never resumes a handler: recovering interrupted work is
  an explicit new call the host coordinates. `JobJournalError` reports a write
  failure, for health. The lasting record of what a call did is the host's audit
  record (section 5).
- **Closing.** `Close` stops the readiness loop, cancels every unfinished job and
  waits for job runners to return, bounded by `JobDrainTimeout` (default 10s), so
  terminal states and journal records are written before `Close` returns. A handler
  that ignores cancellation past the bound is abandoned. Once `Close` starts, `Start`
  fails with `unavailable`/`closed`; the job table insert and the runner count
  happen together under the lock `Close` takes, so no call slips between them.

How each projection handles a `Long` capability:

| Projection | Behaviour |
|---|---|
| Go | `Invoke` waits. `Start` returns a `*Job`. |
| HTTP | `POST` returns `202 {"job": id, "status": …}` with a `Location: /v1/jobs/{id}` header. `GET /v1/jobs/{id}` returns the status (plus the result on success), `GET /v1/jobs/{id}/events` streams events as Server-Sent Events, and `DELETE /v1/jobs/{id}` cancels. Sending `Prefer: wait=N` holds the request for up to N seconds before falling back to 202. |
| MCP / tools | The tool returns `{"job": id, "status": …}` immediately, and the agent follows up with `jobs_wait`. This works with every client. If the client sent a `progressToken`, updates are also sent as `notifications/progress`. |
| Starlark | `apk.decompile(...)` blocks, and waiting doesn't count against the script's step limit. `jobs.start("apk.decompile", ...)` returns a job, for scripts that want to run several at once. |

### 2.7 Readiness

A capability can supply `Check`, for example "is the container daemon up and is
the decompiler image pulled?". The registry runs every check on an interval
(`CheckEvery`, default 30s), each bounded by `CheckTimeout` (default 10s), and when
a capability is added; `RefreshChecks` runs them now. A capability that fails its
check:

- stays in `List()` with `Ready: false` and its `CheckError`, so HTTP listings and
  `GET /v1/health` show why;
- is left out of `tools.Defs`, and so out of MCP tool lists, so agents don't waste
  turns on it;
- fails fast with `unavailable` and reason `not_ready` (and the check's message)
  on `Invoke` or `Start`.

Readiness asks "can this capability run at all right now?". Job heartbeats answer
the separate question "is this particular run still alive?".

### 2.8 Groups

An agent with a hundred-plus tools can't tell from a flat list what a family of
them is for. A **group** gives a family one line:

```go
r.AddGroup(registry.Group{Name: "web", Description: "Search the public web and capture pages."})
r.AddGroup(jobs.Group()) // "jobs", LoadAlways
```

- **Membership is by name, never by assignment.** A capability belongs to the
  registered group with the longest name that is the capability's name or a dotted
  prefix of it: with `platform` and `platform.scripts` registered,
  `platform.scripts.run` is in `platform.scripts` and `platform.overview.get` in
  `platform`. The same registrations always give the same groups, in any order.
  `Entry.Group` and `Descriptor.group` carry it (empty when no group claims the
  name).
- **The adapter that owns the capabilities registers their group**, beside them
  (`jobs.Group` beside `jobs.Capabilities`). Re-registering an identical group is a
  no-op; the same name with different fields is a `conflict`. Groups outlive
  `Replace` and `Remove` of their members.
- **`View.Groups()`** lists the groups with at least one `List()` entry in that
  view, sorted by name. That is the catalogue a host shows an agent.
- **`LoadAlways`** is a hint for hosts that load tools on demand. Such a host
  advertises a `LoadAlways` group's tools from the first step and the rest only
  after the agent asks for that group. The registry itself never hides anything
  because of it.

### 2.9 Descriptors

`Entry.Descriptor()` is the full serializable projection of one entry: every
`Capability` field that can cross a boundary (schemas as JSON, effects as names,
heartbeat in milliseconds, `HasCheck`, `Origin`) plus live state (`current`,
`supersededBy`, `group`, `ready`, `checkError`). Only `Handler` and `Check`
themselves are absent. It is the HTTP listing's wire form verbatim and the one
shape catalogues, system panels and documentation generators consume.

### 2.10 Provenance

`Capability.Origin` records where a definition came from. Go-registered
capabilities have none unless the host sets `WithOrigin`. `starlarkcap.Load` sets
`{Kind: "script", Ref: <filename>, Digest: <content address>}` on every capability
a file defines, where the digest covers the file, every module it loaded and, when
the host passes `LoadOptions.TypesDigest`, its type declarations. `Load` also
reads `Origin` back: it refuses to replace capabilities another file loaded. The
registry stores and projects `Origin` (`Descriptor.origin`) and never interprets
it; audit middleware can record it beside the exact `name@version`.

## 3. Projections

Each projection is one package, takes a `*registry.View`, and stays in sync
through `Watch` or by re-listing per request. See the
[projections guide](projections.md) and the [Starlark guide](starlark.md).

### MCP: `mcp`

```go
func Serve(s *mcpsdk.Server, v *registry.View, opts ...Option) (stop func())
// options: Caller(func(*mcpsdk.ServerSession) registry.Caller), OnClose(func(registry.Caller))
```

- **Built on `tools`.** Tool definitions come from `tools.Defs(v)`, and calls go
  through the same `tools` results. `Effects` map to the tool annotations
  `readOnlyHint`, `destructiveHint`, `idempotentHint` and `openWorldHint`.
- **On `Watch`,** it diffs the old and new tool lists and calls `AddTool` or
  `RemoveTools`. The SDK sends `notifications/tools/list_changed` itself.
  Supersession, new versions and readiness changes all reach the client this way.
- **Caller.** It sets `Caller` once per MCP session: `Kind: "agent"`, with
  `Session` set to the MCP session ID (or a generated stable ID), and the rest
  from the `Caller` option. `OnClose` tells the host when a session that made a
  call ends (section 5). The session's caller is forgotten when it ends whether or
  not `OnClose` is set: a stateless transport makes one short session per request,
  so leaving it to the hook would leak one entry per call.
- **Structured output.** A tool whose capability declares an `Output` schema
  advertises it as the tool's `outputSchema`, and a successful call returns the
  output as `structuredContent` beside the text (the MCP spec requires both, and
  strict clients reject a result without it). A `Long` capability returns a job
  handle, not its output, so its tool advertises no `outputSchema`. Errors and
  non-object outputs are text only.
- **Results.** The first text block is the payload, exactly as `tools.Result.Text`
  has it; notes, when there are any, follow as a second text block, so the first
  stays the serialized `structuredContent`. A failed call's payload is the error
  envelope. A call that ran (not a `Long` call's handle) is timed and carries the
  slow-call note. An unknown tool name is the SDK's protocol error, as the MCP spec
  has it.

### HTTP: `httpapi`

```go
func Handler(v *registry.View, opts ...Option) http.Handler
// options: Caller(func(*http.Request) registry.Caller), Info(title, version)
```

- `POST /v1/{ref as path}` takes a JSON body and returns `{"result": …}` or the
  error envelope `{"error": {"code", "reason"?, "message", "retryable"}}`. `Long`
  capabilities follow the jobs table in 2.6.
- `GET /v1/` lists `List()` as descriptors, and `GET /v1/?all=1` lists `All()`.
  `GET /v1/health` reports readiness (503 when any listed capability is not
  ready). `GET /v1/openapi.json` returns an OpenAPI 3.1 document that includes the
  pinned versions and the `Craggy-Session` and `Craggy-Superseded-By` headers; it
  is generated on each request, so runtime changes show up immediately.
- Errors map by `Code`: invalid 400, denied 403, not_found 404, conflict 409,
  cancelled 499, unavailable 503, anything else 500.
- **Caller.** Authentication is not part of this package. The host wraps the
  handler and puts a `Caller` in the request context, or supplies the `Caller`
  option. `Session` comes from a `Craggy-Session` header when the client sends one
  (section 5).

### In-process tool-calling loops: `tools`

Use this for an agent you run yourself, calling a model API directly rather than
going through MCP. The package handles only the registry side of the loop. It
doesn't depend on any model provider's SDK.

```go
type Def struct {
    Name        string             // ToolName(c.Name)
    Description string             // Title, Description and DescriptionNotes, blank-line separated
    InputSchema *jsonschema.Schema // never nil: an empty object schema when c.Input is nil
    Entry       registry.Entry     // effects, tags, Long, exact ref, for hosts and MCP
}

func Defs(v *registry.View) []Def // one per ready entry in v.List(), sorted by name
func Call(ctx context.Context, v *registry.View, tool string, input json.RawMessage) (text string, isError bool)

type Result struct {
    Text       string          // the payload: output, job handle, or error envelope
    IsError    bool
    Notes      []string        // facts about the call, rendered after Text
    Structured json.RawMessage // output as structured content, when advertised
    Job        string          // a Long call's job ID
    Code       registry.Code   // on failure: the error's code (not_found for an unknown tool)
    Reason     string          // on failure: the error's reason, if any
}
func (r Result) Render() string                // what the model reads: Text, then "note: " lines
func (r Result) Elapsed(d time.Duration) Result // adds "took 12.3s" when d >= SlowCall (5s)
func Do(ctx context.Context, v *registry.View, tool string, input json.RawMessage) Result
func ErrorResult(err error) Result
func Outcome(e registry.Entry, out []byte, err error) Result
func Handle(e registry.Entry, job *registry.Job) Result
```

A host that records tool failures reads `Result.Code`/`Reason` rather than
re-parsing `Text` or re-implementing `Do`, and a host that follows a job reads
`Result.Job`.

#### Tool results

Every projection a model reads (MCP, `tools`) gives a call's result the same
shape. It is text, because that is what every model API and MCP host passes to the
model; the payload inside is JSON.

- **Success:** the capability's output, verbatim. No wrapper: `isError` already
  says it succeeded, and an envelope would cost tokens on every call for nothing
  the model uses. A `Long` call's payload is its job handle (`JobEnvelope`).
- **Failure:** the error envelope, one line of JSON with `isError` set, the same
  body the HTTP projection returns:

  ```json
  {"error":{"code":"unavailable","reason":"no_capacity","message":"every emulator slot is in use","retryable":true}}
  ```

  `code` is the closed set above; `reason` (optional) is its stable
  lower_snake_case refinement; `message` says what went wrong and what to change;
  `retryable` is true only when the same call may succeed later unchanged
  (`unavailable`). Everything else needs a different call. A tool name outside the
  view, even one the model invents, is `not_found` with reason `unknown_tool`.
- **Notes:** facts about the call rather than its output, one `note: <text>` line
  each after the payload: the capability is superseded (`note: x is superseded by
  y`), the call was slow (`note: took 12.3s`, from `SlowCall`, 5s). They are kept
  in `Result.Notes`, never in `Text`, so a host can always parse the payload.

A host whose own agent loop produces failures (an invalid argument, an exhausted
budget) should render them in the same envelope, so the model reads one shape.

An agent that sees only part of the registry:

```go
view := r.View(registry.And(
    registry.Match("storage.*", "classify.*", "apk.*", "jobs.*"),
    registry.Not(registry.Tagged("admin")),
))
ctx = registry.WithCaller(ctx, registry.Caller{ID: "agent:triage-7", Kind: "agent", Session: convID, Parent: &user})

for {
    resp := model.Send(ctx, history, toProvider(tools.Defs(view))) // re-list each turn
    if len(resp.ToolCalls) == 0 {
        break
    }
    for _, tc := range resp.ToolCalls {
        text, isErr := tools.Call(ctx, view, tc.Name, tc.Input)
        history.AddToolResult(tc.ID, text, isErr)
    }
}
```

- **The view is the agent's whole world.** A tool name outside it, even one the
  model invents, gets `not_found` from `Call`. Filtering and enforcement are the
  same thing.
- **Re-list every turn.** Capabilities that are added, superseded or become unready
  at runtime change the agent's tools on its next turn.
- **The caller flows through.** `WithCaller` reaches the middleware and the
  adapters, so the agent's actions are attributed to its own session (section 5).
- **`toProvider` is a few lines in the agent's own code.** The `Def` fields map
  one-to-one onto provider tool definitions (`name`, `description`,
  `input_schema`).

### Starlark: `starlarkcap`

```go
func Globals(ctx context.Context) starlark.StringDict // namespaces as nested built-ins, plus call() and jobs.start
func Exec(ctx context.Context, p Program, lim Limits, hooks Hooks) (Result, error) // run a script
func Check(v *registry.View, filename string, src []byte, extra ...string) []Diagnostic
func Analyze(v *registry.View, filename string, src []byte, extra ...string) Analysis
func AnalyzeWith(v *registry.View, filename string, src []byte, opts CheckOptions) Analysis
func Env(v *registry.View) check.Env
func Load(ctx context.Context, r *registry.Registry, prefix string, src Source, opts LoadOptions) (Loaded, error)
func Watch(ctx context.Context, r *registry.Registry, opts WatchOptions) (*Watcher, error)
func Test(t *testing.T, r *registry.Registry, fsys fs.FS)
```

Everything generic (the dialect, value conversion, schemas, limits, `load()`,
static checks, the test runner) comes from `github.com/craggy-io/script-services`;
`starlarkcap` adds the registry.

- `Globals` exposes the capabilities in `ViewFrom(ctx)` to scripts as built-ins.
  Keyword arguments become the input object, and the result comes back as
  Starlark dict, list and scalar values. `call(ref, **kwargs)` reaches pinned
  versions and superseded entries. `Globals` takes a snapshot when called, so one
  script run sees one consistent set of names: namespaces come from `List()`, and
  `call()`/`jobs.start()` refs must resolve in `All()` as it was then. The call
  itself runs against the live view.
- `Exec` is the script host: it runs a program (module body top to bottom, then
  `run(input)` when defined) against `Globals` plus the host's own built-ins and a
  frozen `input`, under `Limits` (steps, wall-clock; the wall-clock limit also
  bounds every capability call the script is blocked in). `Program.Modules` serves
  `load()`, and `Result` reports the modules loaded and a content digest.
  `Hooks.Print` receives `print()` with its source position (and superseded
  warnings); `Hooks.Call` reports every capability call twice (started with its job
  ID, settled with its error) with the call site, which is what a UI needs for
  per-line markers. Failures are `*Error` with the registry code (the failing
  capability's, with its reason; `cancelled`; else `invalid`), the innermost script
  position and a backtrace. `Exec` tracks every job the script starts: a failed or
  cancelled script cancels the ones still running. What a host adds (artifacts,
  persistence of runs) is the host's: this package knows none of it.
- `Check` is the static half: parse and resolve in the same dialect, then check
  every capability path, literal `call()`/`jobs.start()` ref and keyword argument
  against the view's input schemas, with nearest-name suggestions. `AnalyzeWith`
  also checks `load()` targets. It runs nothing, so an agent can validate a script
  before spending anything on it.
- `Load` is the other direction: a script **defines** capabilities, with
  `capability(name, version, title, description, input, output, effects, tags,
  supersedes, accepts, emits, long, heartbeat, run, check)`. The file is checked
  before anything runs; every name must be under the prefix; supersession outside
  the prefix needs `LoadOptions.MaySupersede`. One file owns one prefix: `Load`
  refuses (`conflict`) a prefix holding another loaded file's capabilities.
  Inside `run`, `ctx.progress(...)` calls `registry.Progress`. `Load` calls
  `Replace(prefix, …)`, and each handler runs its function in a fresh thread with
  step and time limits, the time limit covering its `ctx.call`s. `Watch` keeps a
  directory of such files loaded (one prefix per stem, no `.` in a stem, an
  optional shared `types.go`); `Test` runs their `*_test.star` files.
- **Starlark-defined capabilities are visible by default.** They go through the
  same filters as Go-defined ones, so a view matching `apk.*` includes
  `apk.decompile` the moment its file loads. That, together with the host's
  supersession policy, lets a script take over Go capabilities for agents.
- **No privilege escalation.** When a Starlark capability calls other
  capabilities, those calls go through the *caller's* view (`ViewFrom(ctx)`), not a
  wider script view. A read-only agent that calls `apk.summary` can't write
  anything through it, whatever the script declares.

### Later, same pattern

- A CLI package that turns a view into subcommands and flags.
- `mcp.Import(r, prefix, client, opts...)`: proxy a remote MCP server's tools as
  capabilities, calling `Replace` whenever its tool list changes, with options to
  set `Supersedes` and `Tags` per imported tool.

## 4. Adapters for the domain libraries

Each domain module gets an optional sub-package that imports its own package and
`registry`, and nothing else. It exports one function. For a hypothetical storage
library:

```go
package storagecap // in the storage module, not here

type Deps struct {
    Store *storage.Store
    // Session maps the caller to a storage session. The host owns this
    // mapping (section 5); the adapter only asks for it.
    Session func(ctx context.Context) (*storage.Session, error)
}

func Capabilities(d Deps) []registry.Capability
```

The host then decides what it wants:

```go
r := registry.New(registry.Use(audit, policy), registry.Limit("analysis", 4))

r.Add(storagecap.Capabilities(storagecap.Deps{Store: st, Session: sessions.For})...)
r.Add(classifycap.Capabilities(engine)...)
r.Add(host.Capabilities(st, engine)...) // cross-domain composition, e.g. classify.artifact
r.Add(jobs.Capabilities()...)
r.AddGroup(jobs.Group())

agents  := r.View(registry.And(registry.Or(registry.Tagged("agent"), registry.Match("apk.*", "jobs.*")), registry.Not(registry.Tagged("admin"))))
scripts := r.View(registry.Not(registry.Tagged("admin")))
api     := r.View(registry.All)

stopMCP := mcp.Serve(mcpServer, agents, mcp.OnClose(sessions.End))
http.Handle("/v1/", auth(httpapi.Handler(api)))
```

**Exposing only part of a library:** an adapter returns small, single-purpose
capabilities, and the host filters them:
`registry.Select(storagecap.Capabilities(d), registry.Match("storage.artifact.*", "storage.query"))`.
To restrict access *per audience*, use views instead and keep everything
registered.

Guidance for adapters:

- Writes ask the host-injected dependency (`Deps.Session(ctx)` above) for whatever
  context they need; the adapter never decides who the caller is.
- Large data moves by reference (an artifact ID or a digest), never as JSON.
- List and query capabilities use `PageIn`/`Page`.
- Administrative capabilities are tagged (`admin`) so views can exclude them.
- An adapter whose domain reloads its own definitions (a data-source set, say)
  watches for that and calls `Replace(prefix, …)` with the regenerated
  capabilities.
- Each adapter translates its own sentinel errors in one table:

```go
var codes = registry.Codes{storage.ErrInvalid: registry.Invalid, storage.ErrNotFound: registry.NotFound}
// used as: return out, codes.Wrap(err)
```

## 5. Callers, sessions and middleware

### Who maps a connection to a session?

**Not the code that registers the capability.** An adapter can't know whether a
call came from a user's browser, an agent's MCP connection or a script. The
projection can't know about the host's session model, either. The work is split
three ways:

| Who | Responsibility |
|---|---|
| **Projection** | Says *who* is calling, by filling in `Caller`: identity, kind, the `Session` correlation ID, and `Parent`. It also reports when a connection closes (`mcp.OnClose`). |
| **Host** | Says *what that caller means*: which session, tenant, quota or audit context it maps to. It keeps a map from `Caller.Session` to its own session objects, opens them lazily on first use, and ends them on close or after being idle. It correlates `JobIDFrom(ctx)` with any execution resources a job holds. |
| **Adapter** | Asks the host-injected dependency whenever it needs that context, and does nothing else. |

Typical host policies, none of which this module implements:

| Caller | Host session |
|---|---|
| HTTP user, no `Craggy-Session` header | One session per principal per request, ended when the request finishes. |
| HTTP client that opened a session | A host capability opens a session and returns an ID. Requests that send it in `Craggy-Session` share one session until it is closed or idles out. A UI uses this so that one investigation is one session. |
| MCP connection | One child session per MCP session, whose parent is the session of the user who launched the agent (`Caller.Parent`), or a service session when there is none. Ended by `OnClose`. |
| In-process agent loop | The same as MCP, with the conversation ID as `Session`. |
| Starlark capability | Runs inside the caller's session, attributed to both the script (`Origin`) and whoever invoked it. |
| Job | Keeps the caller's session. If that session has ended by the time the job writes, the host decides whether to reopen a child session under the same parent, so a long job is still attributed correctly after the agent disconnects. |

The registry only carries the identity. It never imports the host's session or
storage types.

### Middleware

Cross-cutting concerns live in middleware, never in a handler or a projection:

- **Audit.** Records each call in the caller's session: the exact
  `name@version`, the input, the job ID and parent job, the final state, any
  `SupersededBy` notice and the capability's `Origin`. Every agent action,
  including every long job, is then recorded without any capability knowing about
  it.
- **Authorization.** Checks `CallerFrom(ctx)` against the capability's tags and
  effects, beyond what views already filter.
- **Limits.** Timeouts for quick capabilities. For concurrency, `Limit(tag, n)`
  (2.6).
- **Telemetry.** A span or log line per call, plus one per job state change.

`Use(mw...)` installs middleware; the first one added is outermost. A job completes
only after the whole chain returns, so audit finalisation inside middleware is part
of the job's success.

## 6. Repository layout

```
README.md, AGENTS.md
docs/
  design.md          this file
  registry.md jobs.md projections.md starlark.md conformance.md
  assets/            wordmarks and the architecture diagram
registry/            core: capability.go func.go registry.go view.go filter.go name.go
                     invoke.go explain.go job.go job_journal.go ready.go group.go
                     descriptor.go page.go caller.go errors.go option.go
                     (deps: stdlib + google/jsonschema-go)
jobs/                jobs.* capabilities, Group, WaitOut
tools/               LLM tool definitions + tool-result encoding (no provider SDK)
mcp/                 MCP projection, built on tools/ (deps: modelcontextprotocol/go-sdk)
httpapi/             HTTP projection, SSE job events, OpenAPI (stdlib only)
starlarkcap/         Globals, Exec, Check, Load, Watch, Test (deps: script-services, go.starlark.net)
conformance/         the suite: one capability set through every projection
examples/host/       a runnable host wiring fake domains together
```

Domain adapters (`storagecap`, `classifycap`, `datacap`, ...) live in **their own
modules**, beside the APIs they wrap, and version with them; this module stays
domain-free. To develop several modules together, use a `go.work` that uses them
all (see the [README](../README.md#development)).

## 7. Build order

The order the module was built in, and the gate each step had to pass:

1. **Core.** `Capability`, `Func` with schema derivation, `Registry` (`Add`,
   `Replace`, `Remove`, snapshots), name and ref rules, versions, supersession
   (cycle rejection, `List` vs `All`), views and filters, `Watch`, `Invoke` with
   validation and middleware, callers, error codes, and `PageIn`/`Page`. Tests:
   concurrent `Replace` during `Invoke`, supersession appearing and going away,
   version pinning, filter semantics, and `Watch` coalescing.
2. **Jobs and readiness.** The job table, `Start`, `Progress`, heartbeat and
   stalled detection, cancellation, the per-tag queue, the `jobs` capabilities, the
   default access rule, and `Check` polling. Tests: a fake long handler that streams
   progress, stalls, recovers, is cancelled, and outlives its caller's context.
3. **HTTP projection,** including 202 + `Location`, `Prefer: wait`, SSE events,
   `/health` and OpenAPI.
4. **`tools`, then the MCP projection** built on it, with live add and remove
   through `Watch`, `jobs_wait`, and progress notifications, tested with the SDK's
   in-memory transports.
5. **Conformance suite.** The same capability set through `Invoke`, HTTP, MCP,
   `tools` and Starlark, requiring identical results, error codes, job behaviour
   and pagination. Every new projection has to pass it.
6. **Starlark `Globals`, then `Load`,** including `supersedes`, `long` and
   `ctx.progress`, and the test that a script can't escalate privileges; then
   `Exec`/`Check`, the supersession policy, `Watch` and `Test`, with the generic
   runtime moved to script-services.
7. **Host plumbing** (outside this module): session mapping, audit middleware,
   injected execution and publication ports.
8. **Domain adapters** (outside this module).

## 8. Changes since the first design

- **`jobs` package.** The job capabilities moved out of `registry` into their own
  package: `jobs.Capabilities()`, `jobs.Group()` and `jobs.WaitOut`, the
  `jobs.wait` output type.
- **Renames.** The HTTP headers are `Craggy-Session` and `Craggy-Superseded-By`;
  the debug build tag that turns on output validation is `craggy_debug`; the
  conformance package is `conformance`.
- **Provenance.** `registry.Origin`, `Capability.Origin`, `WithOrigin` and
  `Descriptor.Origin` record where a definition came from; `starlarkcap.Load` sets
  it for script capabilities, with a content digest.
- **Close drains.** `Registry.Close` waits for job runners after cancelling them,
  bounded by `JobDrainTimeout`, so terminal states and journal records are written
  before it returns.
- **Status journal.** `OpenJobJournal` and `JobJournalError` give restart
  visibility (`lost`, `process_restarted`) without making jobs durable.
- **Starlark runtime split.** The generic runtime (dialect, values, schemas,
  types, modules, checks, test runner) is the separate module
  `github.com/craggy-io/script-services`; `starlarkcap` is the only package that
  imports it.
- **The new `starlarkcap` API:**
  - `Load(ctx, r, prefix, Source, LoadOptions) (Loaded, error)`, where `Source`
    carries the filename, text and a `modules.Source` for `load()`;
  - prefix-scoped names: every capability a file defines must be under its prefix;
  - supersession outside the prefix only via `LoadOptions.MaySupersede`, which
    resolves [open question 3](#10-open-questions);
  - check before install: the file is statically checked before its top level
    runs, and a failed load leaves the registry unchanged;
  - types: `LoadOptions.Types` enables `schema.type("Name")` and `input = "Name"`
    from Go-syntax declarations;
  - module digests: `Loaded.Digest` and `Loaded.Modules` content-address the file
    and everything it loaded (with `LoadOptions.TypesDigest`, its types too);
  - `Watch` hot-reloads a directory (`WatchOptions.Dir`, read through an
    `os.Root`), one prefix per file stem;
  - `Test` runs `*_test.star` files against a registry;
  - `Exec` takes `Program.Modules` and reports `Result.Digest` and
    `Result.Modules`;
  - `AnalyzeWith` (with `CheckOptions.Modules`) and `Env` expose the static
    checker's environment.
- **Known limits.**
  - Errors inside script-defined capabilities surface as `internal`, with the
    nested capability's code in the message rather than as the call's code.
  - Under `Watch`, a script file stem claims its whole prefix, including
    Go-registered capabilities under it (one file still cannot wipe another's:
    nested stems are rejected and `Load` refuses with `conflict`).
  - Jobs are not durable beyond the status journal.

## 9. Deliberately left out (for now)

- **Large binaries in calls.** Big data travels by reference. If uploads over HTTP
  are needed, that's a domain-specific endpoint, not a registry feature.
- **A durable job queue.** Jobs that were running when the process dies show as
  `lost` when the status journal is on. Resuming them requires the host to
  coordinate a new, linked call with whatever owns the underlying resources. The
  registry does not persist or resume work.
- **Native MCP tasks.** `jobs_wait` works with every MCP client today. `Serve` can
  switch to native tasks later without any change to capabilities.
- **`mcp.Import` and a CLI projection** (3, "Later, same pattern").
- **MCP resources and prompts.** These could come later as a second, equally small
  registration shape, if agents need them.

## 10. Open questions

1. **Job access across users.** The default rule lets a caller see jobs from its
   own session and the sessions it started. Should a user also see jobs that other
   users' agents started on shared resources, or should that need `admin`? Hosts
   decide today with `WithJobAccess`.
2. **Superseded capabilities for agents mid-conversation.** If an agent already
   called `jadx_decompile` and then it's superseded, the next turn's tool list
   drops it. Should `tools.Call` still run it if the model calls it again (the
   current behaviour: yes, with a note in the result), or should it refuse and name
   the replacement?
3. **Who may supersede.** *Resolved.* A Starlark file may supersede capabilities
   under its own prefix freely, and outside it only when the host's
   `LoadOptions.MaySupersede` filter admits the target; the filter sees the
   registered capability, so tag policies such as
   `registry.Not(registry.Tagged("admin"))` work, and `nil` admits nothing. A
   denied supersession fails the load with `denied` and installs nothing. Under
   `Watch`, the remaining exposure is a file stem that names a Go namespace, which
   replaces that whole prefix; hosts reserve script directories for script
   prefixes.
