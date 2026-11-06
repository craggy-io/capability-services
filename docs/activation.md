# Coordinated activation and pinned invocation (plan)

[Project](../README.md) · [Documentation index](README.md) · [Agent guide](../AGENTS.md)

**Status:** planned, 2026-11-06. First consumer: craggy-labs (gate packs, workers and
recipes installed at run time). Nothing here is implemented yet; each section ends
with its acceptance tests.

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

// starlarkcap: compute changes from a catalog the host owns; never loads on its own.
type Plan struct {
	Changes  []registry.Change
	Identity Identity // catalog digest + each file's Origin
	Problems []bundle.Diagnostic
}
func PlanCatalog(ctx context.Context, r *registry.Registry, cat *bundle.Catalog, opts LoadOptions) Plan

// One activation attempt, recorded by the host.
type Activation struct {
	Attempted Identity // what the host tried to make callable
	Active    Identity // what is callable after the attempt (unchanged on failure)
	Err       error
}
func Activate(ctx context.Context, r *registry.Registry, cat *bundle.Catalog, opts LoadOptions) Activation
```

The host subscribes to its own `Live`, calls `Activate` with each new catalog, and
records both identities (craggy-labs writes them to its change feed and activity
log). `Watch` becomes a thin loop over `Activate` for hosts that do not own a
catalog, so there is one application path.

**Restart recovery.** `Live.Install` is not crash-safe, so the host writes an
intent record (`fsx.WriteFile`) naming the bundle and digest before installing and
clears it after a successful activation. On start, a remaining intent means the
install was interrupted: the host reloads, activates whatever is on disk and
reports it, or uninstalls it when it fails to validate. Evaluations are accepted
only after this reconciliation.

### Acceptance tests

- A catalog where one of three files fails to compile changes nothing: the
  previous callable set still answers and `Active` is unchanged.
- A valid catalog swaps every prefix in one snapshot: a concurrent `List` never
  sees a mix.
- `Attempted` and `Active` differ exactly when activation failed.
- `Watch` and `Activate` produce identical registries for the same sources.

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
  the capability is replaced or removed. If it cannot be retained (the registry
  closed), the call fails `unavailable` with reason `pin_lost`; it never silently
  switches.
- A call outside the pinned closure fails `denied` with reason `undeclared`. Script
  capabilities invoked through a pinned view run with the same pinned view, so
  nested calls are covered by the same closure.
- Pinning fixes identity, not permission: the pinned view is intersected with the
  caller's view, and readiness checks still apply.
- Empty `Origin.Digest` is allowed by the registry, but `Identities` reports it so
  a certifying host can refuse it with a diagnostic.

### Acceptance tests

- Replace a capability between two calls through one `Pinned`: both calls run the
  original handler; a new `Pin` sees the replacement.
- A nested script call reaches only its declared `Requires`; an undeclared call
  fails `denied/undeclared`.
- A pinned call outside the caller's view fails `denied`.
- `Identities` lists the full closure with digests; `Calls` lists only invoked
  entries.
