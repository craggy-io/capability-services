package starlarkcap

import (
	"context"
	"slices"
	"sort"
	"sync"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/capability-services/registry"
)

// Compilers are the bundle compilers a host adds to the catalog it owns so
// that Activate finds its capability files: the Script compiler under opts
// and one that accepts script tests without running them (as Load does).
func Compilers(opts LoadOptions) []bundle.AnyCompiler { return compilers(opts) }

// FileIdentity names one capability file of an activation.
type FileIdentity struct {
	Prefix string `json:"prefix"`
	Ref    string `json:"ref"`    // contribution, "owner#id"
	Digest string `json:"digest"` // the file's content address (Loaded.Digest)
}

// Identity is what an activation made, or tried to make, callable: the
// catalog's digest and every capability file in it, sorted by prefix.
type Identity struct {
	Catalog string         `json:"catalog"`
	Files   []FileIdentity `json:"files"`
}

// Activation reports one Activate call. Attempted is the catalog it was
// given; Active is what is callable afterwards. They differ exactly when
// Err is set, and then Active is the previous identity.
type Activation struct {
	Attempted Identity `json:"attempted"`
	Active    Identity `json:"active"`
	Err       error    `json:"-"`
}

// Activator applies catalogs a host owns to a registry, whole: every
// capability file of a catalog becomes callable in one registry.Apply, or
// none does and the previous set stays callable. Use it instead of Watch
// when the host keeps its own bundle.Live (with Compilers among its
// compilers) for other kinds too, so the callable set and the catalog
// cannot disagree. An Activator owns the prefixes it installed and unloads
// those that leave the catalog. It is safe for concurrent use; calls are
// serialized.
type Activator struct {
	r    *registry.Registry
	opts LoadOptions

	mu     sync.Mutex
	active Identity
}

// NewActivator returns an Activator for r with nothing active.
func NewActivator(r *registry.Registry, opts LoadOptions) *Activator {
	return &Activator{r: r, opts: opts}
}

// Active is the identity currently callable.
func (a *Activator) Active() Identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneIdentity(a.active)
}

// Activate makes cat's capability files callable as one change. It refuses,
// changing nothing, when a file's prefix is owned by a capability file this
// Activator did not install, when a supersession is not allowed by
// opts.MaySupersede, or when the registry rejects the result.
func (a *Activator) Activate(ctx context.Context, cat *bundle.Catalog) Activation {
	a.mu.Lock()
	defer a.mu.Unlock()
	defs := bundle.All(cat, Script)
	sort.Slice(defs, func(i, j int) bool { return defs[i].Value.Prefix() < defs[j].Value.Prefix() })
	attempted := Identity{Catalog: cat.Digest(), Files: make([]FileIdentity, 0, len(defs))}
	for _, d := range defs {
		attempted.Files = append(attempted.Files, FileIdentity{Prefix: d.Value.Prefix(), Ref: d.Value.info.Ref, Digest: d.Value.info.Digest})
	}
	fail := func(err error) Activation {
		return Activation{Attempted: attempted, Active: cloneIdentity(a.active), Err: err}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	owned := map[string]string{} // prefix -> ref this Activator installed
	for _, f := range a.active.Files {
		owned[f.Prefix] = f.Ref
	}
	all := a.r.View(registry.All)
	changes := make([]registry.Change, 0, len(defs)+len(owned))
	present := map[string]bool{}
	for _, d := range defs {
		f := d.Value
		prefix := f.Prefix()
		present[prefix] = true
		if err := ownPrefix(a.r, prefix, f.info.Ref, owned[prefix]); err != nil {
			return fail(err)
		}
		for _, c := range f.caps {
			for _, s := range c.Supersedes {
				if err := maySupersede(all, prefix, c.Name, s, a.opts.MaySupersede); err != nil {
					return fail(err)
				}
			}
		}
		changes = append(changes, registry.Change{Prefix: prefix, Caps: slices.Clone(f.caps)})
	}
	for prefix := range owned {
		if !present[prefix] {
			changes = append(changes, registry.Change{Prefix: prefix})
		}
	}
	if err := a.r.Apply(changes...); err != nil {
		return fail(err)
	}
	a.active = attempted
	return Activation{Attempted: attempted, Active: cloneIdentity(attempted)}
}

func cloneIdentity(id Identity) Identity {
	id.Files = slices.Clone(id.Files)
	return id
}
