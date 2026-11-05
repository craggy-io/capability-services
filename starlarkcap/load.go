package starlarkcap

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	sl "go.starlark.net/starlark"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/kind"

	"github.com/craggy-io/script-services/check"
	"github.com/craggy-io/script-services/modules"
	"github.com/craggy-io/script-services/schema"
	"github.com/craggy-io/script-services/script"
	"github.com/craggy-io/script-services/starcompile"
	"github.com/craggy-io/script-services/startest"
	"github.com/craggy-io/script-services/types"
	"github.com/craggy-io/script-services/values"

	"github.com/craggy-io/capability-services/registry"
)

// Script is the kind of a capability file, capability.script@1. A bundle
// declares each file in its bundle.yaml, and the contribution's id is the
// file's prefix:
//
//	contributions:
//	  - {id: apk, kind: capability.script@1, path: apk.star}
//
// defines the apk.* capabilities, whatever the file is called. The id must
// be a capability name with no "." (one file defines one top-level
// prefix). The kind is global, so two bundles in one catalog cannot both
// define a prefix unless a bundle override makes one replace the other.
//
// For each contribution the compiler checks the file statically,
// then runs its top level once (starcompile.Compiler, with load() served
// by the contribution's bundle and declared dependencies,
// modules.FromContribution) and collects what it defines. It touches no
// registry: supersession policy and prefix ownership are checked at
// install. Every problem is a bundle diagnostic with a file:line position:
// a file that fails its check never runs, and a capability() call that is
// malformed or names a capability outside the prefix fails the file.
//
// The file and the modules it loads see `schema` (see schema.Module); the
// file also sees
//
//	capability(name, version=1, title="", description="", input=None,
//	           output=None, effects=[], tags=[], supersedes=[], accepts=[],
//	           emits=[], long=False, heartbeat=0, run=<fn>, check=None)
//
// input/output accept schema.* values, a field dict, a raw JSON Schema
// dict, a JSON string, or a declared type's name (LoadOptions.Types, or
// the contribution's types asset). effects are names ("read_only",
// "idempotent", "destructive", "open_world"). heartbeat is in seconds.
// accepts/emits entries are type strings or {"type", "level"} dicts.
// run(ctx, input) returns a JSON-shaped value; check(ctx) returns
// None/True for ready, or fails / returns False or a message for not
// ready. ctx has job_id, caller, progress(stage=, message=, done=, total=,
// unit=, data=), and call(ref, **kwargs) / start(ref, **kwargs), which go
// through the *caller's* view.
//
// Types asset: a contribution may list one asset whose path ends in ".go"
// (for example assets/types.go) holding Go-syntax type declarations
// (types.Parse) for that file alone. Only declared assets count: a
// types.go the manifest does not list is not read.
//
// Digest: a file's digest is its script digest (modules.Digest over its
// source and each direct load() dependency, as starcompile.Unit.Ref
// reports; transitive modules count through their parents' digests). With
// a types digest (the asset's, or LoadOptions.TypesDigest) it is instead a
// SHA-256 over both (see fileDigest).
var Script = kind.New[*File]("capability.script", 1, kind.Global())

// Diagnostic codes the Script compiler reports besides starcompile's
// (script.check, script.load, script.exec for a failed top level or a
// rejected capability() call).
const (
	// CodePrefix: the contribution id is not a valid prefix.
	CodePrefix bundle.Code = "capability.script.prefix"
	// CodeTypes: the contribution's type declarations asset is unusable.
	CodeTypes bundle.Code = "capability.script.types"
)

// File is a compiled capability file, the value of a Script contribution:
// the capabilities its top level defined, not yet installed. It is
// immutable; Load and Watch install it into a registry.
type File struct {
	info Loaded // what installing it reports
	caps []registry.Capability
}

// Prefix is the file's prefix, its contribution id.
func (f *File) Prefix() string { return f.info.Prefix }

// LoadOptions configure Load and Watch. Limits, Print, Types and
// TypesDigest shape compiling; MaySupersede is the install-time policy.
type LoadOptions struct {
	// Limits bound the file's top level and each invocation of a
	// capability (or readiness check) it defines, on a fresh thread per
	// call. The zero value means script.DefaultLimits.
	Limits script.Limits
	// Print receives print() output and superseded warnings, from the
	// file's top level and from its capabilities. nil discards it.
	Print func(pos script.Position, msg string)
	// Types makes Go-syntax type declarations available to every file:
	// schema.type("Name") and input = "Name" / output = "Name" (see
	// schema.WithTypes). Without it, a contribution may declare its own
	// declarations as one asset ending in ".go" (see Script); with it,
	// such an asset is an error.
	Types *types.Package
	// TypesDigest is the content address of the source Types was parsed
	// from (use TypesDigest(src)). When set it is folded into
	// Loaded.Digest, so a changed declaration changes the digest of every
	// file compiled with it. A *types.Package keeps no source, so this
	// package cannot compute it: with Types and no TypesDigest, the digest
	// does not cover the declarations. A types asset's digest is always
	// folded in.
	TypesDigest string
	// MaySupersede is the supersession policy for refs outside the prefix:
	// a capability may supersede any name under the prefix, and a name
	// outside it only when MaySupersede admits it. The filter sees the
	// registered capability the ref names. A ref naming nothing
	// registered is denied outright: the policy cannot judge a capability
	// it cannot see, and a dormant supersession would otherwise spring on
	// whatever registers under that name later. Register the target
	// first, then load. nil admits nothing outside the prefix.
	MaySupersede registry.Filter
}

func (o LoadOptions) limits() script.Limits {
	if o.Limits == (script.Limits{}) {
		return script.DefaultLimits
	}
	return o.Limits
}

// Loaded describes one installed capability file.
type Loaded struct {
	Prefix       string              `json:"prefix"`
	Ref          string              `json:"ref"`          // the contribution, "owner#id"; Origin.Ref of what it installed
	Digest       string              `json:"digest"`       // content address of the file, its direct load() deps and its types; see Script
	Modules      []modules.Ref       `json:"modules"`      // every module the file loaded, transitively, sorted by path
	Capabilities []string            `json:"capabilities"` // refs installed, in definition order
	Diagnostics  []bundle.Diagnostic `json:"diagnostics,omitempty"`
}

// compilers returns the compilers Load and Watch give bundle.Load: the
// Script compiler, and one that accepts craggy.script-test@1
// (startest.Test) contributions without checking or running them, so a
// bundle can hold its capability files' tests beside them for Test.
func compilers(opts LoadOptions) []bundle.AnyCompiler {
	skipTests := func(context.Context, bundle.Contribution) (bundle.Compiled[*starcompile.Unit], []bundle.Diagnostic) {
		return bundle.Compiled[*starcompile.Unit]{}, nil
	}
	return []bundle.AnyCompiler{compiler(opts), bundle.Compile(startest.Test, skipTests)}
}

// compiler is the Script compiler under opts.
func compiler(opts LoadOptions) bundle.AnyCompiler {
	lim := opts.limits()
	sopts := starcompile.Options{
		Check:             check.Env{Predeclared: []string{"capability", "schema"}, Noun: noun},
		ModulePredeclared: sl.StringDict{"schema": schema.Module()},
		Limits:            lim,
	}
	return bundle.Compile(Script, func(ctx context.Context, c bundle.Contribution) (bundle.Compiled[*File], []bundle.Diagnostic) {
		if !registry.ValidName(c.Ref.ID) || strings.Contains(c.Ref.ID, ".") {
			return bundle.Compiled[*File]{}, []bundle.Diagnostic{contributionError(c, CodePrefix, c.Path,
				"capability file id %q must be a capability name without \".\": one file defines one top-level prefix", c.Ref.ID)}
		}
		u, diags := starcompile.New(ctx, c, sopts)
		if u == nil {
			return bundle.Compiled[*File]{}, diags
		}
		f, more := compileFile(ctx, u, opts, lim)
		if f == nil {
			return bundle.Compiled[*File]{}, append(diags, more...)
		}
		f.info.Diagnostics = diags // check warnings: errors stopped New
		return bundle.Compiled[*File]{Value: f}, diags
	})
}

func contributionError(c bundle.Contribution, code bundle.Code, path, format string, args ...any) bundle.Diagnostic {
	return bundle.Diagnostic{
		Code: code, Severity: bundle.SeverityError, Bundle: c.Bundle.ID, Contribution: c.Ref.ID,
		Path: path, Message: fmt.Sprintf(format, args...),
	}
}

// compileFile runs u's top level with a fresh capability() collector.
func compileFile(ctx context.Context, u *starcompile.Unit, opts LoadOptions, lim script.Limits) (*File, []bundle.Diagnostic) {
	c := u.Contribution()
	schemaOpts, typesDigest, diags := typesFor(ctx, c, opts)
	if diags != nil {
		return nil, diags
	}
	schemaMod := schema.Module(schemaOpts...)
	var print func(*sl.Thread, string)
	if opts.Print != nil {
		print = func(th *sl.Thread, msg string) { opts.Print(script.PositionOf(th.CallFrame(1).Pos), msg) }
	}
	prefix := c.Ref.ID
	var (
		defs []registry.Capability
		// done is set once the file's top level has run. capability() may
		// be captured in a global and called later from a handler; after
		// done it fails instead of appending to defs from another
		// goroutine.
		done atomic.Bool
	)
	collect := func(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
		if done.Load() {
			return nil, fmt.Errorf("capability() is only valid while a file loads")
		}
		cp, err := buildCapability(args, kwargs, lim, print, schemaOpts)
		if err != nil {
			return nil, err
		}
		if !underPrefix(prefix, cp.Name) {
			return nil, fmt.Errorf("capability %s is outside the prefix %q", cp.Name, prefix)
		}
		defs = append(defs, cp)
		return sl.None, nil
	}
	ex, err := u.Exec(ctx, starcompile.ExecOptions{
		Predeclared: sl.StringDict{
			"capability": sl.NewBuiltin("capability", collect),
			"schema":     schemaMod,
		},
		ModulePredeclared: sl.StringDict{"schema": schemaMod},
		Print:             opts.Print,
	})
	done.Store(true)
	if err != nil {
		return nil, u.Diagnostics(err)
	}
	digest := fileDigest(u.Ref().Digest, typesDigest)
	origin := registry.Origin{Kind: originKind, Ref: c.Ref.String(), Digest: digest}
	refs := make([]string, len(defs))
	for i := range defs {
		defs[i].Origin = origin
		refs[i] = defs[i].Ref()
	}
	return &File{info: Loaded{Prefix: prefix, Ref: origin.Ref, Digest: digest, Modules: ex.Modules, Capabilities: refs}, caps: defs}, nil
}

// typesFor returns the schema options and types digest for c: its ".go"
// asset, else LoadOptions.Types.
func typesFor(ctx context.Context, c bundle.Contribution, opts LoadOptions) ([]schema.Option, string, []bundle.Diagnostic) {
	var decls []bundle.Asset
	for _, a := range c.Assets {
		if strings.HasSuffix(a.Path(), ".go") {
			decls = append(decls, a)
		}
	}
	switch {
	case len(decls) == 0 && opts.Types == nil:
		return nil, "", nil
	case len(decls) == 0:
		return []schema.Option{schema.WithTypes(opts.Types)}, opts.TypesDigest, nil
	case opts.Types != nil:
		return nil, "", []bundle.Diagnostic{contributionError(c, CodeTypes, decls[0].Path(),
			"the host sets LoadOptions.Types, so a capability file may not declare its own types")}
	case len(decls) > 1:
		return nil, "", []bundle.Diagnostic{contributionError(c, CodeTypes, decls[1].Path(),
			"a capability file takes at most one types asset (.go), not %d", len(decls))}
	}
	src, err := decls[0].Read(ctx)
	if err != nil {
		return nil, "", []bundle.Diagnostic{contributionError(c, CodeTypes, decls[0].Path(), "%v", err)}
	}
	pkg, err := types.Parse(decls[0].Path(), src)
	if err != nil {
		return nil, "", []bundle.Diagnostic{contributionError(c, CodeTypes, decls[0].Path(), "%v", err)}
	}
	return []schema.Option{schema.WithTypes(pkg)}, TypesDigest(src), nil
}

// Load loads the bundles in sources with bundle.Load, then installs every
// capability file they declare (bundle.All, so override targets are
// skipped), each with r.Replace(prefix, …), in catalog order. Reloading is
// calling Load again; unloading a file is r.Replace(its prefix).
//
// A failed load leaves the registry unchanged. A bundle that does not load
// (a missing or invalid bundle.yaml, a file that fails its check or its
// top level) is registry.Invalid, listing every diagnostic with its
// position, and the *bundle.LoadError is in the error chain. When one file
// is refused at install, the files installed before it are rolled back.
//
// Every capability a file defines is named under its prefix (the compiler
// enforces it). It may supersede refs outside the prefix only as
// opts.MaySupersede allows (else registry.Denied); a bare ref supersedes
// every version of its target, so every registered version must be
// admitted.
//
// One file owns one prefix. Load refuses with registry.Conflict, naming
// the other file, when r.Replace(prefix) would remove a capability another
// file installed (its Origin has Kind "script" and a different Ref, the
// file's contribution "owner#id"). The same contribution reinstalls
// freely, and Go-registered capabilities under the prefix are replaced:
// a file "apk" replaces every apk.* capability. Unload the other file
// first (r.Replace(its prefix)) to move a prefix between bundles. The
// check and the Replace are not one atomic step: serialise concurrent
// loads of the same prefix.
//
// Each installed capability's Origin is {Kind: "script", Ref: "owner#id",
// Digest: Loaded.Digest}.
func Load(ctx context.Context, r *registry.Registry, sources []bundle.Source, opts LoadOptions) ([]Loaded, error) {
	cat, err := bundle.Load(ctx, bundle.Options{Sources: sources, Compilers: compilers(opts)})
	if err != nil {
		return nil, loadError(err)
	}
	defs := bundle.All(cat, Script)
	before := r.All()
	out := make([]Loaded, 0, len(defs))
	for i, def := range defs {
		l, err := install(r, def.Value, opts.MaySupersede, "")
		if err != nil {
			for j := i - 1; j >= 0; j-- { // roll back, newest first
				prefix := defs[j].Value.Prefix()
				var prev []registry.Capability
				for _, e := range before {
					if underPrefix(prefix, e.Name) {
						prev = append(prev, e.Capability)
					}
				}
				_ = r.Replace(prefix, prev...)
			}
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// install installs f with r.Replace(prefix, …). replacing is a previous
// Origin.Ref of the same prefix that f may replace without a conflict
// (Watch's own earlier install of a prefix that moved between bundles); ""
// allows none.
func install(r *registry.Registry, f *File, policy registry.Filter, replacing string) (Loaded, error) {
	prefix := f.Prefix()
	if err := ownPrefix(r, prefix, f.info.Ref, replacing); err != nil {
		return Loaded{}, err
	}
	all := r.View(registry.All)
	for _, c := range f.caps {
		for _, s := range c.Supersedes {
			if err := maySupersede(all, prefix, c.Name, s, policy); err != nil {
				return Loaded{}, err
			}
		}
	}
	if err := r.Replace(prefix, slices.Clone(f.caps)...); err != nil {
		return Loaded{}, err
	}
	l := f.info
	l.Modules, l.Capabilities, l.Diagnostics = slices.Clone(l.Modules), slices.Clone(l.Capabilities), slices.Clone(l.Diagnostics)
	return l, nil
}

// ownPrefix refuses an install of ref under prefix when Replace(prefix)
// would remove a capability another file installed, other than replacing.
func ownPrefix(r *registry.Registry, prefix, ref, replacing string) error {
	for _, e := range r.All() {
		if !underPrefix(prefix, e.Name) || e.Origin.Kind != originKind || e.Origin.Ref == ref || (replacing != "" && e.Origin.Ref == replacing) {
			continue
		}
		return registry.Errorf(registry.Conflict,
			"load %s: %s under the prefix %q was loaded from %s, and one file owns one prefix: unload %s first",
			ref, e.Capability.Ref(), prefix, e.Origin.Ref, e.Origin.Ref)
	}
	return nil
}

// loadError maps a failed bundle.Load to registry.Invalid, whose message
// is the *bundle.LoadError's list of diagnostics. A cancelled load stays a
// context error (registry.Cancelled).
func loadError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return registry.WrapCode(registry.Invalid, err)
}

// originKind is Origin.Kind for capabilities Load installs.
const originKind = "script"

// TypesDigest is the content address of a types file's source, for
// LoadOptions.TypesDigest: "sha256:" and the hex SHA-256 of src.
func TypesDigest(src []byte) string {
	h := sha256.Sum256(src)
	return "sha256:" + hex.EncodeToString(h[:])
}

// fileDigest is Loaded.Digest: the script digest alone without a types
// digest, else a SHA-256 under the domain tag "craggy-capability-file-v1"
// over both, each length-prefixed.
func fileDigest(script, types string) string {
	if types == "" {
		return script
	}
	h := sha256.New()
	var n [8]byte
	for _, s := range []string{"craggy-capability-file-v1", script, types} {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func underPrefix(prefix, name string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+".")
}

// maySupersede applies the supersession policy to one ref of capability
// name. A pinned ref is checked against that version; a bare ref
// supersedes every version, so each registered version must pass. A ref
// naming nothing registered is denied (fail closed: see
// LoadOptions.MaySupersede). Malformed refs are left for the registry to
// reject.
func maySupersede(all *registry.View, prefix, name, ref string, policy registry.Filter) error {
	target, _, pinned, ok := registry.ParseRef(ref)
	if !ok || underPrefix(prefix, target) {
		return nil
	}
	var targets []registry.Capability
	if pinned {
		if e, found := all.Get(ref); found {
			targets = append(targets, e.Capability)
		}
	} else {
		for _, e := range all.All() {
			if e.Name == target {
				targets = append(targets, e.Capability)
			}
		}
	}
	if len(targets) == 0 {
		return registry.Errorf(registry.Denied,
			"capability %s may not supersede %s: it is outside the prefix %q and is not registered, so LoadOptions.MaySupersede cannot judge it; register the target first", name, ref, prefix)
	}
	for _, c := range targets {
		if policy == nil || !policy(c) {
			what := ref
			if c.Handler != nil && !pinned {
				what = ref + " (" + c.Ref() + ")"
			}
			return registry.Errorf(registry.Denied,
				"capability %s may not supersede %s: it is outside the prefix %q and LoadOptions.MaySupersede does not allow it", name, what, prefix)
		}
	}
	return nil
}

// capabilityKwargs is the documented capability(...) parameter set.
// Anything else is an error: a misspelled kwarg must never load with the
// field silently unset (a dropped tags=[...] changes who can see the
// capability).
var capabilityKwargs = map[string]bool{
	"name": true, "version": true, "title": true, "description": true,
	"input": true, "output": true, "effects": true, "tags": true,
	"supersedes": true, "accepts": true, "emits": true, "long": true,
	"heartbeat": true, "run": true, "check": true,
}

func buildCapability(args sl.Tuple, kwargs []sl.Tuple, lim script.Limits, print func(*sl.Thread, string), schemaOpts []schema.Option) (registry.Capability, error) {
	kw := map[string]sl.Value{}
	for _, kv := range kwargs {
		key := string(kv[0].(sl.String))
		if !capabilityKwargs[key] {
			return registry.Capability{}, fmt.Errorf("capability: unknown parameter %q", key)
		}
		kw[key] = kv[1]
	}
	// The documented signature is capability(name, ...): one positional
	// name is allowed (and must agree with name= when both are given).
	if len(args) > 1 {
		return registry.Capability{}, fmt.Errorf("capability: at most one positional argument (name), got %d", len(args))
	}
	if len(args) == 1 {
		s, ok := args[0].(sl.String)
		if !ok {
			return registry.Capability{}, fmt.Errorf("capability: name must be a string")
		}
		if v, dup := kw["name"]; dup {
			if vs, isStr := v.(sl.String); !isStr || vs != s {
				return registry.Capability{}, fmt.Errorf("capability: name given both positionally and by keyword")
			}
		} else {
			kw["name"] = s
		}
	}
	getString := func(key string) (string, error) {
		v, ok := kw[key]
		if !ok || v == sl.None {
			return "", nil
		}
		s, ok := v.(sl.String)
		if !ok {
			return "", fmt.Errorf("capability: %s must be a string", key)
		}
		return string(s), nil
	}
	getStrings := func(key string) ([]string, error) {
		v, ok := kw[key]
		if !ok || v == sl.None {
			return nil, nil
		}
		gv, err := values.ToGo(v)
		if err != nil {
			return nil, fmt.Errorf("capability: %s: %w", key, err)
		}
		lst, ok := gv.([]any)
		if !ok {
			return nil, fmt.Errorf("capability: %s must be a list of strings", key)
		}
		out := make([]string, 0, len(lst))
		for _, e := range lst {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("capability: %s must be a list of strings", key)
			}
			out = append(out, s)
		}
		return out, nil
	}

	name, err := getString("name")
	if err != nil {
		return registry.Capability{}, err
	}
	if name == "" {
		return registry.Capability{}, fmt.Errorf("capability: name is required")
	}
	c := registry.Capability{Name: name, Version: 1}
	if c.Title, err = getString("title"); err != nil {
		return c, err
	}
	if c.Description, err = getString("description"); err != nil {
		return c, err
	}
	if v, ok := kw["version"]; ok {
		n, ok := v.(sl.Int)
		if !ok {
			return c, fmt.Errorf("capability: version must be an int")
		}
		iv, ok := n.Int64()
		if !ok || iv < 0 || iv > math.MaxInt32 {
			return c, fmt.Errorf("capability: version %s is out of range", n.String())
		}
		c.Version = int(iv)
	}
	if v, ok := kw["input"]; ok && v != sl.None {
		if c.Input, err = schema.Of(v, schemaOpts...); err != nil {
			return c, fmt.Errorf("capability %s input: %w", name, err)
		}
	}
	if v, ok := kw["output"]; ok && v != sl.None {
		if c.Output, err = schema.Of(v, schemaOpts...); err != nil {
			return c, fmt.Errorf("capability %s output: %w", name, err)
		}
	}
	effectNames, err := getStrings("effects")
	if err != nil {
		return c, err
	}
	if c.Effects, err = registry.ParseEffects(effectNames); err != nil {
		return c, err
	}
	if c.Tags, err = getStrings("tags"); err != nil {
		return c, err
	}
	if c.Supersedes, err = getStrings("supersedes"); err != nil {
		return c, err
	}
	if c.Accepts, err = getSubjects(kw, "accepts"); err != nil {
		return c, err
	}
	if c.Emits, err = getSubjects(kw, "emits"); err != nil {
		return c, err
	}
	if v, ok := kw["long"]; ok {
		b, ok := v.(sl.Bool)
		if !ok {
			return c, fmt.Errorf("capability: long must be a bool")
		}
		c.Long = bool(b)
	}
	if v, ok := kw["heartbeat"]; ok {
		f, okF := sl.AsFloat(v)
		if !okF || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > float64(math.MaxInt64)/float64(time.Second) {
			return c, fmt.Errorf("capability: heartbeat must be a number of seconds in range")
		}
		c.Heartbeat = time.Duration(f * float64(time.Second))
	}
	runV, ok := kw["run"]
	if !ok {
		return c, fmt.Errorf("capability %s: run is required", name)
	}
	runFn, ok := runV.(*sl.Function)
	if !ok {
		return c, fmt.Errorf("capability %s: run must be a function", name)
	}
	if v, ok := kw["check"]; ok && v != sl.None {
		checkFn, ok := v.(*sl.Function)
		if !ok {
			return c, fmt.Errorf("capability %s: check must be a function", name)
		}
		c.Check = makeCheck(checkFn, lim, print)
	}
	c.Handler = makeHandler(name, runFn, lim, print)
	return c, nil
}

// getSubjects reads accepts or emits. Each entry is a type string, or a
// dict with "type" and an optional "level". Both values stay opaque.
func getSubjects(kw map[string]sl.Value, key string) ([]registry.Subject, error) {
	v, ok := kw[key]
	if !ok || v == sl.None {
		return nil, nil
	}
	gv, err := values.ToGo(v)
	if err != nil {
		return nil, fmt.Errorf("capability: %s: %w", key, err)
	}
	lst, ok := gv.([]any)
	if !ok {
		return nil, fmt.Errorf("capability: %s must be a list", key)
	}
	out := make([]registry.Subject, 0, len(lst))
	for i, e := range lst {
		switch t := e.(type) {
		case string:
			out = append(out, registry.Subject{Type: t})
		case map[string]any:
			typ, _ := t["type"].(string)
			if typ == "" {
				return nil, fmt.Errorf("capability: %s[%d] needs a type", key, i)
			}
			lvl := ""
			if raw, ok := t["level"]; ok && raw != nil {
				s, ok := raw.(string)
				if !ok {
					return nil, fmt.Errorf("capability: %s[%d].level must be a string", key, i)
				}
				lvl = s
			}
			out = append(out, registry.Subject{Type: typ, Level: lvl})
		default:
			return nil, fmt.Errorf("capability: %s entries must be strings or {type, level} dicts", key)
		}
	}
	return out, nil
}

func makeHandler(name string, fn *sl.Function, lim script.Limits, print func(*sl.Thread, string)) registry.Handler {
	return func(ctx context.Context, in json.RawMessage) (any, error) {
		input, err := values.FromJSON(in)
		if err != nil {
			return nil, registry.WrapCode(registry.Invalid, err)
		}
		tctx, stop := withTimeLimit(ctx, lim.Timeout)
		defer stop()
		rc := &runCtx{ctx: tctx, name: name}
		out, _, err := script.Call(ctx, lim, name, fn, sl.Tuple{rc, input}, print)
		if err != nil {
			return nil, handlerError(ctx, tctx, name, err)
		}
		raw, err := values.ToJSON(out)
		if err != nil {
			return nil, registry.Errorf(registry.Internal, "%s: result not JSON-convertible: %v", name, err)
		}
		return raw, nil
	}
}

func makeCheck(fn *sl.Function, lim script.Limits, print func(*sl.Thread, string)) func(context.Context) error {
	return func(ctx context.Context) error {
		tctx, stop := withTimeLimit(ctx, lim.Timeout)
		defer stop()
		rc := &runCtx{ctx: tctx, name: "check"}
		out, _, err := script.Call(ctx, lim, "check", fn, sl.Tuple{rc}, print)
		if err != nil {
			if timedOut(ctx, tctx) {
				return errors.New(script.TimeLimitMessage)
			}
			return err
		}
		switch v := out.(type) {
		case sl.NoneType:
			return nil
		case sl.Bool:
			if bool(v) {
				return nil
			}
			return fmt.Errorf("not ready")
		case sl.String:
			if v == "" {
				return nil
			}
			return fmt.Errorf("%s", string(v))
		default:
			return fmt.Errorf("check returned %s, want None, bool or string", out.Type())
		}
	}
}

// handlerError maps a failed run function: cancelled when the job's
// context ended, else internal (the script is at fault, not the caller),
// with the script position in the message. tctx is the time-limited
// context the script's ctx.call and ctx.start ran under: a call it cut
// short is the script's time limit, not a cancellation.
func handlerError(ctx, tctx context.Context, name string, err error) error {
	if ctx.Err() != nil {
		return registry.WrapCode(registry.Cancelled, ctx.Err())
	}
	var se *script.Error
	errors.As(err, &se)
	msg := err.Error()
	if se != nil {
		msg = se.Message
	}
	if timedOut(ctx, tctx) {
		msg = script.TimeLimitMessage
	}
	if se != nil && se.Pos.Line > 0 {
		msg = fmt.Sprintf("%s:%d:%d: %s", se.Pos.File, se.Pos.Line, se.Pos.Col, msg)
	}
	return &registry.Error{Code: registry.Internal, Message: name + ": " + msg, Err: err}
}

// withTimeLimit derives the context a script's capability calls run under:
// ctx bounded by timeout (0 means ctx alone). Starlark cannot interrupt a
// built-in that is blocked waiting, so the calls themselves must see the
// time limit.
func withTimeLimit(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// timedOut reports whether the time limit, not the caller, ended tctx.
func timedOut(ctx, tctx context.Context) bool {
	return ctx.Err() == nil && errors.Is(tctx.Err(), context.DeadlineExceeded)
}

// runCtx is the `ctx` value passed to script run/check functions.
type runCtx struct {
	ctx  context.Context
	name string
}

func (rc *runCtx) String() string        { return "<ctx " + rc.name + ">" }
func (rc *runCtx) Type() string          { return "run-ctx" }
func (rc *runCtx) Freeze()               {}
func (rc *runCtx) Truth() sl.Bool        { return sl.True }
func (rc *runCtx) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: ctx") }
func (rc *runCtx) Attr(name string) (sl.Value, error) {
	switch name {
	case "job_id":
		id, _ := registry.JobIDFrom(rc.ctx)
		return sl.String(id), nil
	case "caller":
		c, ok := registry.CallerFrom(rc.ctx)
		if !ok {
			return sl.None, nil
		}
		d := new(sl.Dict)
		_ = d.SetKey(sl.String("id"), sl.String(c.ID))
		_ = d.SetKey(sl.String("kind"), sl.String(c.Kind))
		_ = d.SetKey(sl.String("session"), sl.String(c.Session))
		return d, nil
	case "progress":
		return sl.NewBuiltin("ctx.progress", rc.progress).BindReceiver(rc), nil
	case "call":
		return sl.NewBuiltin("ctx.call", rc.call).BindReceiver(rc), nil
	case "start":
		return sl.NewBuiltin("ctx.start", rc.start).BindReceiver(rc), nil
	}
	return nil, nil
}
func (rc *runCtx) AttrNames() []string {
	return []string{"call", "caller", "job_id", "progress", "start"}
}

var _ sl.HasAttrs = (*runCtx)(nil)

func (rc *runCtx) progress(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	var stage, message, unit string
	var done, total int64
	var data sl.Value
	if err := sl.UnpackArgs("ctx.progress", args, kwargs,
		"stage?", &stage, "message?", &message, "done?", &done, "total?", &total, "unit?", &unit, "data?", &data); err != nil {
		return nil, err
	}
	u := registry.Update{Stage: stage, Message: message, Done: done, Total: total, Unit: unit}
	if data != nil && data != sl.None {
		gv, err := values.ToGo(data)
		if err != nil {
			return nil, err
		}
		u.Data = gv
	}
	registry.Progress(rc.ctx, u)
	return sl.None, nil
}

// call invokes through the *caller's* view (ViewFrom the handler ctx), so a
// script can never widen its caller's access. A call cut short by the
// script's time limit (or the job's cancellation) cancels its job.
func (rc *runCtx) call(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	var ref string
	if err := sl.UnpackPositionalArgs("ctx.call", args, nil, 1, &ref); err != nil {
		return nil, err
	}
	v := registry.ViewFrom(rc.ctx)
	if v == nil {
		return nil, registry.Errorf(registry.Unavailable, "no view for this call")
	}
	return invokeVia(thread, rc.ctx, v, ref, nil, kwargs, true)
}

func (rc *runCtx) start(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	var ref string
	if err := sl.UnpackPositionalArgs("ctx.start", args, nil, 1, &ref); err != nil {
		return nil, err
	}
	v := registry.ViewFrom(rc.ctx)
	if v == nil {
		return nil, registry.Errorf(registry.Unavailable, "no view for this call")
	}
	val, _, err := startJob(rc.ctx, v, ref, kwargs)
	return val, err
}
