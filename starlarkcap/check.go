package starlarkcap

import (
	"github.com/craggy-io/script-services/check"
	"github.com/craggy-io/script-services/modules"

	"github.com/craggy-io/capability-services/registry"
)

// Diagnostic is one problem Check found.
type Diagnostic = check.Diagnostic

// Check validates src against the capability globals of v (nil means
// none), call(), jobs, input and the host's extra predeclared names,
// without running anything (see check.Analyze):
//
//   - a dotted path from a capability namespace must name a capability or
//     namespace in v (unknown names come with a nearest-name suggestion);
//   - a literal ref passed to call() or jobs.start() must resolve in v;
//   - keyword arguments of a direct call must fit the input schema: no
//     unknown names when the schema is closed, no missing required ones
//     (skipped when the call passes a positional dict or **kwargs).
//
// Diagnostics are in source order. An empty result means the script
// compiles and every reference Check can see resolves; it says nothing
// about runtime values.
func Check(v *registry.View, filename string, src []byte, extra ...string) []Diagnostic {
	return Analyze(v, filename, src, extra...).Diagnostics
}

// Analysis is what Analyze found in a script without running it.
type Analysis struct {
	Diagnostics []Diagnostic // Check's diagnostics, in source order
	// Uses is every capability a direct call or a literal call() or
	// jobs.start() ref resolves to, once each by name, in source order:
	// what the script will parse the output of.
	Uses []registry.Entry
	// Loads are the canonical paths of the script's load() targets, once
	// each, in source order.
	Loads []string
}

// CheckOptions extend Analyze.
type CheckOptions struct {
	// Extra are host-predeclared names beside the capability globals
	// (Program.Predeclared's names).
	Extra []string
	// Modules, when set, is where load() targets are looked up: they must
	// exist and parse, and define the names loaded from them.
	Modules modules.Source
}

// Analyze is Check plus the capabilities the script calls (Analysis.Uses)
// and the modules it loads (Analysis.Loads).
func Analyze(v *registry.View, filename string, src []byte, extra ...string) Analysis {
	return AnalyzeWith(v, filename, src, CheckOptions{Extra: extra})
}

// AnalyzeWith is Analyze with options; with CheckOptions.Modules it also
// checks load() targets.
func AnalyzeWith(v *registry.View, filename string, src []byte, opts CheckOptions) Analysis {
	env := Env(v)
	env.Predeclared = append(env.Predeclared, opts.Extra...)
	env.Modules = opts.Modules
	a := check.Analyze(env, filename, src)
	out := Analysis{Diagnostics: a.Diagnostics, Loads: a.Loads}
	for _, c := range a.Uses {
		if e, ok := c.Value.(registry.Entry); ok {
			out.Uses = append(out.Uses, e)
		}
	}
	return out
}

// Env describes what Exec predeclares for scripts run against v, for
// check.Analyze: one callable per entry in v.List() (Value is the
// registry.Entry), and call / jobs.start resolving refs through v.Get.
// Messages name them capabilities (Noun "capability": "unknown capability
// x.y"). A nil v has no callables and resolves no refs.
func Env(v *registry.View) check.Env {
	resolve := func(ref string) (check.Callable, bool) {
		if v == nil {
			return check.Callable{}, false
		}
		e, ok := v.Get(ref)
		if !ok {
			return check.Callable{}, false
		}
		return callableOf(e), true
	}
	env := check.Env{Noun: noun, RefFuncs: map[string]func(string) (check.Callable, bool){
		"call":       resolve,
		"jobs.start": resolve,
	}}
	if v != nil {
		for _, e := range v.List() {
			env.Callables = append(env.Callables, callableOf(e))
		}
	}
	return env
}

// noun is what check messages call a callable in this package.
const noun = "capability"

func callableOf(e registry.Entry) check.Callable {
	return check.Callable{Name: e.Name, Input: e.Capability.Input, Value: e}
}
