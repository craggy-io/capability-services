# Jobs

[Project](../README.md) · [Documentation index](README.md) · [Design](design.md#26-long-running-jobs)

The job table lives in package `registry`; package `jobs` exposes it as
capabilities. Available Go API.

## Purpose

Give every call a status, stage progress, liveness and cancellation, without each
projection reinventing them, and let agents and scripts follow long work through
ordinary capabilities.

## Features

- Every `Invoke` and `Start` is a job; `Long` capabilities are started and
  followed rather than waited for.
- Stage progress (`Update`) with counters and a unit per stage, never a
  fabricated overall percentage.
- Heartbeats and `stalled` detection; cancellation that can never turn into
  success; child jobs cancelled with their parent.
- Event replay with per-stage coalescing; live subscription.
- Per-tag concurrency limits; per-caller access rules.
- An optional status journal that reports interrupted jobs as `lost` after a
  restart.
- Optionally, a job table shared by every instance of a service, and long jobs
  handed to a task queue that runs each inside a request
  ([shared jobs and dispatch](#shared-jobs-and-dispatch)).
- `jobs.list`, `jobs.get`, `jobs.wait`, `jobs.cancel` as capabilities, in a
  `LoadAlways` group.

## Mental model

```
queued ──► running ◄──► stalled (derived: no Progress for Heartbeat)
              │
              └──► succeeded | failed | cancelled          (only the job runner)
lost: restored from the status journal after a restart     (never resumed)
```

`Start` validates the call, creates the job, and runs the middleware chain and
handler on its own goroutine with a context that keeps the caller and view but
drops the request's cancellation. The runner is the only code that sets a terminal
state, after the whole chain returns and the result encodes. `Wait` blocks for that
state; a `Wait` context that ends leaves the job running.

## Responsibilities and boundaries

The registry tracks jobs in memory. It does not persist results beyond the
optional status journal, resume handlers, reserve physical resources or decide
what a stalled job means. Handlers honour their context and release what they
hold; the host persists outputs and audit records before a handler returns, and
coordinates any retry of `lost` work as a new call.

## Public interfaces

In `registry`:

- `(*View).Start(ctx, ref, in) (*Job, error)`, `Invoke` (Start + Wait).
- `(*Job).ID`, `Capability`, `Status`, `Wait(ctx)`, `Result`, `Events(ctx)`,
  `Cancel`.
- `JobStatus`, `JobState` (`queued`, `running`, `stalled`, `succeeded`, `failed`,
  `cancelled`, `lost`) and `JobState.Terminal`, `Event`, `Update`,
  `Update.Percent`, `JobFilter`.
- `Progress(ctx, Update)`, `JobIDFrom(ctx)`.
- `(*View).Job`, `Jobs` (view only) and `JobFor`, `JobsFor` (view plus the access
  rule for the caller in `ctx`).
- Options: `AsLong(heartbeat)`, `Limit(tag, n)`, `KeepJobs(d)`,
  `WithJobAccess(f)`, `JobDrainTimeout(d)`; `OpenJobJournal(path)`,
  `JobJournalError()`, `Close()`.

In `jobs`:

- `Capabilities() []registry.Capability`, `Group() registry.Group`, `WaitOut`.

## The jobs capabilities

| Capability | Input | Output | Notes |
| --- | --- | --- | --- |
| `jobs.list` | `state`, `capability` (name or glob), `limit`, `cursor` | `Page[JobStatus]`, oldest first | Read-only. Unknown state is `invalid`. |
| `jobs.get` | `id` | `JobStatus` | Read-only. |
| `jobs.wait` | `id`, `timeout` (seconds, at most 60; 0 returns at once) | `WaitOut{status, result?}` | A failed job is reported in the status, not as a call error, so agents poll without a busy loop. |
| `jobs.cancel` | `id` | `JobStatus` | Idempotent. The returned status may still be running until the handler notices. |

Each acts only on jobs whose capability is in the caller's view and which the
access rule allows; anything else is `not_found`. They need a view on their
context, which every registry call provides. `jobs.Group()` is
`{Name: "jobs", LoadAlways: true}`: an agent that starts a long capability needs the
controls to follow its handle.

## Shared jobs and dispatch

A service with several instances (Cloud Run) needs two things the in-memory
table cannot give: a poll that reaches another instance must find the job, and
a long job must keep CPU after the response that started it (Cloud Run
throttles CPU between requests). ADR [0001](decisions/0001-shared-jobs-and-dispatch.md).

- `ShareJobs(store SharedJobs)` writes every job's `JobRecord` to `store`:
  state changes always, a running job's progress at most once a second. Job IDs
  become random (`job-` and 24 hex digits). `Job`, `JobFor`, `Jobs`, `JobsFor`,
  and so `jobs.*` and `/v1/jobs/{id}`, find jobs other instances hold. Such a
  handle re-reads the store: `Status` and `Result` read it once, `Wait` polls
  every 250 ms until the job is terminal, `Events` emits one event per read
  that moved its `Seq` (intermediate events are not replayed), and `Cancel`
  asks the holding instance through the store, which checks every second.
  Retention is the store's: `List` drops jobs ended longer than `KeepJobs`
  ago. `JobStoreError` reports the first failed write. It cannot be combined
  with `OpenJobJournal`.
- `DispatchLong(d Dispatcher)` makes `Start` of a `Long` capability record the
  job `queued` in the store and hand a `Task` (job ID, exact capability, input,
  caller, parent) to `d` instead of running it. Quick capabilities still run in
  the calling request. The host serves the queue's deliveries and calls
  `RunTask(ctx, task)`, which claims the job (exactly one delivery wins; the
  rest get `ErrTaskNotQueued`, to be acknowledged) and runs it to its end in
  that request, under the registry's full view with the recorded caller; the
  request's end cancels it. Start already applied the starting view. A refused
  dispatch fails the `Start` with `unavailable`. It needs `ShareJobs`.
- A dispatched job's parent link is recorded but cancelling the parent does not
  cancel it across instances.
- `SharedJobs` implementations pass `jobstoretest.Run`; `jobstoretest.Memory`
  is an in-process one. `adapters/postgres` (its own module, pgx) is the
  PostgreSQL one: one row per job, guarded so an older or non-final record
  never replaces a newer or final one; `Claim` is a conditional update.

## Working with other packages

Projections expose `Long` capabilities as handles: HTTP 202 + `Location` and the
`/v1/jobs/{id}` routes, a `JobEnvelope` from `tools`/MCP with
`notifications/progress` for clients that send a progress token, and
`jobs.start` and `ctx.start` in Starlark. See [projections](projections.md) and
[Starlark](starlark.md). The [conformance suite](conformance.md) requires every
projection to agree on job behaviour.

## Examples

From [registry/example_test.go](../registry/example_test.go)
(`ExampleView_Start`, `ExampleRegistry_Close`) and
[jobs/example_test.go](../jobs/example_test.go) (`Example`):

```go
build := registry.Func("demo.build", "Build something slowly",
	func(ctx context.Context, _ struct{}) (string, error) {
		for i := int64(1); i <= 3; i++ {
			registry.Progress(ctx, registry.Update{Stage: "compile", Done: i, Total: 3, Unit: "files"})
		}
		registry.Progress(ctx, registry.Update{Stage: "publish", Message: "writing output"})
		return "built", nil
	}, registry.AsLong(time.Minute))

job, err := r.Start(ctx, "demo.build", nil)
out, err := job.Wait(ctx) // "built"
for ev := range job.Events(ctx) { ... }
// queued / running / running compile 3/3 files / running publish (percent -1) / succeeded
```

Following a job through the capabilities, with the default access rule:

```go
alice := registry.WithCaller(ctx, registry.Caller{ID: "agent:a", Kind: "agent", Session: "s-alice"})
job, _ := r.Start(alice, "demo.build", nil)
raw, _ := r.Invoke(alice, "jobs.wait", json.RawMessage(`{"id":"`+job.ID()+`","timeout":5}`))
var w jobs.WaitOut // w.Status.State == "succeeded", w.Result == {"artifact":"out.bin"}

bob := registry.WithCaller(ctx, registry.Caller{ID: "agent:b", Kind: "agent", Session: "s-bob"})
_, err := r.Invoke(bob, "jobs.get", json.RawMessage(`{"id":"`+job.ID()+`"}`)) // not_found
```

## Semantics, errors and limits

- **Progress.** `Progress` outside a job does nothing. An update with a negative
  counter, or `Done` above a known `Total`, is ignored, as is any update after a
  terminal state. Each update is a heartbeat and moves a queued job to running.
- **Stalled** is derived in `Status`: a running job whose last heartbeat is older
  than its `Heartbeat` (default 30s) reports `stalled` until the next update. It is
  a warning, not a kill.
- **Terminal states.** A handler error with code `cancelled` (including context
  errors) ends the job `cancelled`; any other error `failed`, with its code and
  reason in `JobStatus.Error`. Once `Cancel` was requested the job ends `cancelled`
  even if the handler returns success. `Wait` returns the stored code and reason.
- **Cancellation** cancels the handler's context; only `Cancel` (directly, via
  `jobs.cancel`, `DELETE /v1/jobs/{id}`, a cancelled parent, `starlarkcap.Exec`
  cleanup, or `Close`) does. A job started from a handler is that job's child
  (`JobStatus.Parent`); a parent that finishes normally leaves its children
  running.
- **Queueing.** `Limit(tag, n)` holds calls to capabilities carrying `tag` in
  `queued` until a slot frees; a job cancelled while queued ends `cancelled`.
  Limits of 0 or less are ignored.
- **Events.** `Events` replays history from the start, then streams live events,
  and closes after the terminal event or when its context ends. Replay keeps only
  the latest of consecutive progress updates for one stage, so `Seq` can skip.
  Live subscribers that fall behind miss events; `Status` stays authoritative.
- **Access.** The default rule admits a caller whose `Session` equals the job
  caller's, the same `ID` and `Session`, or a caller named (by `Session` or `ID`) in
  the job caller's `Parent` chain. With no caller in the context, `JobFor`/`JobsFor`
  trust local code and apply only the view. `WithJobAccess` replaces the rule.
- **Retention.** Finished jobs are purged after `KeepJobs` (default 1h; 0 or less
  keeps them for the registry's lifetime). Job IDs are `job-N`, from a
  process-wide counter.
- **Journal.** `OpenJobJournal(path)` must be called before any job starts. It
  appends one JSON record per job event and compacts the file to one record per job
  when it opens. Restored terminal jobs keep their results; restored unfinished
  jobs become `lost` with `internal`/`process_restarted`. After a write failure the
  journal stops and `JobJournalError` reports it; jobs keep running.
- **Close** cancels unfinished jobs and waits for their runners, up to
  `JobDrainTimeout` (default 10s; 0 or less does not wait). From the moment it
  starts, `Start` and `Invoke` fail with `unavailable`/`closed`. A job is inserted
  and its runner counted in one step under the lock `Close` takes, so a `Start`
  racing `Close` either fails or creates a job `Close` cancels and waits for.

## Validation and evidence

`go test ./registry` covers the lifecycle and replay
(`TestJobLifecycleAndEventsReplay`, `TestReplayCoalescesSameStageProgress`),
stalling and recovery, cancellation and late progress, publication failure
failing the job, jobs outliving their caller's context, invalid progress, the
per-tag queue, readiness, purging, child cancellation, `Close` draining and its
bound (`TestCloseWaitsForJobRunners`, `TestCloseDrainIsBounded`), starts after and
racing `Close` (`TestStartAfterCloseFails`, `TestStartRacingCloseLeavesNoRunningJob`)
and the journal
(`TestJobJournalRestoresRunningJobAsLost`,
`TestJobJournalRestoresCompletedResultAndAdvancesIDs`). `go test ./jobs` covers the
capabilities, access rules, the group and reason preservation through
`jobs.wait`.
