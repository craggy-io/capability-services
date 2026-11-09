# 0001: Shared jobs, dispatched long jobs and a long-poll feed

## Status
Accepted — 2026-11-09, by the user's advance acceptance of the cloud-first plan's ADRs
("Accept in advance") in the session that implemented it. Part of the Craggy cloud
hosting pattern (craggy-cloud ADR 0001).

## Context
Craggy apps run on Cloud Run with several stateless instances. Three things in this module
assumed one process:

- **The job table is in memory**, with an optional local journal. A `jobs.get` or
  `/v1/jobs/{id}` that reaches another instance answers `not_found`.
- **Long jobs run on a goroutine after the response** (202 + `Location`). Cloud Run with
  request-based billing throttles CPU once no request is in flight, so such a goroutine
  stalls.
- **The change feed** (`server.Events`) is Server-Sent Events over an in-process hub.
  Firebase Hosting buffers proxied responses, so SSE does not stream through its rewrite,
  and an in-process hub sees only its own instance's changes.

## Decision

1. **`registry.SharedJobs`**, an optional store of `JobRecord`s that every instance of a
   service shares (`ShareJobs`). Lookups that miss the local table read it; handles on
   other instances' jobs re-read it to wait, stream and cancel. Cancellation is a mark the
   holding instance polls once a second. Progress writes are coalesced to one a second per
   job. IDs are random. The registry stays standard library only; the PostgreSQL store is
   `adapters/postgres`, a separate module.
2. **`registry.DispatchLong(Dispatcher)`**: a `Long` job is recorded `queued` and handed to a
   queue (Cloud Tasks in the cloud, implemented by the host) instead of a goroutine. The
   queue delivers it to the host, which calls `RunTask` inside that request, so CPU stays
   allocated. `Claim` (queued → running, one winner) makes duplicate deliveries harmless.
   Short work keeps `Prefer: wait`, inside the calling request.
3. **`longpoll`**: an ordered change log (a `Source` with `Changes` and `Wait`) served as an
   ordinary capability that returns at once when there are changes, or waits up to 25 s
   for one. It goes through `/api` like any call: no CORS, no new origin, no streaming.
   The host's source reads shared storage (the artifact-services journal), so any instance
   answers. SSE stays for local hosts.

Local hosts change nothing: without `ShareJobs` and `DispatchLong` the registry behaves as
before.

## Alternatives considered

| Option | Why not |
| --- | --- |
| Sticky sessions so polls reach the starting instance | Cloud Run session affinity is best effort; instances scale to zero |
| CPU always allocated (instance-based billing) | Pays for idle CPU; loses the free tier at this scale |
| Pub/Sub push instead of Cloud Tasks | No per-task name for de-duplication or delay; push retries are less controllable |
| A registry-level Postgres dependency | Breaks rule 1 (registry is standard library plus jsonschema-go) |
| SSE through a separate, unproxied domain | CORS, a new origin in every CSP, and a second TLS name per app |

## Consequences

- A dispatched job's events seen from another instance are snapshots (one per read), not
  every progress step.
- Cancelling a parent does not cancel a dispatched child on another instance.
- Every state change of a shared job is a database write; a chatty job writes once a
  second at most.
