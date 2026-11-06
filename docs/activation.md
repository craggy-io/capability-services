# Coordinated activation and pinned invocation (plan)

[Project](../README.md) · [Documentation index](README.md) · [Agent guide](../AGENTS.md)

**Status:** implemented in v0.3.0 (2026-11-06). First consumer: craggy-labs (gate
packs, workers and recipes installed at run time). Each section ends with its
acceptance tests and where they live. Differences from the first draft are noted
inline.

Two guarantees a host needs once bundles that define capabilities can be installed
while it runs:

1. **Coordinated activation.** The host owns one `bundle.Live` catalog. A new
   catalog becomes callable all at once or not at all, and the host can tell what
   it *attempted* from what is *active*.
2. **Pinned invocation.** A long evaluation fixes the exact set of capabilities it
   may call, including what nested script capabilities call, and keeps calling
   those implementations even if newer ones are activated meanwhile.

Both stay inside the existing shapes: `registry.Capability` with its `Origin`,
`bundle.Load`/`bundle.Live` for content, and `registry.Codes` for errors.

## 1. Coordinated activation

Today `starlarkcap.Watch` owns its own `bundle.Live` and applies each capability
file with its own `Replace(prefix)`. A failure half-way leaves some files new and
some old, and the host's own catalog (gate packs, workers) can disagree with the
callable set.

### API

```go
// registry: replace a whole set of capability prefixes in one snapshot swap.
type Change struct {
	Prefix string       // every capability under Prefix is replaced
	Caps   []Capability // empty removes the prefix
}
func (r *Registry) Apply(changes ...Change) error // all-or-nothing, one snapshot

// starlarkcap: apply catalogs the host owns; never loads on its own.
func Compilers(opts LoadOptions) []bundle.AnyCompiler // add to the host's Live
type Identity struct { Catalog string; Files []FileIdentity } // catalog digest + each file
type Activation struct {
	Attempted Identity // what the host tried to make callable
	Active    Identity // what is callable after the attempt (unchanged on failure)
	Err       error
}
func NewActivator(r *registry.Registry, opts LoadOptions) *Activator
func (a *Activator) Activate(ctx context.Context, cat *bundle.Catalog) Activation
func (a *Activator) Active() Identity
```

The host subscribes to its own `Live`, calls `Activate` with each new catalog, and
records both identities (craggy-labs writes them to its change feed and activity
log). The draft's separate `PlanCatalog` step was folded into the stateful
`Activator`, which remembers the prefixes it owns so it can unload those that
leave the catalog. `Watch` keeps its per-file semantics (a refused file is
retried while the others stay current) for hosts that do not own a catalog;
hosts that need whole-catalog activation use an `Activator`.

**Restart recovery.** `Live.Install` is not crash-safe, so the host writes an
intent record (`fsx.WriteFile`) naming the bundle and digest before installing and
clears it after a successful activation. On start, a remaining intent means the
install was interrupted: the host reloads, activates whatever is on disk and
reports it, or uninstalls it when it fails to validate. Evaluations are accepted
only after this reconciliation.

### Acceptance tests

`registry/pin_test.go` (`TestApply*`) and `starlarkcap/activate_test.go`.

- A file that fails to compile fails `bundle.Load`, so the host's `Live` keeps
  its previous catalog and nothing is activated. A catalog the registry or
  supersession policy refuses changes nothing: the previous callable set still
  answers and `Active` is unchanged.
- A valid catalog swaps every prefix in one snapshot: a concurrent `List` never
  sees a mix.
- `Attempted` and `Active` differ exactly when activation failed.
- A prefix another capability file owns is refused (`conflict`), whole.

## 2. Pinned invocation

### API

```go
// Capability gains the refs it may call (scripts and recipes declare them).
type Capability struct { ...; Requires []string }

// Pin resolves refs, and transitively each resolved capability's Requires, now.
type Pinned struct{ ... }
func (v *View) Pin(refs ...string) (*Pinned, error) // not_found / undeclared on a missing provider
func (p *Pinned) View() *View                       // allows exactly the pinned entries
func (p *Pinned) Identities() []Identity            // name, version, Origin (kind, ref, digest)
func (p *Pinned) Calls() []Identity                 // what was actually invoked through it
```

- A pinned view resolves each ref to the snapshot entry captured by `Pin`, not the
  current one; the handler stays reachable while the `Pinned` is held, even if
  the capability is replaced or removed. A closed registry fails every call
  `unavailable/closed`, so no separate `pin_lost` reason was needed; a pinned
  call never silently switches.
- A call outside the pinned closure fails `denied` with reason `undeclared`. Script
  capabilities invoked through a pinned view run with the same pinned view, so
  nested calls are covered by the same closure.
- Pinning fixes identity, not permission: the pinned view is intersected with the
  caller's view (outside it is `not_found`, as for any view), and readiness
  checks still apply: a pinned entry the registry has since replaced runs its
  own `Check` per call.
- Script capabilities declare `requires = [...]` in `capability(...)`. A call
  outside the closure stays `denied/undeclared` when it surfaces through a
  script handler.
- Empty `Origin.Digest` is allowed by the registry, but `Identities` reports it so
  a certifying host can refuse it with a diagnostic.

### Acceptance tests

`registry/pin_test.go` (`TestPin*`) and `starlarkcap/activate_test.go`
(`TestRequiresPinsNestedScriptCalls`).

- Replace a capability between two calls through one `Pinned`: both calls run the
  original handler; a new `Pin` sees the replacement.
- A nested script call reaches only its declared `Requires`; an undeclared call
  fails `denied/undeclared`.
- A pinned call outside the caller's view fails `not_found`.
- `Identities` lists the full closure with digests; `Calls` lists only invoked
  entries.
