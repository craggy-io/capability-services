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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sl "go.starlark.net/starlark"

	"github.com/craggy-io/script-services/check"
	"github.com/craggy-io/script-services/modules"
	"github.com/craggy-io/script-services/schema"
	"github.com/craggy-io/script-services/script"
	"github.com/craggy-io/script-services/types"
	"github.com/craggy-io/script-services/values"

	"github.com/craggy-io/capability-services/registry"
)

// Source is a capability file plus where its load() statements resolve.
type Source struct {
	Filename string         // names the file in positions and Origin; base for "./" load paths
	Src      []byte         // the file's text
	Modules  modules.Source // serves load(); nil means the file may not load modules
}

// LoadOptions configure Load.
type LoadOptions struct {
	// Limits bound the file's top level and each invocation of a
	// capability (or readiness check) it defines, on a fresh thread per
	// call. The zero value means script.DefaultLimits.
	Limits script.Limits
	// Print receives print() output and superseded warnings, from the
	// file's top level and from its capabilities. nil discards it.
	Print func(pos script.Position, msg string)
	// Types makes Go-syntax type declarations available: schema.type("Name")
	// and input = "Name" / output = "Name" (see schema.WithTypes).
	Types *types.Package
	// TypesDigest is the content address of the source Types was parsed
	// from (use TypesDigest(src)). When set it is folded into
	// Loaded.Digest, so a changed declaration changes the digest of every
	// file loaded with it. A *types.Package keeps no source, so Load cannot
	// compute this itself: with Types and no TypesDigest, the digest does
	// not cover the declarations. Watch fills it for its types.go.
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

// Loaded describes a successful Load.
type Loaded struct {
	Prefix       string             `json:"prefix"`
	Digest       string             `json:"digest"`       // content address of the file, its direct load() deps and LoadOptions.TypesDigest; see Load
	Modules      []modules.Ref      `json:"modules"`      // every module the file loaded, transitively, sorted by path
	Capabilities []string           `json:"capabilities"` // refs installed, in definition order
	Diagnostics  []check.Diagnostic `json:"diagnostics,omitempty"`
}

// noEntry is an Entry name no Starlark global can have: Load runs a file's
// top level only.
const noEntry = "-"

// Load checks a capability file, executes it, and atomically installs the
// capabilities it defines with r.Replace(prefix, …): reloading is calling
// Load again, unloading is r.Replace(prefix). A failed load leaves the
// registry unchanged.
//
// Before anything runs, the file is checked (check.Analyze, with load()
// targets looked up in src.Modules); any error diagnostic rejects it with
// registry.Invalid and a message listing the positioned diagnostics.
// Every capability must be named under prefix (else Invalid), and may
// supersede refs outside prefix only as LoadOptions.MaySupersede allows
// (else Denied); a bare ref supersedes every version of its target, so
// every registered version must be admitted.
//
// One file owns one prefix. Load refuses with Conflict, naming the other
// file, when r.Replace(prefix) would remove a capability another file
// installed with Load (its Origin has Kind "script" and a different Ref),
// as loading "a" from a.star would wipe a.b.* loaded from a.b.star — and
// likewise when prefix overlaps a prefix another file already loaded (an
// ancestor or descendant, in either order), which Load tracks per
// registry. The same file reloads freely, and Go-registered capabilities
// under the prefix are replaced as before. Unload the other file first
// (r.Replace(its prefix)) to move capabilities between files. The check
// and the Replace are not one atomic step: serialise concurrent Loads of
// overlapping prefixes (Watch loads one file at a time).
//
// Each installed capability's Origin is {Kind: "script", Ref:
// src.Filename, Digest: Loaded.Digest}. Loaded.Digest is the file's script
// digest (script.Result.Digest: modules.Digest over its source and each
// direct load() dependency, spec paired with the module's digest;
// transitive modules count through their parents' digests). With
// LoadOptions.TypesDigest set it is instead a SHA-256 over that script
// digest and the types digest (see fileDigest).
//
// The file and the modules it loads see `schema` (see schema.Module); the
// file also sees
//
//	capability(name, version=1, title="", description="", input=None,
//	           output=None, effects=[], tags=[], supersedes=[], accepts=[],
//	           emits=[], long=False, heartbeat=0, run=<fn>, check=None)
//
// input/output accept schema.* values, a field dict, a raw JSON Schema
// dict, a JSON string, or with LoadOptions.Types a declared type's name.
// effects are names ("read_only", "idempotent", "destructive",
// "open_world"). heartbeat is in seconds. accepts/emits entries are type
// strings or {"type", "level"} dicts. run(ctx, input) returns a
// JSON-shaped value; check(ctx) returns None/True for ready, or fails /
// returns False or a message for not ready. ctx has job_id, caller,
// progress(stage=, message=, done=, total=, unit=, data=), and call(ref,
// **kwargs) / start(ref, **kwargs), which go through the *caller's* view.
func Load(ctx context.Context, r *registry.Registry, prefix string, src Source, opts LoadOptions) (Loaded, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	filename := src.Filename
	if filename == "" {
		filename = "script.star"
	}
	lim := opts.Limits
	if lim == (script.Limits{}) {
		lim = script.DefaultLimits
	}
	var schemaOpts []schema.Option
	if opts.Types != nil {
		schemaOpts = append(schemaOpts, schema.WithTypes(opts.Types))
	}
	schemaMod := schema.Module(schemaOpts...)

	// Check before anything runs.
	a := check.Analyze(check.Env{Predeclared: []string{"capability", "schema"}, Modules: src.Modules, Noun: noun}, filename, src.Src)
	var errs, warnings []check.Diagnostic
	for _, d := range a.Diagnostics {
		if d.Severity == "error" {
			errs = append(errs, d)
		} else {
			warnings = append(warnings, d)
		}
	}
	if len(errs) > 0 {
		return Loaded{}, registry.Errorf(registry.Invalid, "load %s: check failed:\n%s", filename, formatDiagnostics(filename, errs))
	}

	var print func(*sl.Thread, string)
	if opts.Print != nil {
		print = func(th *sl.Thread, msg string) { opts.Print(script.PositionOf(th.CallFrame(1).Pos), msg) }
	}
	all := r.View(registry.All)
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
		c, err := buildCapability(args, kwargs, lim, print, schemaOpts)
		if err != nil {
			return nil, err
		}
		if !underPrefix(prefix, c.Name) {
			return nil, registry.Errorf(registry.Invalid, "capability %s is outside the prefix %q", c.Name, prefix)
		}
		for _, ref := range c.Supersedes {
			if err := maySupersede(all, prefix, c.Name, ref, opts.MaySupersede); err != nil {
				return nil, err
			}
		}
		defs = append(defs, c)
		return sl.None, nil
	}
	var loader *modules.Loader
	if src.Modules != nil {
		loader = modules.NewLoader(src.Modules, modules.Options{
			Predeclared: sl.StringDict{"schema": schemaMod},
			MaxSteps:    lim.MaxSteps,
		})
	}
	res, err := script.Exec(ctx, script.Program{
		Filename: filename,
		Source:   src.Src,
		Predeclared: sl.StringDict{
			"capability": sl.NewBuiltin("capability", collect),
			"schema":     schemaMod,
		},
		Entry:   noEntry,
		Modules: loader,
	}, script.Options{Limits: lim, Print: opts.Print})
	done.Store(true)
	if err != nil {
		return Loaded{}, loadError(filename, err)
	}

	digest := fileDigest(res.Digest, opts.TypesDigest)
	origin := registry.Origin{Kind: originKind, Ref: filename, Digest: digest}
	refs := make([]string, 0, len(defs))
	for i := range defs {
		defs[i].Origin = origin
		refs = append(refs, defs[i].Ref())
	}
	if err := ownPrefix(r, prefix, filename); err != nil {
		return Loaded{}, err
	}
	// Remember what Replace is about to remove, to roll back if the
	// install-time supersession recheck fails.
	var prev []registry.Capability
	for _, e := range r.All() {
		if underPrefix(prefix, e.Name) {
			prev = append(prev, e.Capability)
		}
	}
	if err := r.Replace(prefix, defs...); err != nil {
		return Loaded{}, err
	}
	// The policy was checked while the file's top level ran, which can be
	// minutes before this install: a target registered or re-tagged in
	// between must pass it too. Re-check against the registry as it now
	// stands and roll the install back on failure.
	for _, c := range defs {
		for _, ref := range c.Supersedes {
			if err := maySupersede(all, prefix, c.Name, ref, opts.MaySupersede); err != nil {
				_ = r.Replace(prefix, prev...)
				return Loaded{}, err
			}
		}
	}
	ownersFor(r).record(prefix, filename)
	return Loaded{Prefix: prefix, Digest: digest, Modules: res.Modules, Capabilities: refs, Diagnostics: warnings}, nil
}

// prefixOwners records, per registry, which file loaded which prefix.
// Installed capabilities alone cannot answer that: a file that owns "a"
// but defines only a.x leaves nothing under "a.b" for a nested a.b.star
// to collide with, yet the nested load would make the owner's next
// reload wipe a.b.* — refused here in either order. Entries whose file
// no longer has anything installed under its prefix are pruned on the
// next check, so unloading (r.Replace(prefix), which bypasses Load)
// frees the prefix again.
var prefixOwners sync.Map // *registry.Registry -> *ownerSet

type ownerSet struct {
	mu       sync.Mutex
	byPrefix map[string]string // prefix -> filename
}

func ownersFor(r *registry.Registry) *ownerSet {
	v, _ := prefixOwners.LoadOrStore(r, &ownerSet{byPrefix: map[string]string{}})
	return v.(*ownerSet)
}

func (o *ownerSet) record(prefix, filename string) {
	o.mu.Lock()
	o.byPrefix[prefix] = filename
	o.mu.Unlock()
}

// overlapsOther reports a recorded owner (prefix, file) of a prefix that
// overlaps prefix (either is the other or under it), pruning records
// whose file has nothing installed under its prefix any more.
func (o *ownerSet) overlapsOther(r *registry.Registry, prefix, filename string) (string, string, bool) {
	installed := func(p, file string) bool {
		for _, e := range r.All() {
			if underPrefix(p, e.Name) && e.Origin.Kind == originKind && e.Origin.Ref == file {
				return true
			}
		}
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for p, file := range o.byPrefix {
		if !installed(p, file) {
			delete(o.byPrefix, p)
			continue
		}
		if file != filename && (underPrefix(p, prefix) || underPrefix(prefix, p)) {
			return p, file, true
		}
	}
	return "", "", false
}

// ownPrefix refuses a load of filename under prefix when Replace(prefix)
// would remove a capability another file installed with Load, or when
// prefix overlaps a prefix another file loaded.
func ownPrefix(r *registry.Registry, prefix, filename string) error {
	for _, e := range r.All() {
		if underPrefix(prefix, e.Name) && e.Origin.Kind == originKind && e.Origin.Ref != filename {
			return registry.Errorf(registry.Conflict,
				"load %s: %s under the prefix %q was loaded from %s, and one file owns one prefix: unload %s first",
				filename, e.Capability.Ref(), prefix, e.Origin.Ref, e.Origin.Ref)
		}
	}
	if p, file, ok := ownersFor(r).overlapsOther(r, prefix, filename); ok {
		return registry.Errorf(registry.Conflict,
			"load %s: the prefix %q overlaps %q, which %s owns, and one file owns one prefix: unload %s first",
			filename, prefix, p, file, file)
	}
	return nil
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

func formatDiagnostics(filename string, ds []check.Diagnostic) string {
	lines := make([]string, len(ds))
	for i, d := range ds {
		file := d.Pos.File
		if file == "" {
			file = filename
		}
		lines[i] = fmt.Sprintf("%s:%d:%d: %s: %s", file, d.Pos.Line, d.Pos.Col, d.Severity, d.Message)
	}
	return strings.Join(lines, "\n")
}

// loadError maps a failed file execution to a registry error: a coded
// error raised by capability(...) (outside the prefix, denied supersession)
// keeps its code, anything else is Invalid. The message is positioned.
func loadError(filename string, err error) error {
	code, msg := registry.Invalid, err.Error()
	var se *script.Error
	if errors.As(err, &se) {
		msg = se.Message
		var re *registry.Error
		switch {
		case se.Code == script.Cancelled:
			code = registry.Cancelled
		case errors.As(err, &re):
			code, msg = re.Code, re.Message
		}
		if se.Pos.Line > 0 {
			msg = fmt.Sprintf("%s:%d:%d: %s", se.Pos.File, se.Pos.Line, se.Pos.Col, msg)
		}
	}
	return &registry.Error{Code: code, Message: "load " + filename + ": " + msg, Err: err}
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
