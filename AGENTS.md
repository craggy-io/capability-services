# Capability Services: coding-agent integration guide

Use this guide when editing this repository or integrating its Go module,
`github.com/craggy-io/capability-services`, into a host or a domain adapter.
Repository development rules below apply to work here. Consumers should explicitly
provide this guide to their coding agent; installing a dependency does not imply
its AGENTS.md is read. Start with the [README quick start](README.md#quick-start),
the [design](docs/design.md) (its section 1 is the rule set this guide
summarises) and the executable package examples (`*/example_test.go`), which
`go test` compiles and checks.

## What belongs here

- `registry/`: the one capability shape, registration, snapshots, versions,
  supersession, views/filters, middleware, jobs, readiness, groups, error codes
  and reasons.
- `jobs/`: the `jobs.*` capabilities over the registry's job table.
- Projections, each in its own package: `tools/`, `mcp/` (built on `tools`),
  `httpapi/`, `starlarkcap/`.
- `conformance/`: the suite every projection passes; `examples/`: runnable hosts.

## What does not belong here

- Knowledge of any domain: storage, classification, data, environments, security
  tooling. Capabilities are registered by adapters beside the domain libraries,
  never implemented here.
- Session stores, authentication, audit/lineage records, quotas, physical resource
  admission. These are host application responsibilities; the registry only
  carries `Caller` identity and the active job ID (`JobIDFrom`).
- Generic Starlark machinery (dialect, value conversion, schemas, `load()`, static
  checks, the test runner). That is `github.com/craggy-io/script-services`; add a
  generic seam there and keep the registry-specific part in `starlarkcap`.

## Choose the owning package

| Need | Package and contract |
| --- | --- |
| Register a capability, build views, invoke, version, supersede, read listings | [registry](docs/registry.md) |
| Run a call as a job, report progress, follow, wait for or cancel it | [registry jobs](docs/jobs.md) and the [`jobs` capabilities](docs/jobs.md#the-jobs-capabilities) |
| Offer a view's capabilities to a model you call yourself | [tools](docs/projections.md#tools-in-process-tool-calling-loops) |
| Serve a view over MCP | [mcp](docs/projections.md#mcp) |
| Serve a view over HTTP with OpenAPI | [httpapi](docs/projections.md#http) |
| Let scripts call capabilities, define them, hot-reload or test them | [starlarkcap](docs/starlark.md) |
| Prove a new projection behaves like the others | [conformance](docs/conformance.md) |

## Import rules

1. **Core `registry`: standard library plus `github.com/google/jsonschema-go`
   only.** It never imports a projection (`jobs`, `tools`, `mcp`, `httpapi`,
   `starlarkcap`) or any domain module.
2. **Projections import `registry`, never the reverse**, and never each other,
   except `mcp`, which is built on `tools`.
3. **Only `starlarkcap` imports `github.com/craggy-io/script-services`.** No
   other package in this module does, the example host included: it uses the
   aliases `starlarkcap.Position`, `Limits` and `Frame` instead. Hosts
   elsewhere that use `Source.Modules`, `LoadOptions.Types` or `Program.Modules`
   import script-services' `modules` and `types` packages themselves; that is the
   host's choice, not a second bridge.
4. **Domain cores never import this module.** Only their optional `*cap` adapter
   packages do; an adapter imports its own domain package and `registry`, and
   nothing else from another domain. Do not make changes here that would require a
   domain core to import this module.
5. **Go calls Go.** In-process Go code calls library APIs directly; the registry
   is only for crossing a boundary (an agent, a script, a remote client, a person).
6. **This module never imports the host.** No library imports the application that
   wires it.

Re-check from this module root (each command should print nothing unless noted).
Import audit 2026-11-04: rules 1-3 hold for every package in this module,
`examples/host` included; rule 4 is checked in each domain module.

```sh
# 1. registry: no non-stdlib import except jsonschema-go, no import of this module
go list -f '{{range .Imports}}{{println .}}{{end}}' ./registry | grep '\.' | grep -v '^github.com/google/jsonschema-go/'
go list -f '{{range .Imports}}{{println .}}{{end}}' ./registry | grep 'craggy-io/capability-services'
# 2. this module's packages each projection imports: registry everywhere, plus tools for mcp
for p in jobs tools mcp httpapi starlarkcap; do echo "$p:" $(go list -f '{{range .Imports}}{{println .}}{{end}}' ./$p | grep 'craggy-io/capability-services/' | sed 's#.*/##'); done
# 3. script-services importers among all packages, examples included: prints only .../starlarkcap
go list -f '{{.ImportPath}}:{{range .Imports}} {{.}}{{end}}' ./... | grep 'craggy-io/script-services' | cut -d: -f1
# 4. from a domain module's root: prints only paths of *cap adapter packages
go list -f '{{.ImportPath}}:{{range .Imports}} {{.}}{{end}}' ./... | grep 'craggy-io/capability-services' | cut -d: -f1
```

```powershell
# 1.
go list -f '{{range .Imports}}{{println .}}{{end}}' ./registry | Select-String '\.' | Select-String -NotMatch '^github.com/google/jsonschema-go/'
go list -f '{{range .Imports}}{{println .}}{{end}}' ./registry | Select-String 'craggy-io/capability-services'
# 2.
foreach ($p in 'jobs','tools','mcp','httpapi','starlarkcap') { "${p}: " + ((go list -f '{{range .Imports}}{{println .}}{{end}}' "./$p" | Select-String 'craggy-io/capability-services/' | ForEach-Object { ($_ -split '/')[-1] }) -join ' ') }
# 3.
go list -f '{{.ImportPath}}:{{range .Imports}} {{.}}{{end}}' ./... | Select-String 'craggy-io/script-services' | ForEach-Object { ($_ -split ':')[0] }
# 4. (domain module root)
go list -f '{{.ImportPath}}:{{range .Imports}} {{.}}{{end}}' ./... | Select-String 'craggy-io/capability-services' | ForEach-Object { ($_ -split ':')[0] }
```

## Construct and close

Build in this order:

1. `registry.New(opts...)`. Registry options: `Use(middleware...)` (first is
   outermost), `Limit(tag, n)`, `CheckEvery(d)`, `CheckTimeout(d)`, `KeepJobs(d)`,
   `WithJobAccess(f)`, `ValidateOutput(on)`, `JobDrainTimeout(d)`. Capability
   options passed to `New` are ignored, and the reverse.
2. Optionally `r.OpenJobJournal(path)`, before any job starts.
3. `r.Add(...)` each adapter's capabilities and `jobs.Capabilities()`;
   `r.AddGroup(...)` their groups, including `jobs.Group()`. `Add` and `Replace` are
   all-or-nothing; handle their errors.
4. Load script capabilities with `starlarkcap.Load` or `starlarkcap.Watch`, with
   the host's `LoadOptions` (supersession policy, limits, print sink, types).
5. Build views with `r.View(filter)`.
6. Start projections: `mcp.Serve` (returns `stop`), `httpapi.Handler`, and
   `tools.Defs`/`tools.Do` per agent turn.

Close in reverse: stop accepting requests (HTTP server shutdown, MCP `stop()`,
which removes the tools it added), `Watcher.Close()`, then `r.Close()` last.
`Close` stops the readiness loop, cancels every unfinished job and waits for job
runners to return, bounded by `JobDrainTimeout` (default 10s; 0 or less does not
wait). From the moment it starts, `Start` and `Invoke` fail with `unavailable`
(reason `closed`), so a call racing `Close` either fails or is cancelled and
waited for. A handler that ignores cancellation past the bound is abandoned and
may still finish, and journal, later. `Close` is idempotent; do not use the
registry after it. `Watcher.Close` may be called from its own `OnResult`.

## Views as policy

- A view is the registry filtered by name (`Match`), tag (`Tagged`), effect
  (`ReadOnlyOnly`) or any `Filter`, combined with `And`/`Or`/`Not`. Build one view
  per audience; never register a capability twice to restrict it. `Select` filters
  a slice before `Add` when a host wants only part of an adapter at all.
- Filtering and enforcement are the same thing. A ref outside the view is
  `not_found`, whether it exists elsewhere or not.
- Versions, supersession and readiness are computed registry-wide, then filtered.
  `List` is current, non-superseded entries (unready ones included, with
  `Ready: false`); `All` adds old versions and superseded entries; `Get` reaches
  both.
- The registry sets `ViewFrom(ctx)` on every handler context. Nested calls are
  clamped to it: even `r.Invoke` from inside a handler intersects with the view the
  outer call arrived on. Hosts hand a view to context-only projections with
  `registry.WithView` (Starlark `Globals`, `Exec`).
- Caller identity is separate from the view. Projections fill in `Caller`
  (`ID`, `Kind`, `Session`, `Parent`, `Attrs`); the host maps it to whatever session
  concept it owns, in middleware or in adapter dependencies. See
  [callers and sessions](docs/design.md#5-callers-sessions-and-middleware).

## Jobs

- Every `Invoke` and `Start` creates a job; `Invoke` is `Start` plus `Wait`.
  Projections start `Long` capabilities and return a handle instead of waiting.
- Report progress with `registry.Progress(ctx, Update{...})`. Counters describe
  one named stage in one unit; never add unlike units, average stage percentages
  or treat `Done == Total` as job completion. Invalid updates and updates after a
  terminal state are ignored.
- The job context drops the request's cancellation; only `Job.Cancel` (or
  `jobs.cancel`, `DELETE /v1/jobs/{id}`, a cancelled parent job, `Close`) stops a
  job. Handlers must honour `ctx`. A `Long` job with no progress for its
  `Heartbeat` (default 30s) reports `stalled` until its next update; that is a
  warning, never a kill.
- `Limit(tag, n)` is application backpressure on concurrent calls, not physical
  capacity accounting; a queued call waits in state `queued`.
- Job access: `View.JobFor`/`JobsFor` (used by HTTP, MCP and the `jobs.*`
  capabilities) apply the access rule for the caller in `ctx`; by default a caller
  sees jobs from its own `Session` and from sessions it started (via `Parent`).
  Replace the rule with `WithJobAccess`. `View.Job`/`Jobs` trust local Go code and
  apply only the view.
- Durability: the job table is in memory; finished jobs stay for `KeepJobs`
  (default 1h). `OpenJobJournal` appends a status record per event and, on the next
  start, restores terminal jobs (results stay waitable) and marks non-terminal ones
  `lost` with reason `process_restarted`. It never resumes a handler. Expose
  `JobJournalError()` as a health failure. Anything that must survive a restart
  (outputs, audit records) is the host's to persist before the handler returns.

See [jobs](docs/jobs.md).

## Errors: codes and reasons

- Codes are a closed set: `invalid` (400), `not_found` (404), `denied` (403),
  `conflict` (409), `unavailable` (503), `cancelled` (499), `internal` (500).
  `Code.Retryable` is true only for `unavailable`.
- A reason is an optional lower_snake_case refinement within a code
  (`ValidReason`). Well-known: `no_capacity`, `backend_down`, `not_ready` (all
  `unavailable`), and `closed` (`unavailable`, a call on a closed registry). This
  module also emits `unknown_tool` (`not_found`, from `tools.Do`) and
  `process_restarted` (`internal`, restored `lost` jobs). Adapters may add their
  own in the same form.
- Build errors with `Errorf`/`ReasonErrorf`; classify foreign errors with
  `WrapCode` (leaves an already-coded error alone) or `WrapReason` (always
  applies). `ReasonErrorf`/`WrapReason` panic on an invalid reason. Each adapter
  translates its sentinels in one `registry.Codes` table whose sentinels do not
  overlap: an error matching sentinels with different codes wraps as `internal`.
- `CodeOf`, `ReasonOf` and `Info` read the outermost `*registry.Error`, so code and
  reason survive `%w` wrapping. Context cancellation and deadlines map to
  `cancelled`; anything uncoded is `internal`.
- Every projection carries the same `ErrorEnvelope`
  (`{"error":{"code","reason"?,"message","retryable"}}`): HTTP bodies, tool and
  MCP results, `jobs.*` status, `starlarkcap.Error`.
- A failure inside a script-defined capability is `internal` (the script is at
  fault, not its caller), with the nested capability's code in the message.
  `starlarkcap.Exec`, by contrast, reports an unhandled capability failure with
  that capability's code and reason.

## Script loading policy

- `starlarkcap.Load(ctx, r, prefix, Source, LoadOptions)` checks the file, runs
  it, and installs its capabilities with `r.Replace(prefix, ...)`. Reloading is
  calling `Load` again; unloading is `r.Replace(prefix)`. A failed load leaves the
  registry unchanged.
- One file owns one prefix: `Load` fails with `conflict`, naming the other file,
  when the `Replace` would remove capabilities another file loaded (`Origin.Kind`
  `"script"`, a different `Origin.Ref`). Unload that file first to move names
  between files. Serialise concurrent `Load`s of overlapping prefixes.
- Every capability a file defines must be named under its prefix (else
  `invalid`). It may supersede refs under the prefix freely and refs outside it
  only when `LoadOptions.MaySupersede` admits them (else `denied`). The filter sees
  the registered target capability when there is one, so tag policies work
  (`registry.Not(registry.Tagged("admin"))`); a bare ref supersedes every version,
  so every registered version must pass. `nil` admits nothing outside the prefix.
  Choose the narrowest policy that serves the deployment.
- Script capabilities go through the same view filters as Go ones and call other
  capabilities through the caller's view; a script never widens its caller's
  access.
- Installed capabilities carry `Origin{Kind: "script", Ref: filename, Digest}`;
  `Loaded.Digest` and `Loaded.Modules` content-address the file and every module it
  loaded, and the types when `LoadOptions.TypesDigest` is set (use
  `starlarkcap.TypesDigest(src)` beside `LoadOptions.Types`). Record them in audit
  if runs must be reproducible.
- `Watch` maps each top-level `*.star` file (not `*_test.star`) to the prefix
  named by its stem. A stem claims its whole prefix: a `jadx.star` replaces every
  `jadx.*` capability, Go-registered ones included. Point `Watch` at a directory
  whose stems are reserved for scripts. Stems must be valid capability names and
  contain no `.` (one file, one prefix; a dotted stem is reported as `invalid`).
  An optional `types.go` in the root types every file and reloads them all when
  it changes.
- Give `Watch` a directory as `WatchOptions.Dir` (opened as an `os.Root`, so
  symbolic links cannot leave it) or an `fs.FS` as `Root` (an `embed.FS`, or
  `os.OpenRoot(dir)`'s `FS()`); never `os.DirFS`, which follows links out.
- Limits: each script-defined handler and check runs on a fresh thread under
  `LoadOptions.Limits` (zero means `starlarkcap.DefaultLimits`: 5,000,000 steps,
  10 minutes). `Exec` takes its `Limits` as given: the zero value is unbounded.
  The wall-clock limit also bounds the capability calls a script is blocked in
  (`Exec`'s calls, a handler's `ctx.call`), and the waited-on job is cancelled.

See [Starlark](docs/starlark.md).

## Developing and verifying in this repository

- Preserve concurrent edits and reread before patching. Use `rg` for searches.
- Check existing helpers before adding boilerplate. On the second occurrence of a
  pattern, extract or widen a small shared helper at the narrowest useful scope.
- After changing a data model (`Capability`, `Descriptor`, `JobStatus`, `Update`,
  `ErrorInfo`, `tools.Result`, `starlarkcap.Loaded`), trace every reader,
  projection, JSON tag, hand-written schema (`jobs` status schema, OpenAPI) and
  downstream consumer; silent field omissions are bugs.
- A projection change must keep the [conformance suite](docs/conformance.md)
  passing for both the Go and the Starlark capability sets; a new projection must
  join it.
- Fully remove obsolete code and call sites rather than adding deprecation markers,
  compatibility shims, or commented-out remnants. Coordinate cross-module API
  removal (script-services, domain adapters, hosts) with the user.
- Keep examples in `example_test.go` with `// Output:` checks and update the guides
  that quote them in the same change.
- Verify fixes by running the affected code and existing tests. If coverage is
  missing, add and run a minimal regression test or reproduction. Report what ran
  and any limits; do not equate an unrun check with a pass.

From this module root, inside the Go workspace described in the
[README](README.md#development), using the Go version specified in
[go.mod](go.mod):

```sh
go build ./... && go vet ./... && go test ./... -count=1
go test -race ./...
go test -tags craggy_debug ./...
```

Run focused package tests while iterating and relevant broader checks for the
change. After changing an exported API of script-services, run this module's tests
in the same workspace. In a host or adapter repository, run that repository's own
tests against its actual registry construction, views and policies; these module
commands do not replace them. This guide lists verification commands, not a claim
that a particular checkout has passed them.
