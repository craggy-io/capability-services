# Starlark

[Project](../README.md) · [Documentation index](README.md) · [Design](design.md#starlark-starlarkcap)

Package `starlarkcap`. Available Go API. It is the only package in this module
that imports `github.com/craggy-io/script-services`, the generic Starlark runtime.

## Purpose

Connect scripts and the registry in both directions: scripts **call**
capabilities (`Globals`, `Exec`, checked statically by `Check`), and scripts
**define** capabilities (`Load`, kept current by `Watch`, tested by `Test`).

## Features

- Capabilities as nested built-ins (`storage.artifact.get(id = "…")`), `call()`
  for pinned and superseded refs, and `jobs.start()` for concurrent work.
- A script host (`Exec`) with step and wall-clock limits, `load()` modules,
  content digests, print and per-call hooks with source positions, coded errors
  with backtraces, and cleanup of jobs a failed script started.
- Static checks against a view (`Check`, `Analyze`, `AnalyzeWith`, `Env`) with
  nearest-name suggestions, run before anything executes.
- Capability files (`Load`): prefix-scoped names, one file per prefix, the host's
  supersession policy, check before install, schemas from `schema.*`, field dicts,
  raw JSON Schema or declared Go-syntax types, readiness checks, and `Origin` with
  a digest of the file, its modules and its types.
- Hot reload of a directory (`Watch`, confined to it by an `os.Root`) and
  Starlark tests (`Test`).

## Mental model

```
             Globals / Exec / Check                  Load / Watch / Test
 script ───────────────────────────► registry ◄─────────────────────────── capability file
         calls through ViewFrom(ctx)            Replace(prefix, caps...)
```

Everything generic comes from script-services: the dialect (Starlark plus
`set()`, `while`, top-level control flow and global reassignment; no recursion),
value conversion between Starlark, Go and JSON, the `schema` module and
`types.go` declarations, `load()` resolution with content digests, static checks,
and the `*_test.star` runner with its `assert` module. Read that module's guides
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
- `Load(ctx, r, prefix, Source, LoadOptions) (Loaded, error)`;
  `Source{Filename, Src, Modules}`; `LoadOptions{Limits, Print, Types,
  TypesDigest, MaySupersede}`; `TypesDigest(src) string`; `Loaded{Prefix, Digest,
  Modules, Capabilities, Diagnostics}`.
- `Watch(ctx, r, WatchOptions{Dir, Root, Interval, Load, OnResult}) (*Watcher,
  error)`; `(*Watcher).Close()`; `TypesFile` (`"types.go"`).
- `Test(t, r, fsys)`.

Types from script-services that appear in this API: `modules.Source` (for
example `modules.DirRoot(dir)` for a directory on disk, `modules.Dir(fsys)` for an
`embed.FS`, or `modules.Map(files)`), `*modules.Loader`, `modules.Ref`,
`*types.Package` (from `types.Parse`), `check.Diagnostic`, `check.Env`,
`script.Limits`, `script.Position`. `Limits`, `Position` and `Frame` are aliased
here, so a host needs script-services' `modules` and `types` packages only for
`Source.Modules`, `Program.Modules` and `LoadOptions.Types`.

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

## Load: defining capabilities

`Load(ctx, r, prefix, src, opts)` installs the capabilities a file defines under
`prefix`:

1. **Check.** The file is analysed with `capability` and `schema` predeclared and
   its `load()` targets looked up in `src.Modules`. Any error diagnostic rejects
   the load with `invalid` and the positioned diagnostics; nothing has run.
   Warnings are returned in `Loaded.Diagnostics`.
2. **Run.** The file's top level executes under `opts.Limits` (zero means
   `DefaultLimits`), with `print` to `opts.Print`. Each `capability(...)` call is
   validated as it runs.
3. **Install.** If `r.Replace(prefix)` would remove a capability another file
   loaded (see [one file per prefix](#load-policy)), the load fails with
   `conflict`. Otherwise `r.Replace(prefix, defs...)` swaps the prefix
   atomically. Any failure in any step leaves the registry unchanged.

```python
capability(name, version=1, title="", description="", input=None, output=None,
           effects=[], tags=[], supersedes=[], accepts=[], emits=[], long=False,
           heartbeat=0, run=<fn>, check=None)
```

- `input`/`output`: a `schema.*` value, a field dict
  (`{"artifact": schema.string(required = True)}`), a raw JSON Schema dict, a JSON
  string, or, with `LoadOptions.Types`, a declared type's name
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

Each invocation of `run` or `check` runs on a fresh thread under `opts.Limits`.
Modules see `schema` only; the capability file also sees `capability`, which
works only while the file's top level runs: captured and called later (from a
`run` function, say) it fails with "capability() is only valid while a file
loads".

### Load policy

- **Prefix-scoped names.** Every capability must be named `prefix` or under
  `prefix.`; otherwise the load fails with `invalid` at the offending line.
- **One file per prefix.** A load fails with `conflict`, naming the other file,
  when `r.Replace(prefix)` would remove a capability another file installed with
  `Load` (its `Origin.Kind` is `"script"` and its `Origin.Ref` a different
  filename): loading `a` from `a.star` would otherwise wipe `a.b.*` loaded from
  `a.b.star`, in either order. The same file reloads freely, and Go-registered
  capabilities under the prefix are replaced as before. To move capabilities
  between files, unload the old one first (`r.Replace(its prefix)`). The check
  and the swap are two steps, so serialise concurrent loads of overlapping
  prefixes (`Watch` loads one file at a time).
- **Supersession.** A capability may supersede any ref under its prefix. A ref
  outside the prefix needs `LoadOptions.MaySupersede` to admit it, else the load
  fails with `denied` and installs nothing. The filter is called with the
  registered capability the ref names, so both name and tag policies work:
  `registry.Match("jadx.*")`, `registry.Not(registry.Tagged("admin"))`. A ref
  naming nothing registered is denied outright — the policy cannot judge a
  capability it cannot see, and a dormant supersession must not spring on
  whatever registers under that name later; register the target first. A pinned
  ref (`x.y@1`) is checked against that version; a bare ref supersedes every
  version, so every registered version must pass, and one that fails denies the
  load. The policy is checked again at install time, so a target registered or
  re-tagged while the file's top level ran must pass too (the load rolls back
  if it does not). `nil` admits nothing outside the prefix.
- **Same filters as Go.** Script capabilities appear in every view whose filter
  admits them, and nested calls stay within the caller's view.

### Origin and digests

Every installed capability gets `Origin{Kind: "script", Ref: src.Filename,
Digest: Loaded.Digest}`, visible as `Entry.Origin` and `Descriptor.origin` (the
HTTP listing). Without types, `Loaded.Digest` is the content address
script-services computes for a program (`script.Result.Digest`, built with
`modules.Digest`): it covers the file's text and each direct `load()`
dependency, the spec as written paired with that module's digest, so modules
further down count through their parents' digests; it does not cover where the
files live. `Loaded.Modules` lists every module loaded, transitively, with its
path and digest. A module change changes the digest of every file that loads it,
on its next load. The exact module digest construction is script-services'; see
its modules guide.

A `*types.Package` keeps no source, so `Load` cannot hash the declarations
behind `LoadOptions.Types` itself. Pass `LoadOptions.TypesDigest` (use
`starlarkcap.TypesDigest(src)` on the text you parsed) and `Loaded.Digest`
becomes a SHA-256, under the tag `craggy-capability-file-v1`, over the script
digest and the types digest: a changed declaration then changes the digest of
every file loaded with it. With `Types` and no `TypesDigest` the digest does not
cover the declarations. `Watch` sets both for its `types.go`.

## Watch: hot reload

`Watch(ctx, r, WatchOptions{Dir: dir, ...})` loads every top-level `*.star` file
in the directory except `*_test.star`, each under the prefix named by its stem
(`apk.star` defines `apk.*`), with `load()` resolving against the same
directory, and polls every `Interval` (default one second).

- **The directory confines the files.** `Dir` is opened with `modules.DirRoot`,
  an `os.Root`: no path, `..` or symbolic link reaches outside it, so a symlinked
  capability file or module pointing elsewhere does not load. `Root` takes an
  `fs.FS` instead, for an `embed.FS`, an `fstest.MapFS` or an `os.Root`'s FS
  (`root, _ := os.OpenRoot(dir); root.FS()`); do not pass `os.DirFS(dir)`, which
  follows symbolic links out of the directory. Set exactly one.
- A file reloads when its text changes, when any module it loaded changes, or
  when the types file changes. A failed reload keeps the previous version
  installed and reports the error; a file that has never loaded retries whenever
  any `.star` file under the root changes.
- Removing a file unloads its prefix with `r.Replace(prefix)`.
- `OnResult(prefix, loaded, err)` is called after every load, reload and unload
  (an unload reports a `Loaded` with only `Prefix` set), and once for each
  rejected file, from the watcher's goroutine (from `Watch` itself during the
  initial scan).
- The initial scan finishes before `Watch` returns. `Close` (or the context
  ending) stops polling; installed capabilities stay installed. `Close` waits for
  a scan in progress, except when called from `OnResult`: on the watcher's own
  goroutine it stops the watcher without waiting for itself.
- `WatchOptions.Load` applies to every file, including its `MaySupersede` policy.
- **Types.** Unless `WatchOptions.Load.Types` is set, a `types.go`
  (`starlarkcap.TypesFile`) in the root is parsed and passed to every file as
  `LoadOptions.Types`, with `TypesDigest` set, so the declarations count in every
  `Loaded.Digest`. When it changes, appears or disappears every file reloads;
  while it does not parse, every (re)load fails with its error and previous
  versions stay installed.
- **One file defines one prefix.** A stem containing `.` (`apk.extra.star`) is
  rejected and reported with `invalid`: its prefix would nest inside `apk.star`'s,
  whose `Replace("apk")` would wipe it. Define `apk.extra.*` capabilities in
  `apk.star`.
- **A stem claims its whole prefix.** `jadx.star` replaces every `jadx.*`
  capability, Go-registered ones included, and removing it unloads them all. Use
  a directory whose stems are reserved for scripts. A stem must be a valid
  capability name, or its load fails.

## Test: Starlark tests

`Test(t, r, fsys)` runs every `*_test.star` file in `fsys` as Go subtests (see
script-services' `startest`). Test files and the modules they load see `assert`
plus the capability globals of `r.View(registry.All)` (namespaces, `call()`,
`jobs.start`), so they call capabilities exactly as scripts do, including ones a
capability file defined through `Load`. See
[scripttest_test.go](../starlarkcap/scripttest_test.go) and its fixtures in
[testdata/scripts](../starlarkcap/testdata/scripts/apk_test.star). For a
directory on disk pass an `os.Root`'s FS, which keeps symbolic links inside it:

```go
func TestCapabilities(t *testing.T) {
	r := registry.New()
	defer r.Close()
	// ... add Go capabilities, Load the .star files under test ...
	root, err := os.OpenRoot("testdata/scripts")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	starlarkcap.Test(t, r, root.FS())
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
and runs the Starlark projection beside the others. Hosts combine `Exec` with
their own `Predeclared` built-ins and `modules.Loader`, and `Load`/`Watch` with
their supersession policy.

## Examples

Compiled and checked by `go test`, in
[starlarkcap/example_test.go](../starlarkcap/example_test.go):

- `ExampleLoad`: a file with a shared module supersedes `jadx.decompile`; denied
  without a policy, allowed with `MaySupersede: registry.Match("jadx.*")`; `Origin`
  and digest; unloading restores the original.
- `ExampleLoad_checkFirst`: a typo rejects the load before the file runs.
- `ExampleLoad_types`: `input = "DecompileIn"` from `types.Parse`.
- `ExampleLoad_errors`: a nested `conflict` surfaces as `internal`.
- `ExampleExec`: hooks, print positions, an unhandled `not_found` and its
  `Error()` text.
- `ExampleCheck`: diagnostics with suggestions ("unknown capability ...").
- `ExampleWatch`: the initial scan of a directory opened with `Dir`.

```go
l, err := starlarkcap.Load(ctx, r, "apk", starlarkcap.Source{
	Filename: "apk.star",
	Src:      []byte(apkStar),
	Modules:  modules.Map(map[string]string{"lib/names.star": "def sources_for(name): ..."}),
}, starlarkcap.LoadOptions{MaySupersede: registry.Match("jadx.*")})
// l.Capabilities == [apk.decompile@1], l.Modules[0].Path == "lib/names.star"
// r.Get("apk.decompile").Origin == {script apk.star <l.Digest>}
// r.Get("jadx.decompile").SupersededBy == [apk.decompile]
```

## Semantics, errors and limits

- **Load errors.** Check failures, names outside the prefix, malformed
  `capability()` arguments, runtime errors and limits in the file's top level, and
  failing modules are `invalid`, with `file:line:col` positions. A denied
  supersession is `denied`. A cancelled context is `cancelled`. `Replace`'s own
  errors (for example a duplicate `name@version`) pass through.
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
- **Watch** polls; there is no file-system notification. Reads that fail count as
  absent files.
- **Script capabilities are not durable.** Each `Load` replaces the prefix's
  handlers; running jobs keep the version they started with.

## Validation and evidence

`go test ./starlarkcap` covers the globals (kwargs and dicts, the view as the
whole world, `jobs.start`/wait/cancel, superseded warnings), `Load` (definition
and supersession, subjects, no privilege escalation, failures leaving the
registry unchanged, step/time limits and checks, raw and dict schemas, names
outside the prefix, the supersession policy, check before install, shared-module
digests and `Origin`, positioned module failures, types, print and `ctx`
attributes, one file per prefix, `capability()` after the load, every-version
supersession policy, types in the digest, the time limit around `ctx.call`),
`Exec` (body then entry with hooks, positions and codes, host-name clashes,
cancelling started jobs, reasons, modules and digests, JSON shape, the time limit
around a blocked call, `Error()` text, `call()` against the snapshot),
`Check`/`Analyze`/`AnalyzeWith`, `Watch` (lifecycle, retry when a module appears,
policy and stopping with the context, nested stems rejected, `Close` from
`OnResult`, reload on a `types.go` change, symbolic links kept inside `Dir`, an
`fs.FS` root) and `Test` against
[testdata/scripts](../starlarkcap/testdata/scripts/apk.star).
