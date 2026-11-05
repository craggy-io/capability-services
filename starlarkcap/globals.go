// Package starlarkcap is the Starlark projection, in both directions:
// Globals exposes a view's capabilities to scripts as nested built-ins, Exec
// runs a script against them (Check validates one without running it), and
// capability files define capabilities: Load installs them from bundles,
// Watch hot-reloads bundles through a bundle.Live, and Test runs their
// Starlark tests.
//
// Capability files are read only from bundles (foundation-kit's bundle
// package): a folder needs a bundle.yaml that declares each file as a
// capability.script@1 contribution (Script) whose id is the file's prefix.
// Nothing is discovered from file names.
//
// Everything generic (the dialect, value conversion, schemas, limits,
// load(), static checks, the shared .star compiler, the test runner) comes
// from github.com/craggy-io/script-services; this package adds the
// registry.
//
// Calling: storage.artifact.get(id = "…") — keyword arguments become the
// input object; results come back as Starlark dicts/lists/scalars.
// call("storage.artifact.get@1", id = "…") reaches pinned versions and
// superseded entries. Long capabilities block (waiting consumes no steps);
// jobs.start(ref, **kwargs) returns a job value with .id, .status(),
// .wait(timeout_seconds = N) and .cancel() for concurrent work.
//
// Defining (see Script): capability(name = "apk.decompile",
// description = "...", supersedes = [...], long = True,
// accepts = [{"type": "android.apk", "level": "validated"}],
// input = {"artifact": schema.string(required = True)}, run = _impl)
// where _impl(ctx, input) returns a JSON-shaped value and ctx provides
// ctx.progress(stage=, message=, done=, total=, unit=, data=),
// ctx.call(ref, **kwargs) through the *caller's* view, ctx.job_id and
// ctx.caller.
package starlarkcap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	sl "go.starlark.net/starlark"

	"github.com/craggy-io/script-services/values"

	"github.com/craggy-io/capability-services/registry"
)

// Globals exposes the capabilities in ViewFrom(ctx) to scripts. It takes a
// snapshot when called, so one script run sees one consistent set of
// names: namespaces come from List, and call() and jobs.start() refs must
// resolve in All as it was then (a capability registered later is
// not_found). A bare ref must name a capability in the snapshot, a pinned
// one that exact version. Calls then execute against the live view with
// the given ctx (its Caller flows through): the version that runs is the
// one the ref names now, and a capability removed since fails with
// not_found. With no view in ctx, the returned built-ins fail with
// unavailable.
//
// Three names are reserved beside the namespaces: call (the built-in),
// jobs.start (the built-in wins over a capability named jobs.start), and,
// in Exec, input. Capabilities under those names are still reachable by
// ref, e.g. call("jobs.start") or call("input.x"); their namespaces are
// shadowed.
func Globals(ctx context.Context) sl.StringDict {
	return newGlobals(ctx, nil).dict()
}

type globals struct {
	ctx   context.Context
	view  *registry.View            // nil when ctx carried none
	all   map[string]registry.Entry // snapshot of v.All(), by name@version
	names map[string]bool           // names in all
	obs   *observer                 // Exec's call hooks and job tracking; nil for plain Globals
}

func newGlobals(ctx context.Context, obs *observer) *globals {
	g := &globals{ctx: ctx, obs: obs}
	if v := registry.ViewFrom(ctx); v != nil {
		g.view = v
		g.all = map[string]registry.Entry{}
		g.names = map[string]bool{}
		for _, e := range v.All() {
			g.all[e.Capability.Ref()] = e
			g.names[e.Name] = true
		}
	}
	return g
}

// inSnapshot checks a call() or jobs.start() ref against the snapshot: a
// bare name must have been registered, a pinned ref that exact version.
func (g *globals) inSnapshot(ref string) error {
	name, ver, pinned, ok := registry.ParseRef(ref)
	if !ok {
		return registry.Errorf(registry.Invalid, "bad ref %q", ref)
	}
	if pinned {
		if _, ok := g.all[fmt.Sprintf("%s@%d", name, ver)]; ok {
			return nil
		}
	} else if g.names[name] {
		return nil
	}
	return registry.Errorf(registry.NotFound, "capability %q not found", ref)
}

// capNode is one node of the namespace tree. A node may carry an Entry
// (a capability ends here) and children (capabilities below it) at once:
// a.b can be callable while a.b.c also exists.
type capNode struct {
	entry    *registry.Entry
	children map[string]*capNode
}

// capabilityTree nests entries by dotted name. Check builds its
// environment from the same v.List(), so what Check accepts is what a
// script can call.
func capabilityTree(entries []registry.Entry) map[string]*capNode {
	roots := map[string]*capNode{}
	for _, e := range entries {
		parts := strings.Split(e.Name, ".")
		level := roots
		var node *capNode
		for _, p := range parts {
			node = level[p]
			if node == nil {
				node = &capNode{children: map[string]*capNode{}}
				level[p] = node
			}
			level = node.children
		}
		entry := e
		node.entry = &entry
	}
	return roots
}

// dict builds the predeclared names: one namespace per top-level
// capability segment, call(), and jobs.start.
func (g *globals) dict() sl.StringDict {
	root := sl.StringDict{}
	if g.view != nil {
		for k, node := range capabilityTree(g.view.List()) {
			root[k] = g.buildNode(k, node)
		}
	}
	jobsNS, isNS := root["jobs"].(*namespace)
	if !isNS {
		jobsNS = &namespace{name: "jobs", attrs: map[string]sl.Value{}}
		root["jobs"] = jobsNS
	}
	jobsNS.attrs["start"] = sl.NewBuiltin("jobs.start", g.jobsStart)
	root["call"] = sl.NewBuiltin("call", g.callBuiltin)
	return root
}

func (g *globals) requireView() (*registry.View, error) {
	if g.view == nil {
		return nil, registry.Errorf(registry.Unavailable, "no registry view in this script context")
	}
	return g.view, nil
}

func (g *globals) buildNode(path string, node *capNode) sl.Value {
	call := func(thread *sl.Thread, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
		return g.invoke(thread, node.entry.Name, args, kwargs)
	}
	if len(node.children) == 0 {
		if node.entry == nil {
			return sl.None
		}
		e := node.entry
		return sl.NewBuiltin(e.Name, func(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
			return call(thread, args, kwargs)
		})
	}
	ns := &namespace{name: path, attrs: map[string]sl.Value{}}
	if node.entry != nil {
		ns.call = call // callable namespace: a.b() and a.b.c() both work
	}
	for k, child := range node.children {
		ns.attrs[k] = g.buildNode(path+"."+k, child)
	}
	return ns
}

// invoke runs a capability (blocking, even for Long ones) and converts the
// result back. Errors surface as "<code>: <message>".
func (g *globals) invoke(thread *sl.Thread, ref string, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	v, err := g.requireView()
	if err != nil {
		return nil, err
	}
	if g.obs != nil {
		return g.obs.invoke(thread, g.ctx, v, ref, args, kwargs)
	}
	return invokeVia(thread, g.ctx, v, ref, args, kwargs, false)
}

// invokeVia starts ref and waits for it under ctx. When ctx ends first the
// job keeps running, as with View.Invoke, unless cancelAbandoned is set.
func invokeVia(thread *sl.Thread, ctx context.Context, v *registry.View, ref string, args sl.Tuple, kwargs []sl.Tuple, cancelAbandoned bool) (sl.Value, error) {
	in, err := values.KwargsToJSON(ref, args, kwargs)
	if err != nil {
		return nil, err
	}
	if entry, ok := v.Get(ref); ok && entry.Superseded() {
		warnf(thread, "warning: %s is superseded by %s", entry.Name, strings.Join(entry.SupersededBy, ", "))
	}
	job, err := v.Start(ctx, ref, in)
	if err != nil {
		return nil, err // coded "*code*: message" via Error.Error()
	}
	out, err := job.Wait(ctx)
	if err != nil {
		if cancelAbandoned && ctx.Err() != nil {
			job.Cancel()
		}
		return nil, err
	}
	return values.FromJSON(out)
}

func warnf(thread *sl.Thread, format string, args ...any) {
	if thread != nil && thread.Print != nil {
		thread.Print(thread, fmt.Sprintf(format, args...))
	}
}

func (g *globals) callBuiltin(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	var ref string
	if err := sl.UnpackPositionalArgs("call", args, nil, 1, &ref); err != nil {
		return nil, err
	}
	v, err := g.requireView()
	if err != nil {
		return nil, err
	}
	if err := g.inSnapshot(ref); err != nil {
		if g.obs != nil {
			g.obs.rejected(thread, ref, err)
		}
		return nil, err
	}
	if g.obs != nil {
		return g.obs.invoke(thread, g.ctx, v, ref, nil, kwargs)
	}
	return invokeVia(thread, g.ctx, v, ref, nil, kwargs, false)
}

func (g *globals) jobsStart(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	var ref string
	if err := sl.UnpackPositionalArgs("jobs.start", args, nil, 1, &ref); err != nil {
		return nil, err
	}
	v, err := g.requireView()
	if err != nil {
		return nil, err
	}
	if err := g.inSnapshot(ref); err != nil {
		if g.obs != nil {
			g.obs.rejected(thread, ref, err)
		}
		return nil, err
	}
	val, job, err := startJob(g.ctx, v, ref, kwargs)
	if g.obs != nil {
		if err != nil {
			g.obs.rejected(thread, ref, err)
		} else if job != nil {
			g.obs.track(job)
			g.obs.started(thread, job)
		}
	}
	return val, err
}

func startJob(ctx context.Context, v *registry.View, ref string, kwargs []sl.Tuple) (sl.Value, *registry.Job, error) {
	in, err := values.KwargsToJSON(ref, nil, kwargs)
	if err != nil {
		return nil, nil, err
	}
	job, err := v.Start(ctx, ref, in)
	if err != nil {
		return nil, nil, err
	}
	return &jobValue{job: job, ctx: ctx}, job, nil
}

// --- namespace value ---

type namespace struct {
	name  string
	attrs map[string]sl.Value
	// call is set when a capability's name ends exactly at this
	// namespace (a.b beside a.b.c): the namespace itself is callable.
	call func(thread *sl.Thread, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error)
}

func (n *namespace) Name() string { return n.name }

// CallInternal makes a namespace with an entry callable. A namespace
// without one keeps Starlark's usual not-callable error text.
func (n *namespace) CallInternal(thread *sl.Thread, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	if n.call == nil {
		return nil, fmt.Errorf("invalid call of non-function (namespace)")
	}
	return n.call(thread, args, kwargs)
}

var _ sl.Callable = (*namespace)(nil)

func (n *namespace) String() string        { return "<capabilities " + n.name + ">" }
func (n *namespace) Type() string          { return "namespace" }
func (n *namespace) Freeze()               {}
func (n *namespace) Truth() sl.Bool        { return sl.True }
func (n *namespace) Hash() (uint32, error) { return sl.String(n.name).Hash() }
func (n *namespace) Attr(name string) (sl.Value, error) {
	v, ok := n.attrs[name]
	if !ok {
		return nil, nil // no such attr
	}
	return v, nil
}
func (n *namespace) AttrNames() []string {
	out := make([]string, 0, len(n.attrs))
	for k := range n.attrs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ sl.HasAttrs = (*namespace)(nil)

// --- job value ---

type jobValue struct {
	job *registry.Job
	ctx context.Context
}

func (j *jobValue) String() string {
	return "<job " + j.job.ID() + " " + string(j.job.Status().State) + ">"
}
func (j *jobValue) Type() string          { return "job" }
func (j *jobValue) Freeze()               {}
func (j *jobValue) Truth() sl.Bool        { return sl.True }
func (j *jobValue) Hash() (uint32, error) { return sl.String(j.job.ID()).Hash() }
func (j *jobValue) Attr(name string) (sl.Value, error) {
	switch name {
	case "id":
		return sl.String(j.job.ID()), nil
	case "status":
		return sl.NewBuiltin("job.status", func(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
			return statusToStarlark(j.job.Status())
		}).BindReceiver(j), nil
	case "wait":
		return sl.NewBuiltin("job.wait", j.wait).BindReceiver(j), nil
	case "cancel":
		return sl.NewBuiltin("job.cancel", func(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
			j.job.Cancel()
			return statusToStarlark(j.job.Status())
		}).BindReceiver(j), nil
	}
	return nil, nil
}
func (j *jobValue) AttrNames() []string { return []string{"cancel", "id", "status", "wait"} }

var _ sl.HasAttrs = (*jobValue)(nil)

func (j *jobValue) wait(thread *sl.Thread, b *sl.Builtin, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	var timeoutV sl.Value
	if err := sl.UnpackArgs("job.wait", args, kwargs, "timeout_seconds?", &timeoutV); err != nil {
		return nil, err
	}
	timeout := 0.0
	if timeoutV != nil {
		f, ok := sl.AsFloat(timeoutV)
		if !ok {
			return nil, fmt.Errorf("job.wait: timeout_seconds must be a number")
		}
		timeout = f
	}
	ctx := j.ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(j.ctx, secondsDuration(timeout))
		defer cancel()
	}
	res, _ := j.job.Wait(ctx)
	st := j.job.Status()
	d, err := statusToStarlark(st)
	if err != nil {
		return nil, err
	}
	out := d.(*sl.Dict)
	if st.State == registry.JobSucceeded {
		rv, err := values.FromJSON(res)
		if err != nil {
			return nil, err
		}
		// Wrap: {status: ..., result: ...} to mirror jobs.wait.
		wrapped := new(sl.Dict)
		_ = wrapped.SetKey(sl.String("status"), out)
		_ = wrapped.SetKey(sl.String("result"), rv)
		return wrapped, nil
	}
	wrapped := new(sl.Dict)
	_ = wrapped.SetKey(sl.String("status"), out)
	return wrapped, nil
}

func secondsDuration(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func statusToStarlark(st registry.JobStatus) (sl.Value, error) { return values.FromGo(st) }
