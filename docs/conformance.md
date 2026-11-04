# Conformance

[Project](../README.md) · [Documentation index](README.md) · [Design](design.md#7-build-order)

Package `conformance` (formerly the registry test package). Available Go API.

## Purpose

Prove that every projection is a mechanical translation of the registry: one
fixed capability set, run through `Invoke`, `tools`, HTTP, MCP and Starlark, must
give identical results, error codes, job behaviour and pagination. Every new
projection has to pass it.

## Features

- A standard fake capability set (`Capabilities`, `Items`) and a registry built
  from it plus the `jobs.*` capabilities (`NewRegistry`).
- The same set written in Starlark (`ScriptFile`, installed by `LoadScript` under
  `star.*`), so script-defined capabilities are held to the same contract as
  Go-defined ones.
- `Suite(t)`, which runs both sets through every projection and checks script
  `Origin` in the HTTP listing.

## Mental model

| Go (`test.*`) | Starlark (`star.*`) | Exercises |
| --- | --- | --- |
| `test.echo` | `star.echo` | a plain read-only call |
| `test.add` | `star.add` | typed input and output |
| `test.fail` (`conflict: boom`) | `star.fail` (calls `test.fail`) | error envelopes; the script's failure is `internal` with `conflict: boom` in its message |
| `test.deny` (`denied`) | `star.deny` (calls `test.deny`) | the `denied` code and its HTTP 403 mapping |
| `test.full` (`unavailable`/`no_capacity`) | `star.full` (calls `test.full`) | reason and `retryable` in every envelope |
| `test.admin` (tagged `admin`) | `star.admin` | view enforcement: invisible and `not_found` through a `Not(Tagged("admin"))` view |
| `test.old` / `test.new` (supersedes `test.old`) | `star.old` / `star.new` | superseded entries leave listings, still answer, and carry the note/header |
| `test.ver` (@1 and @2) | `star.ver` (@1 and @2) | pinned refs resolve to the named version everywhere |
| `test.items.list` | `star.items.list` (calls `test.items.list`) | `Page` and cursors over `Items` |
| `test.slow` (`Long`, staged progress) | `star.slow` (`Long`, `ctx.start`s `test.slow`) | job handles followed with `jobs.wait`, decoded status parity |
| `test.unready` (check fails) | `star.unready` (script `check`) | readiness: hidden from tools, `unavailable` on call |

For each set the suite takes the reference error from `Invoke` and requires every
projection to report that same envelope (HTTP body, tool and MCP results) or, in
Starlark, an error containing the same coded text.

## Responsibilities and boundaries

The suite tests this module's projections against an in-process registry, with
`httptest` for HTTP and the SDK's in-memory transports for MCP. It does not test a
host's authentication, session mapping or audit; hosts and adapters test those in
their own repositories.

## Public interfaces

- `Suite(t *testing.T)`
- `Capabilities() []registry.Capability` (without the job capabilities)
- `NewRegistry() (*registry.Registry, error)` (the set plus `jobs.Capabilities()`)
- `LoadScript(ctx, r) (starlarkcap.Loaded, error)` (installs `ScriptFile` under
  `star`, as `conformance.star`)
- `ScriptFile` (the Starlark source), `Items` (the data behind `items.list`)

## Working with other packages

`conformance` imports every package in the module: `registry`, `jobs`, `tools`,
`mcp`, `httpapi` and `starlarkcap`. Other packages' tests and examples may reuse
`Capabilities` and `NewRegistry` as a fixture. A new projection adds a subtest to
`projections` in [suite.go](../conformance/suite.go) for both sets.

## Examples

[conformance_test.go](../conformance/conformance_test.go) is the whole test:

```go
func TestConformanceSuite(t *testing.T) {
	conformance.Suite(t)
}
```

[example_test.go](../conformance/example_test.go) `ExampleLoadScript` shows the two
sets agreeing on results while a script failure surfaces as `internal`:

```go
r, _ := conformance.NewRegistry()
defer r.Close()
loaded, _ := conformance.LoadScript(ctx, r)
// loaded.Capabilities: star.echo@1 star.add@1 star.fail@1 star.deny@1 star.full@1 star.admin@1 star.old@1 star.new@1 star.ver@1 star.ver@2 star.items.list@1 star.slow@1 star.unready@1

goOut, _ := r.Invoke(ctx, "test.add", json.RawMessage(`{"a":19,"b":23}`))   // {"sum":42}
starOut, _ := r.Invoke(ctx, "star.add", json.RawMessage(`{"a":19,"b":23}`)) // {"sum":42}

_, goErr := r.Invoke(ctx, "test.fail", json.RawMessage(`{"text":"x"}`))   // conflict
_, starErr := r.Invoke(ctx, "star.fail", json.RawMessage(`{"text":"x"}`)) // internal, message contains "conflict: boom"
```

## Semantics, errors and limits

- Equality is on canonical JSON. `Long` calls return handles in `tools`, HTTP
  (202) and MCP, and the suite follows them with the `jobs.wait` capability
  through the same projection; in Starlark (`Globals` executed directly) the long
  call blocks and returns its result.
- Pagination is checked by following `next` until it is empty and requiring each
  item exactly once.
- Readiness is checked through `Invoke` (`unavailable`) and through `tools.Defs`,
  which must leave both unready capabilities (`test_unready` and `star_unready`)
  out.
- The suite registers no output validation of its own; run it under
  `-tags craggy_debug` to validate every output against its schema too.

## Validation and evidence

`go test ./conformance` runs `Suite` and `ExampleLoadScript`. Because both sets go
through every projection, a change to `registry`, `jobs`, `tools`, `mcp`,
`httpapi` or `starlarkcap` that alters a result, a code, job behaviour or
pagination in one projection only fails here.
