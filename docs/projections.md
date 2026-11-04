# Projections: tools, MCP and HTTP

[Project](../README.md) · [Documentation index](README.md) · [Design](design.md#3-projections)

Packages `tools`, `mcp` and `httpapi`. Available Go API. The Starlark projection
has its [own guide](starlark.md).

## Purpose

Translate a view mechanically into the surfaces that call capabilities: an
in-process model loop, an MCP server, and an HTTP API with OpenAPI. A projection
adds transport, never policy: what a caller may see and call is the view it is
given.

## Features

- One tool-definition and tool-result shape for every model-facing surface
  (`tools`, `mcp`), with structured output where the capability declares it.
- Live MCP tool lists: registration, supersession, version and readiness changes
  reach clients as `notifications/tools/list_changed`.
- Effects as MCP tool annotations; job progress as monotonic MCP progress
  notifications.
- HTTP invocation by ref path, pinned versions, 202 + `Location` for long calls,
  `Prefer: wait=N`, job status, SSE events and cancel, listings, health, and an
  OpenAPI 3.1 document generated per request.
- One error envelope, mapped from the registry code, in every projection.

## Mental model

Each projection takes a `*registry.View`. `tools.Defs(v)` is the model-facing
listing: one definition per **ready** entry in `v.List()` (current, not
superseded). `mcp.Serve` is `tools` plus a sync loop on `v.Watch`. `httpapi`
re-reads the view on every request. Calls go through `v.Invoke`/`v.Start`, so
validation, readiness and view enforcement happen once, in the registry.

## Responsibilities and boundaries

Projections fill in `registry.Caller` and nothing more. Authentication, session
mapping and audit are the host's: wrap the HTTP handler, supply `Caller` options,
and react to `mcp.OnClose`. No projection imports a model provider SDK; `mcp`
imports the official MCP Go SDK.

## tools: in-process tool-calling loops

```go
func Defs(v *registry.View) []Def
func Do(ctx context.Context, v *registry.View, tool string, input json.RawMessage) Result
func Call(ctx context.Context, v *registry.View, tool string, input json.RawMessage) (text string, isError bool)
func ErrorResult(err error) Result
func Outcome(e registry.Entry, out []byte, err error) Result
func Handle(e registry.Entry, job *registry.Job) Result
func StructuredOutput(e registry.Entry, out []byte) json.RawMessage
func AppendNotes(text string, notes ...string) string
func ElapsedNote(d time.Duration) string
const SlowCall = 5 * time.Second
```

- `Def{Name, Description, InputSchema, Entry}`: `Name` is `ToolName(c.Name)`;
  `Description` joins the title, description and `registry.DescriptionNotes`
  (subjects and examples) with blank lines; `InputSchema` is never nil (the empty
  object schema for no input); `Entry` carries effects, tags, `Long` and the exact
  ref for hosts. The fields map one-to-one onto provider tool definitions.
- `Do` maps the tool name back, runs a quick capability to completion or starts a
  `Long` one and returns its `JobEnvelope` handle (`{"job", "status"}`), and fills
  `Result`: `Text` (the payload: output, handle or error envelope; never notes),
  `IsError`, `Notes`, `Structured` (the output, when the capability declares an
  output schema, is not `Long`, and returned a JSON object), `Job`, and on failure
  `Code`/`Reason`. `Render()` is what the model reads: `Text`, then one
  `note: <note>` line per note. `Call` is `Do(...).Render()` plus `IsError`.
- Notes are facts about the call: `x is superseded by y` for a superseded
  capability the model still names (it runs), and `took 12.3s` from
  `Result.Elapsed(d)` when `d >= SlowCall`. `Do` does not time calls; the host
  measures what the model waited for.
- A tool name outside the view, including one the model invents, is `not_found`
  with reason `unknown_tool`.
- Re-list every turn: unready, superseded and newly added capabilities change the
  definitions immediately.

## mcp

```go
func Serve(s *mcpsdk.Server, v *registry.View, opts ...Option) (stop func())
func Caller(f func(*mcpsdk.ServerSession) registry.Caller) Option
func OnClose(f func(registry.Caller)) Option
```

- `Serve` adds one tool per `tools.Def` and keeps the set in sync on every
  `Watch` notification, removing and re-adding a tool whose definition changed.
  `stop` ends the sync and removes the tools it added.
- Annotations: `readOnlyHint`, `idempotentHint`, `destructiveHint` and
  `openWorldHint` from `Effects`; `title` from the capability title. A
  non-`Long` capability with an `Output` schema advertises it as `outputSchema`
  and returns `structuredContent` beside the text.
- Results: the first text block is `tools.Result.Text`; notes, if any, are a
  second text block; `isError` is set on failure. A call that ran to completion is
  timed and gets the slow-call note.
- Caller: built once per MCP session from the `Caller` option; `Kind` defaults
  to `agent`, `Session` to the MCP session ID (or a generated `mcp-session-N`), `ID`
  to `agent:<session>`. The session's caller is forgotten when the session ends.
  `OnClose` runs then, for sessions that made at least one call.
- Progress: when a client sends a `progressToken`, job updates become
  `notifications/progress` with progress values 1, 2, 3, ... (monotonic, no total)
  and the stage, message and counters in the message text, for example
  `push (100/100 bytes)`. A new stage is sent at once; repeats within a stage are
  paced to one per 250ms, reading the latest from the job's status. For `Long`
  capabilities forwarding continues in the background after the handle returns;
  quick calls with a token run as jobs and forward while waiting.
- An unknown tool name is the SDK's protocol error.

## HTTP

```go
func Handler(v *registry.View, opts ...Option) http.Handler
func Caller(f func(*http.Request) registry.Caller) Option
func Info(title, version string) Option // OpenAPI info; default "capability-services", "1"
```

| Route | Behaviour |
| --- | --- |
| `POST /v1/{ref as path}` | Invoke. `storage.artifact.get` is `/v1/storage/artifact/get`; pin with `/v1/storage/artifact/get@1`. Body is the JSON input (empty means `{}`), at most 32 MiB. Returns `200 {"result": ...}` or the error envelope. |
| `POST` to a `Long` capability | `202 {"job", "status"}` with `Location: /v1/jobs/{id}`. With `Prefer: wait=N` the request waits up to N seconds first and returns `200` with the result, or the error, if the job finished. |
| `GET /v1/jobs/{id}` | `{"job", "status", "result"?}`. |
| `GET /v1/jobs/{id}/events` | Server-Sent Events: `id` is the event `Seq`, `event` is `progress` or the state name, `data` the `registry.Event` JSON. Replays from the start. |
| `DELETE /v1/jobs/{id}` | Cancel; returns the job envelope. |
| `GET /v1/` | `{"capabilities": [Descriptor...]}` for `List()`; `?all=1` for `All()`. |
| `GET /v1/health` | `{"ok", "capabilities": [{"ref", "ready", "error"?}]}`; 503 when any listed capability is not ready. |
| `GET /v1/openapi.json` | OpenAPI 3.1, generated per request: a path per current capability and a pinned path per registered version, input and output schemas, error responses, `202` for `Long` ones, `deprecated` for superseded ones, the optional `Craggy-Session` request header on every operation and the `Craggy-Superseded-By` response header on capability results. |

- `POST` under `/v1/jobs/...` invokes capabilities (`jobs.list` is
  `POST /v1/jobs/list`); only `GET` and `DELETE` there are job routes.
- A superseded capability still answers, with a `Craggy-Superseded-By` header
  naming its superseders.
- Caller: a `Caller` already in the request context (put there by the host's auth
  wrapper with `registry.WithCaller`) wins; otherwise the `Caller` option derives
  one. A `Craggy-Session` header sets `Caller.Session` only when that caller has
  none — it never overrides the host's authenticated session. A request with no
  identity at all is an anonymous caller (`Kind: "http"`), never the
  trusted-local no-caller path.
- Job routes apply the job-access rule for that caller (`View.JobFor`), for
  anonymous requests too: a headerless caller sees no jobs under the default
  rule. Hosts that want open job access install `registry.WithJobAccess`.
- The handler serves paths with or without the `/v1` prefix, so it also works
  under `http.StripPrefix("/v1", ...)`.

## One error envelope

Every projection reports a failure as `registry.ErrorEnvelope`:

```json
{"error":{"code":"unavailable","reason":"no_capacity","message":"every emulator slot is in use","retryable":true}}
```

| Code | HTTP | Retryable |
| --- | --- | --- |
| `invalid` | 400 | no |
| `denied` | 403 | no |
| `not_found` | 404 | no |
| `conflict` | 409 | no |
| `cancelled` | 499 | no |
| `unavailable` | 503 | yes |
| `internal` (and anything uncoded) | 500 | no |

`reason` appears only when the error has one. Tool and MCP results carry the same
JSON as their text payload; `jobs.*` status carries it as `error`; Starlark
reports it as `starlarkcap.Error` ([Starlark](starlark.md)).

## Working with other packages

All three import `registry`; `mcp` also imports `tools`. They expose the `jobs.*`
capabilities like any others ([jobs](jobs.md)). The
[conformance suite](conformance.md) runs one capability set through all of them
and Starlark, requiring identical results, codes, job behaviour and pagination.

## Examples

Compiled and checked by `go test`:

- [tools/example_test.go](../tools/example_test.go) `Example`: `Defs` for an
  agent view, a structured result, a name outside the view (`unknown_tool`), a
  superseded tool with its note, and a `Long` call's handle.
- [mcp/example_test.go](../mcp/example_test.go) `ExampleServe`: an in-memory MCP
  client listing tools with annotations and output schema, and calling one.
- [httpapi/example_test.go](../httpapi/example_test.go) `ExampleHandler`: success,
  `not_found`, `invalid` with its message, the superseded header, and 202 +
  `Location`.

```go
agents := r.View(registry.And(
	registry.Or(registry.Tagged("agent"), registry.Match("jobs.*")),
	registry.Not(registry.Tagged("admin"))))

res := tools.Do(ctx, agents, "storage_artifact_get", json.RawMessage(`{"id":"art-1"}`))
// res.Text == `{"id":"art-1","name":"app.apk"}`, res.Structured is the same object

res = tools.Do(ctx, agents, "admin_wipe", nil)
// {"error":{"code":"not_found","reason":"unknown_tool","message":"no tool \"admin_wipe\" in this view; call only the tools you were given","retryable":false}}

text, _ := tools.Call(ctx, agents, "jadx_decompile", json.RawMessage(`{"id":"art-1"}`))
// "jadx output"
// note: jadx.decompile is superseded by apk.decompile
```

## Semantics, errors and limits

- `tools.Defs`, and so MCP tool lists, omit unready capabilities; HTTP listings
  and health keep them with `ready: false`.
- Agents never get pinned versions as tools. HTTP clients and scripts can pin.
- A `Long` capability's tool advertises no output schema, because its result is a
  handle. Non-object outputs are never structured content.
- HTTP `Invoke` uses the request context to wait: a client that disconnects gets
  nothing more, but the job keeps running and is visible through the job routes.
- The OpenAPI document declares `Craggy-Session` as an optional header parameter
  of every operation (`components.parameters.CraggySession`) and
  `Craggy-Superseded-By` as a response header of capability results
  (`components.headers.CraggySupersededBy`, sent only when the capability is
  superseded); it also marks superseded operations `deprecated`.
- `Prefer: wait=N` accepts fractional seconds and has no upper bound of its own.

## Validation and evidence

`go test ./tools` covers definitions with subjects and examples, calls, `Long`
handles, superseded notes, caller flow, structured output, codes and reasons,
notes and elapsed time. `go test ./mcp` covers listing and annotations, long jobs
with `jobs_wait`, monotonic progress across stages and its pacing, live tool
changes, caller and `OnClose`, structured content and error envelopes, and the
notes-after-payload result shape. `go test ./httpapi` covers invocation and the
code-to-status map, the long-job flow with SSE, listings, health and OpenAPI
(including its description of the Craggy headers), the superseded and session
headers, caller options and context wrappers, and reasons
in envelopes.
