# Starlark

[Project](../README.md) · [Documentation index](README.md) · [Design](design.md#starlark-starlarkcap)

Package `starlarkcap`. Available Go API. It is the only package in this module
that imports `github.com/craggy-io/script-services`, the generic Starlark runtime.

## Purpose

Connect scripts and the registry in both directions: scripts **call**
capabilities (`Globals`, `Exec`, checked statically by `Check`), and scripts
**define** capabilities (capability files in bundles: `Load`, kept current by
`Watch`, tested by `Test`).

## Features

- Capabilities as nested built-ins (`storage.artifact.get(id = "…")`), `call()`
  for pinned and superseded refs, and `jobs.start()` for concurrent work.
- A script host (`Exec`) with step and wall-clock limits, `load()` modules,
  content digests, print and per-call hooks with source positions, coded errors
  with backtraces, and cleanup of jobs a failed script started.
- Static checks against a view (`Check`, `Analyze`, `AnalyzeWith`, `Env`) with
  nearest-name suggestions, run before anything executes.
- Capability files, read only from bundles with a `bundle.yaml` (the
  `capability.script@1` kind, `Script`): the contribution id is the prefix, one
  file per prefix, the host's supersession policy, check before anything runs,
  schemas from `schema.*`, field dicts, raw JSON Schema or declared Go-syntax
  types, readiness checks, and `Origin` with a digest of the file, its modules and
  its types.
- Loading through foundation-kit's `bundle.Load` (`Load`), hot reload through
  `bundle.Live` (`Watch`),
  and Starlark tests declared in the same bundle (`Test`).

## Mental model

```
             Globals / Exec / Check                  Load / Watch / Test
 script ───────────────────────────► registry ◄─────────────────────────── capability file
         calls through ViewFrom(ctx)            Replace(prefix, caps...)

 bundle.yaml ─► bundle.Load / bundle.Live ─► Script compiler (starcompile) ─► *File ─► install
```

Everything generic comes from script-services: the dialect (Starlark plus
`set()`, `while`, top-level control flow and global reassignment; no recursion),
value conversion between Starlark, Go and JSON, the `schema` module and
Go-syntax type declarations, `load()` resolution with content digests, static
checks, the shared `.star` compiler (`starcompile`) and the test runner
(`startest`) with its `assert` module. Bundles, their manifests, reloading and
installing are foundation-kit's `bundle` package. Read that module's guides
for those rules. `starlarkcap` supplies the registry side: the built-ins, the
`capability()` function, call hooks, job tracking and error mapping.

## Responsibilities and boundaries

`starlarkcap` never widens access: every call from a script goes through the view
in its context, and every call from a script-defined capability goes through the
caller's view. The host chooses that view, the limits, the supersession policy,
where modules come from, and what extra built-ins scripts see. Persisting runs,
artifacts and audit records is the host's.

## Public interfaces

- `Globals(ctx) starlark.StringDict`
- `Exec(ctx, Program, Limits, Hooks) (Result, error)`; `Program{Filename, Source,
  Predeclared, Input, Entry, Modules}`; `Result{Value, Steps, Digest, Modules}`;
  `Hooks{Print, Call}`; `CallEvent`; `Error`; aliases `Limits`, `Position`,
  `Frame`; `DefaultLimits`.
- `Check(v, filename, src, extra...) []Diagnostic`, `Analyze(...) Analysis`,
  `AnalyzeWith(v, filename, src, CheckOptions{Extra, Modules}) Analysis`,
  `Env(v) check.Env`; `Analysis{Diagnostics, Uses, Loads}`.
- `Script` (`kind.Kind[*File]`, `capability.script@1`, global); `*File`;
  `CodePrefix`, `CodeTypes`.
- `Load(ctx, r, []bundle.Source, LoadOptions) ([]Loaded, error)`;
  `LoadOptions{Limits, Print, Types, TypesDigest, MaySupersede}`;
  `TypesDigest(src) string`; `Loaded{Prefix, Ref, Digest, Modules, Capabilities,
  Diagnostics}`.
- `Watch(ctx, r, WatchOptions{Sources, Interval, Load, OnResult, OnError})
  (*Watcher, error)`; `(*Watcher).Close()`.
- `Test(t, r, fsys, LoadOptions)`.

Types from other modules that appear in this API: foundation-kit's
`bundle.Source` (`bundle.Dir(dir)` for a folder, `bundle.Embedded(name, fsys)` for
an `embed.FS` or `fstest.MapFS`), `bundle.Catalog`, `bundle.Diagnostic`,
`bundle.AnyCompiler` and `bundle.Ref`; script-services' `*modules.Loader` and
`modules.Source` (for `Exec` and `AnalyzeWith`), `modules.Ref`, `*types.Package`
(from `types.Parse`), `check.Diagnostic`, `check.Env`, `script.Limits`,
`script.Position`. `Limits`, `Position` and `Frame` are aliased here, so a host
needs script-services' `modules` and `types` packages only for `Program.Modules`,
`CheckOptions.Modules` and `LoadOptions.Types`.

## Globals: calling capabilities

`Globals(ctx)` reads `registry.ViewFrom(ctx)` (set it with `registry.WithView`)
and returns:

- one namespace per top-level name segment of `v.List()`, nested by dots, with a
  built-in at each capability: keyword arguments (or one positional dict) become
  the input object, and the JSON result comes back as Starlark values;
- `call(ref, **kwargs)`, which reaches pinned versions (`"x.y@1"`) and superseded
  entries in the view's `All()`;
- `jobs.start(ref, **kwargs)`, returning a job value with `.id`, `.status()`,
  `.wait(timeout_seconds = N)` (a dict `{status, result?}`, mirroring
  `jobs.wait`) and `.cancel()`. When the view lists `jobs.*` capabilities they sit
  in the same `jobs` namespace.

`Globals` takes a snapshot when it is called, so one script run sees one
consistent set of names: the namespaces come from `List()`, and a `call()` or
`jobs.start()` ref must resolve in `All()` as it was then (a bare name must have
been registered, a pinned ref that exact version). A capability registered later
is `not_found` to that run. The call itself goes to the live view: the version
that runs is the one the ref names at call time, and a capability removed since
fails with `not_found`.

Calls block until the job finishes, even for `Long` capabilities; waiting uses no
steps. A failed call is a Starlark error whose text is the registry error
(`<code>: <message>`). Calling a superseded capability prints
`warning: x is superseded by y` through the print hook. With no view in the
context, every built-in fails with `unavailable`.

## Exec: running a script

`Exec(ctx, p, lim, hooks)` runs `p.Source` against `Globals(ctx)`, the host's
`p.Predeclared` built-ins and a frozen `input`: the module body top to bottom,
then the entry function (`run` by default) as `run(input)`, or `run()` if it takes
no parameters. `Result.Value` is the entry function's return value as JSON (`null`
without one); `Result.Digest` is the program's content address
(`modules.Digest` over the source and each direct `load()` dependency, the spec
as written paired with that module's digest), and `Result.Modules` lists every
module loaded through `p.Modules`, transitively.

- `Hooks.Print(pos, msg)` receives `print()` output and superseded warnings.
  `Hooks.Call(CallEvent)` reports each capability call twice: started (with its
  job ID) and settled (with `Err` on failure and `Elapsed`); a call the registry
  rejects before creating a job reports only the settled event, and a
  `jobs.start` only the started one (its job may outlive the script). Hooks run
  on the script's goroutine.
- Every job the script starts is tracked. If the script fails or its context
  ends, those still running are cancelled. A script that finishes cleanly leaves
  jobs it started with `jobs.start` running. When `Exec` itself runs inside a
  registry job, cancelling that job cancels the script's jobs too.
- `lim` bounds steps and wall-clock time as given: the zero value is unbounded.
  Pass `starlarkcap.DefaultLimits` (5,000,000 steps, 10 minutes) or your own.
  `lim.Timeout` also bounds every capability call, `jobs.start` and job `.wait()`
  the script makes, so a script blocked in a slow call stops at the limit with
  `invalid` and "script exceeded its time limit" (not `cancelled`, which is the
  caller's context ending), and the call's job is cancelled with the rest.

## Check: static analysis

`Check(v, filename, src, extra...)` parses and resolves `src` in the same dialect
and checks, against `v` (nil means no capabilities):

- every dotted path from a capability namespace names a capability or namespace
  in `v`, with a nearest-name suggestion when it does not;
- every literal ref passed to `call()` or `jobs.start()` resolves in `v`;
- keyword arguments of a direct call fit the input schema: no unknown names when
  the schema is closed, no missing required ones (skipped for a positional dict or
  `**kwargs`).

`extra` names the host's predeclared built-ins. `Analyze` adds `Uses` (each
capability the script calls, once per name, in source order) and `Loads`;
`AnalyzeWith` with `CheckOptions.Modules` also checks that `load()` targets exist,
parse and define the loaded names. `Env(v)` is the underlying `check.Env`, for
hosts that call script-services' checker directly. An empty result means every
reference the checker can see resolves; it says nothing about runtime values.

## Capability files and bundles

Capability files are read only from bundles. A folder (or `embed.FS`) needs a
`bundle.yaml` at its root that declares each file as a contribution of the kind
`capability.script@1`; nothing is discovered from file names, and a folder with
no `bundle.yaml` does not load:

```yaml
apiVersion: craggy.io/bundle/v1
metadata: {id: example.apk, version: 1.0.0}
contributions:
  - {id: apk, kind: capability.script@1, path: apk.star}           # defines apk.*
  - {id: scan, kind: capability.script@1, path: tools/scanner.star, assets: [assets/types.go]}
  - {id: apk-test, kind: craggy.script-test@1, path: apk_test.star} # run by Test only
```

- **The contribution id is the prefix**, whatever the file is called. It must be a
  capability name with no `.` (`[a-z0-9-]+`): one file defines one top-level
  prefix, so prefixes never nest. Otherwise the load fails with
  `capability.script.prefix`.
- **Modules** are support files of the same bundle, loaded as `//lib/x.star` or
  relative to the file, or files of a bundle declared in `dependencies`, loaded as
  `@<bundle-id>/x.star` (script-services' `modules.FromContribution`). They are
  not declared as contributions.
- **Types**: a contribution may list one asset whose path ends in `.go` (such as
  `assets/types.go`) with Go-syntax type declarations for that file. With
  `LoadOptions.Types` set by the host, such an asset is an error
  (`capability.script.types`).
- **Tests** are `craggy.script-test@1` contributions (paths ending in
  `_test.star`). `Load` and `Watch` accept them without checking or running them;
  `Test` runs them.
- The kind is global: two bundles in one catalog cannot both define a prefix
  (`bundle.id_conflict`) unless a bundle override replaces one with the other.

## Load: defining capabilities

`Load(ctx, r, sources, opts)` runs `bundle.Load` over `sources` and installs
every capability file of the catalog.

1. **Check.** Each file is analysed with `capability` and `schema` predeclared and
   its `load()` targets looked up in its bundle and declared dependencies. Any
   error diagnostic fails the load with `invalid` and the positioned diagnostics;
   nothing has run. Warnings are in `Loaded.Diagnostics`.
2. **Run.** The file's top level executes once, while the catalog compiles, under
   `opts.Limits` (zero means `DefaultLimits`), with `print` to `opts.Print`. Each
   `capability(...)` call is validated as it runs. Compiling touches no registry,
   and `bundle.Live` reuses a compiled file until its bundle or a dependency
   changes.
3. **Install.** For each file, in catalog order: if `r.Replace(prefix)` would
   remove a capability another file installed (see
   [one file per prefix](#load-policy)), it fails with `conflict`; the
   supersession policy is checked against the registry as it stands; then
   `r.Replace(prefix, defs...)` swaps the prefix atomically. `Load` is all
   or nothing: a failure rolls back the files installed before it, and a
   bundle that fails to load installs nothing.

```python
capability(name, version=1, title="", description="", input=None, output=None,
           effects=[], tags=[], supersedes=[], requires=[], accepts=[], emits=[], dependencies=[], long=False,
           heartbeat=0, run=<fn>, check=None)
```

- `input`/`output`: a `schema.*` value, a field dict
  (`{"artifact": schema.string(required = True)}`), a raw JSON Schema dict, a JSON
  string, or, with `LoadOptions.Types` or a types asset, a declared type's name
  (`input = "DecompileIn"`, or `schema.type("DecompileIn")`).
- `effects`: `"read_only"`, `"idempotent"`, `"destructive"`, `"open_world"`.
  `heartbeat` is in seconds. `accepts`/`emits` entries are type strings or
  `{"type", "level"}` dicts.
- `run(ctx, input)` returns a JSON-shaped value. `ctx` has `job_id`, `caller` (a
  dict of `id`, `kind`, `session`, or `None`), `progress(stage=, message=, done=,
  total=, unit=, data=)`, and `call(ref, **kwargs)` / `start(ref, **kwargs)`, which
  go through the **caller's** view and are bounded by `opts.Limits.Timeout`: a
  `ctx.call` cut short by the limit cancels its job, and the capability fails
  `internal` with "script exceeded its time limit".
- `check(ctx)` returns `None`, `True` or `""` for ready; `False`, a message, or a
  failure for not ready.
- `dependencies` lists external prerequisites as `{"kind": "api_key", "name": "web.brave.api-key"}`
  or `{"kind": "container", "name": "worker-profile"}`. The host registry's
  dependency resolver checks them before the capability appears in tool lists.

Each invocation of `run` or `check` runs on a fresh thread under `opts.Limits`.
Modules see `schema` only; the capability file also sees `capability`, which
works only while the file's top level runs: captured and called later (from a
`run` function, say) it fails with "capability() is only valid while a file
loads".

### Load policy

- **Prefix-scoped names.** Every capability must be named `prefix` or under
  `prefix.`; otherwise the load fails with `invalid` at the offending line.
- **One file per prefix.** An install fails with `conflict`, naming the other
  file, when `r.Replace(prefix)` would remove a capability another file installed
  (its `Origin.Kind` is `"script"` and its `Origin.Ref` a different contribution
  `owner#id`). The same contribution reinstalls freely, and Go-registered
  capabilities under the prefix are replaced as before. To move a prefix to
  another bundle outside `Watch`, unload it first (`r.Replace(prefix)`); `Watch`
  moves its own. The check and the swap are two steps, so serialise concurrent
  installs of the same prefix (`Watch` installs one file at a time).
- **Supersession.** A capability may supersede any ref under its prefix. A ref
  outside the prefix needs `LoadOptions.MaySupersede` to admit it, else the
  install fails with `denied`. The filter is called with the registered
  capability the ref names, so both name and tag policies work:
  `registry.Match("jadx.*")`, `registry.Not(registry.Tagged("admin"))`. A ref
  naming nothing registered is denied outright — the policy cannot judge a
  capability it cannot see, and a dormant supersession must not spring on
  whatever registers under that name later; register the target first. A pinned
  ref (`x.y@1`) is checked against that version; a bare ref supersedes every
  version, so every registered version must pass, and one that fails denies the
  install. The policy is checked at install time, against the registry as it
  then stands, not while the file compiles. `nil` admits nothing outside the
  prefix.
- **Same filters as Go.** Script capabilities appear in every view whose filter
  admits them, and nested calls stay within the caller's view.

### Origin and digests

Every installed capability gets `Origin{Kind: "script", Ref: "owner#id",
Digest: Loaded.Digest}` (the contribution, such as `example.apk#apk`, also in
`Loaded.Ref`), visible as `Entry.Origin` and `Descriptor.origin` (the HTTP
listing). Without types, `Loaded.Digest` is the script digest script-services'
compiler computes (`starcompile.Unit.Ref().Digest`, built with `modules.Digest`,
equal to `script.Result.Digest` for the same file): it covers the file's text and
each direct `load()` dependency, the spec as written paired with that module's
digest, so modules further down count through their parents' digests; it does not
cover where the files live. `Loaded.Modules` lists every module loaded,
transitively, with its path and digest. A module change changes the digest of
every file that loads it.

With a types asset, or `LoadOptions.TypesDigest` beside `LoadOptions.Types` (use
`starlarkcap.TypesDigest(src)` on the text you parsed: a `*types.Package` keeps
no source), `Loaded.Digest` is a SHA-256, under the tag
`craggy-capability-file-v1`, over the script digest and the types digest, so a
changed declaration changes the digest of every file typed by it. With `Types`
and no `TypesDigest` the digest does not cover the declarations.

## Watch: hot reload

`Watch(ctx, r, WatchOptions{Sources: []bundle.Source{bundle.Dir(dir)}, ...})`
holds the sources in a `bundle.Live`, compiled as `Load` compiles, installs every
capability file of the first catalog before it returns, and applies each new
catalog to the registry. The Live reloads every `Interval` (default one second)
with `Run`; a `Subscribe` callback applies the result.

- **Bundles only.** Every source needs its `bundle.yaml`. Declare a contribution
  to add a file; remove it from the manifest to unload the prefix with
  `r.Replace(prefix)`. Modules and test files are not watched as capability files.
- **Roots discover bundles.** `WatchOptions.Roots` names folders whose child
  folders with a `bundle.yaml` are bundles (`bundle.Children`), listed again on
  every reload: adding a bundle folder installs its files, removing one unloads
  them. A root must exist when `Watch` is called. `Sources` and `Roots` combine;
  at least one is required.
- **The first load must succeed.** Otherwise `Watch` returns the error
  (`invalid`, with the diagnostics and the `*bundle.LoadError`) and starts nothing.
- **Reloads are whole-catalog.** When any bundle fails to load (a syntax error in
  one file, a missing module, an invalid manifest), the previous catalog stays
  current, every installed file stays as it was, and `OnError` receives the
  error. Fix the problem and the next reload applies.
- **What reinstalls.** A file reinstalls when its digest or its contribution
  changes: its text, any module it loads, or its types. Files whose digest is
  unchanged are left alone even when another file of their bundle changed. A file
  whose install was refused (`conflict`, `denied`) is retried with every new
  catalog. Moving a prefix between watched bundles is a reinstall, not a conflict.
- **Confined to the bundle.** `bundle.Dir` reads through an `os.Root`, and
  `bundle.Load` refuses symbolic links and paths that leave the bundle, so a
  symlinked capability file or module fails the load.
- `OnResult(prefix, loaded, err)` is called after every install, reinstall and
  unload (an unload reports a `Loaded` with only `Prefix` set), and for every
  refused install, from the watcher's goroutine (from `Watch` itself for the first
  catalog). `OnError(err)` is called for each failed reload.
- `Close` (or the context ending) stops reloading; installed capabilities stay
  installed. `Close` waits for a reload in progress, except while an `OnResult`
  or `OnError` callback runs (so a callback may call it): then it stops the
  watcher without waiting, and the catalog being applied installs and unloads
  nothing more after the callback returns.
- `WatchOptions.Load` applies to every file, including its `MaySupersede` policy.
- **A prefix claims its whole name.** A file `jadx` replaces every `jadx.*`
  capability, Go-registered ones included, and unloading it unloads them all. Use
  ids reserved for scripts.

## Test: Starlark tests

`Test(t, r, fsys, opts)` loads the bundle rooted at `fsys` and runs every
`craggy.script-test@1` contribution it declares as Go subtests (see
script-services' `startest`); an undeclared `*_test.star` file does not run. Test
files and the modules they load see `assert` plus the capability globals of
`r.View(registry.All)` (namespaces, `call()`, `jobs.start`), so they call
capabilities exactly as scripts do, including ones a capability file installed
through `Load`. The bundle's capability files compile with `opts` but are not
installed by `Test`: install them first. See
[scripttest_test.go](../starlarkcap/scripttest_test.go) and its bundle in
[testdata/scripts](../starlarkcap/testdata/scripts/bundle.yaml). For a directory
on disk pass an `os.Root`'s FS, which keeps symbolic links inside it:

```go
func TestCapabilities(t *testing.T) {
	r := registry.New()
	defer r.Close()
	// ... add Go capabilities ...
	if _, err := starlarkcap.Load(t.Context(), r, []bundle.Source{bundle.Dir("testdata/scripts")}, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot("testdata/scripts")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	starlarkcap.Test(t, r, root.FS(), starlarkcap.LoadOptions{})
}
```

```python
load("//lib/names.star", "java_sources")

def test_decompile_through_registry():
    out = apk.decompile(artifact = "app.apk")
    assert.eq(out["sources"], java_sources("app.apk"))

def test_input_is_validated():
    assert.fails(lambda: apk.decompile(artifakt = "x"), "invalid")
```

## Working with other packages

`starlarkcap` imports `registry` and script-services. The
[conformance suite](conformance.md) loads its Starlark capability set with `Load`
from an in-memory bundle and runs the Starlark projection beside the others.
Hosts combine `Exec` with their own `Predeclared` built-ins and `modules.Loader`,
and `Load`/`Watch` with their supersession policy. Capability files
compile through script-services' `starcompile` and load through foundation-kit's
`bundle`, the same path as every other kind of authored content.

## Examples

Compiled and checked by `go test`, in
[starlarkcap/example_test.go](../starlarkcap/example_test.go):

- `ExampleLoad`: a bundle whose file loads a shared module supersedes
  `jadx.decompile`; denied
  without a policy, allowed with `MaySupersede: registry.Match("jadx.*")`; `Origin`
  and digest; unloading restores the original.
- `ExampleLoad_checkFirst`: a typo rejects the load before the file runs.
- `ExampleLoad_types`: `input = "DecompileIn"` from `types.Parse`.
- `ExampleLoad_errors`: a nested `conflict` surfaces as `internal`.
- `ExampleExec`: hooks, print positions, an unhandled `not_found` and its
  `Error()` text.
- `ExampleCheck`: diagnostics with suggestions ("unknown capability ...").
- `ExampleWatch`: the initial install of a bundle directory (`bundle.Dir`) that
  also declares a test file.

```go
loaded, err := starlarkcap.Load(ctx, r, []bundle.Source{bundle.Dir("caps")},
	starlarkcap.LoadOptions{MaySupersede: registry.Match("jadx.*")})
// caps/bundle.yaml declares {id: apk, kind: capability.script@1, path: apk.star}
// loaded[0].Capabilities == [apk.decompile@1], loaded[0].Modules[0].Path == "lib/names.star"
// r.Get("apk.decompile").Origin == {script example.apk#apk <loaded[0].Digest>}
// r.Get("jadx.decompile").SupersededBy == [apk.decompile]
```

## Semantics, errors and limits

- **Load errors.** A bundle that does not load (a missing or invalid
  `bundle.yaml`, an invalid prefix, check failures, names outside the prefix,
  malformed `capability()` arguments, runtime errors and limits in the file's top
  level, failing modules) is `invalid`. The message is the `*bundle.LoadError`'s, listing
  every diagnostic as `bundle:path:line:col: severity: message (code)`
  (`bundle.Diagnostic.String`), and the `*bundle.LoadError` is in the
  error chain for its `Diagnostics`. A denied supersession is `denied`, a prefix
  owned by another file `conflict`. A cancelled context is `cancelled`.
  `Replace`'s own errors (for example a duplicate `name@version`) pass through.
- **Errors inside script-defined capabilities surface as `internal`**, with the
  script position and the nested failure in the message: a `ctx.call` that fails
  with `conflict` gives
  `internal: apk.lock: apk.star:2:41: conflict: artifact art-1 is already locked`.
  The script, not its caller, is at fault, so `registry.CodeOf` is `internal` and
  any reason is not carried. A cancelled job's handler error is `cancelled`. Bad
  input never reaches the script: the registry rejects it first.
- **Exec errors** are `*starlarkcap.Error{Code, Reason, Message, Pos, Backtrace,
  Steps}`: the failing capability's code and reason when an unhandled call stopped
  the script, `cancelled` when the context ended, otherwise `invalid` (compile and
  runtime errors, limits; a timeout's message is "script exceeded its time
  limit"). `Unwrap` exposes a `*registry.Error`, so `registry.CodeOf` and
  `ReasonOf` work. When a capability call stopped the script, `Message` is that
  call's error text, which begins with its code
  (`not_found: capability "x.y" not found`); `Error()` names the code once
  (`not_found: bad.star:1:5: capability "x.y" not found`). A `Predeclared` name
  that collides with a capability global, `call`, `jobs` or `input` is `invalid`.
- **Snapshot and live view.** What a script can name is fixed when `Globals` (or
  `Exec`) builds its globals: namespaces from `List()`, `call()` and `jobs.start()`
  refs from `All()`. The call itself resolves the ref against the live view.
- **Time limits reach blocked calls.** Starlark cannot interrupt a built-in that
  is waiting, so `Exec` and script-defined handlers and checks run their
  capability calls under a context bounded by `Limits.Timeout`. A script stuck in
  a call stops at the limit, and the job it was waiting on is cancelled.
- **Watch** polls through `bundle.Live.Run`; there is no file-system
  notification. Each reload reads and hashes every bundle; only compiling is
  skipped for unchanged ones.
- **Script capabilities are not durable.** Each install replaces the prefix's
  handlers; running jobs keep the version they started with.

## Validation and evidence

`go test ./starlarkcap` covers the globals (kwargs and dicts, the view as the
whole world, `jobs.start`/wait/cancel, superseded warnings), `Load` (definition
and supersession, subjects, no privilege escalation, failures leaving the
registry unchanged, step/time limits and checks, raw and dict schemas, names
outside the prefix, the supersession policy, check before install, shared-module
digests and `Origin`, positioned module failures, types, print and `ctx`
attributes, one file per prefix, dotted prefixes rejected, rollback across files,
`capability()` after the load, every-version supersession policy, types in the
digest, the time limit around `ctx.call`),
`Exec` (body then entry with hooks, positions and codes, host-name clashes,
cancelling started jobs, reasons, modules and digests, JSON shape, the time limit
around a blocked call, `Error()` text, `call()` against the snapshot),
`Check`/`Analyze`/`AnalyzeWith`, `Watch` (lifecycle with a failed reload kept
whole, the first load and recovery, policy, retry and stopping with the context,
dotted ids rejected, `Close` from `OnResult`, reinstall on a types asset change
only for the file that declares it, symbolic links refused, an embedded bundle, a
prefix moving between bundles) and `Test` against the
[testdata/scripts](../starlarkcap/testdata/scripts/bundle.yaml) bundle.
