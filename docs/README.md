# Documentation

[Project](../README.md) · [Agent guide](../AGENTS.md) · [Design](design.md)

Every package has a Go implementation and tests. Each guide describes the
available API, its semantics and limits, and links to the package's
`example_test.go`, whose `// Output:` checks `go test` verifies. Operations that
call capabilities take a context first; constructors, filters and listings do not.

## Design

- [Design](design.md): the rules, the core API, naming, versions and supersession,
  pagination, jobs, readiness, groups, projections, callers and sessions as host
  responsibilities, middleware, what is deliberately left out, and open questions.

## Package guides

Core:

- [Registry](registry.md): the capability shape, typed registration, runtime
  change, versions, supersession, views as policy, readiness, groups, errors and
  descriptors.
- [Jobs](jobs.md): the job lifecycle, stage progress, cancellation, access, the
  status journal, shared jobs across instances and dispatch to a task queue,
  and the `jobs` package's `jobs.*` capabilities.
- [Long-poll](longpoll.md): an ordered change log as a capability that waits.

Projections:

- [Projections](projections.md): `tools` for in-process LLM loops, `mcp` on the
  official Go SDK, `httpapi` with OpenAPI, and the result and error shapes they
  share.
- [Starlark](starlark.md): `starlarkcap` in both directions: `Globals`, `Exec`,
  `Check`/`Analyze`, `Load` and its policy, `Watch`, `Test`, and `Origin`.
- [Typed Go clients](clientgen.md): `clientgen` turns descriptors into Go source
  for a compiled caller: the naming scheme, the schema-to-Go rules and their
  fallbacks, and how a paged capability is detected.

Verification:

- [Conformance](conformance.md): one capability set, Go- and Starlark-defined,
  through every projection.

Each package guide uses the same sections: purpose, features, mental model,
responsibilities and boundaries, public interfaces, working with other packages,
examples, semantics/errors/limits, and validation.

## Related module

`starlarkcap` bridges to `github.com/craggy-io/script-services`, the generic
Starlark runtime (dialect, values, schemas, Go-syntax type declarations, `load()`
modules and digests, static checks, the shared `.star` compiler, the test runner).
That module has its own README, agent guide and package guides; read them for
anything below the registry boundary. Capability files load from bundles (a folder with a `bundle.yaml`) through
`github.com/craggy-io/foundation-kit`'s `bundle` package.

## Branding

The two SVG wordmarks in `assets/` are unchanged copies of the Craggy brand kit's
light/dark variants, as used by artifact-services and script-services. The root
README selects the appropriate variant by display theme.
`assets/capability-services-architecture.svg` follows the artifact-services
architecture diagram's palette and type.
