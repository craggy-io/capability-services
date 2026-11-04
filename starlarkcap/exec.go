package starlarkcap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sl "go.starlark.net/starlark"

	"github.com/craggy-io/script-services/modules"
	"github.com/craggy-io/script-services/script"
	"github.com/craggy-io/script-services/values"

	"github.com/craggy-io/capability-services/registry"
)

// Limits bound one script execution: Exec, a Load file's top level, and
// each invocation of a script-defined capability (see script.Limits).
type Limits = script.Limits

// DefaultLimits are the limits Load uses when LoadOptions.Limits is zero.
var DefaultLimits = script.DefaultLimits

// Position is a source location in a script.
type Position = script.Position

// Frame is one call frame of a script error's backtrace.
type Frame = script.Frame

// Program is one script for Exec: source plus what the host adds.
type Program struct {
	// Filename names the script in positions and is the base for relative
	// load() paths. Defaults to "script.star".
	Filename string
	Source   []byte
	// Predeclared adds host built-ins beside the capability globals,
	// call(), jobs and input. A name that collides with one of those is
	// an invalid program.
	Predeclared sl.StringDict
	// Input is a JSON-shaped value (anything encoding/json marshals),
	// bound to the global `input` and passed to the entry function.
	Input any
	// Entry is the function called after the module body runs, when the
	// script defines it: Entry(input), or Entry() if it takes no
	// parameters. Its return value is the Result. Defaults to "run".
	Entry string
	// Modules serves the script's load() statements. nil means load()
	// fails. The host chooses what modules see (modules.Options.Predeclared);
	// capability globals are the script's alone unless the host adds them.
	Modules *modules.Loader
}

// CallEvent reports one capability call a script made. Each call reports
// twice: when it starts (Done false; JobID set once the job exists) and
// when it settles (Done true, with Err for a failed call). A call the
// registry rejects before a job exists reports only the settled event. A
// jobs.start reports only the started event (the job may deliberately
// outlive the Exec); a rejected jobs.start reports the settled one.
type CallEvent struct {
	Ref     string              `json:"ref"`
	Pos     Position            `json:"pos"` // the call site in the script
	JobID   string              `json:"jobId,omitempty"`
	Done    bool                `json:"done"`
	Err     *registry.ErrorInfo `json:"error,omitempty"`
	Elapsed time.Duration       `json:"elapsed,omitempty"`
}

// Hooks observe one Exec. Both are optional and are called on the script's
// goroutine, so they must not block for long.
type Hooks struct {
	Print func(pos Position, msg string) // print() output and superseded warnings
	Call  func(CallEvent)
}

// Result is a finished Exec.
type Result struct {
	Value   json.RawMessage // the entry function's return value; null without one
	Steps   uint64          // Starlark execution steps used
	Digest  string          // content address: modules.Digest over Source and its direct load() deps (see script.Result)
	Modules []modules.Ref   // every module loaded, transitively, sorted by path
}

// Error is a failed Exec: a compile error, a runtime error, a failed
// capability call the script did not handle, or a limit. Code is the
// registry code callers map: the failing capability's code when that is
// what stopped the script, cancelled when ctx ended, invalid otherwise
// (the script, its input or a limit is at fault; a timeout is invalid
// with the message "script exceeded its time limit").
type Error struct {
	Code      registry.Code `json:"code"`
	Reason    string        `json:"reason,omitempty"` // the failing capability's reason, when it stopped the script
	Message   string        `json:"message"`
	Pos       Position      `json:"pos"`                 // innermost script position; zero when unknown
	Backtrace []Frame       `json:"backtrace,omitempty"` // outermost first
	Steps     uint64        `json:"steps"`
}

// Error formats the error as "code: file:line:col: message". When Message
// already starts with "code: " (a failed call's error text) the code is
// not repeated; Message itself is unchanged.
func (e *Error) Error() string {
	msg := strings.TrimPrefix(e.Message, string(e.Code)+": ")
	if e.Pos.Line > 0 {
		return fmt.Sprintf("%s: %s:%d:%d: %s", e.Code, e.Pos.File, e.Pos.Line, e.Pos.Col, msg)
	}
	return fmt.Sprintf("%s: %s", e.Code, msg)
}

// Unwrap lets registry.CodeOf, registry.ReasonOf and errors.As see the
// registry code and reason.
func (e *Error) Unwrap() error {
	return &registry.Error{Code: e.Code, Reason: e.Reason, Message: e.Message}
}

// errorOf maps a script failure onto registry codes: a *registry.Error in
// the cause (a capability call the script did not handle) keeps its code
// and reason, a cancelled script is Cancelled, anything else (including a
// timeout) is Invalid.
func errorOf(err error) *Error {
	var se *script.Error
	if !errors.As(err, &se) {
		return &Error{Code: registry.Invalid, Message: err.Error()}
	}
	out := &Error{Code: registry.Invalid, Message: se.Message, Pos: se.Pos, Backtrace: se.Backtrace, Steps: se.Steps}
	var re *registry.Error
	switch {
	case se.Code == script.Cancelled:
		out.Code = registry.Cancelled
	case se.TimedOut:
	case errors.As(se, &re):
		out.Code, out.Reason = re.Code, re.Reason
	}
	return out
}

// Exec runs a script against the capabilities in registry.ViewFrom(ctx):
// the module body top to bottom, then the entry function if defined (see
// script.Exec). Calls go through that view with ctx's caller, so a script
// never has wider access than whoever runs it. lim bounds steps and
// wall-clock; lim.Timeout also bounds every capability call and job wait
// the script makes, so a script blocked in a call stops at the limit (an
// invalid error, "script exceeded its time limit", not cancelled).
//
// Every job the script starts — capability calls and jobs.start — is
// tracked: if the script fails or ctx ends, those still running are
// cancelled. A script that finishes cleanly leaves jobs it deliberately
// started with jobs.start running. When Exec itself runs inside a registry
// job, cancelling that job cancels the script's jobs too (see View.Start).
func Exec(ctx context.Context, p Program, lim Limits, hooks Hooks) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Calls block in Go, where the thread's own timer cannot reach them,
	// so they run under the time limit as a context deadline.
	runCtx, stop := withTimeLimit(ctx, lim.Timeout)
	defer stop()
	obs := &observer{hooks: hooks}
	predeclared := newGlobals(runCtx, obs).dict()
	// `input` is Exec's own global (bound by script.Exec below): a
	// capability namespace of that name would collide with the binding
	// and fail every Exec. It stays reachable via call("input.…").
	delete(predeclared, "input")
	for name, v := range p.Predeclared {
		if _, clash := predeclared[name]; clash || name == "input" {
			return Result{}, &Error{Code: registry.Invalid, Message: fmt.Sprintf("host built-in %q collides with a capability global", name)}
		}
		predeclared[name] = v
	}
	res, err := script.Exec(runCtx, script.Program{
		Filename:    p.Filename,
		Source:      p.Source,
		Predeclared: predeclared,
		Input:       p.Input,
		Entry:       p.Entry,
		Modules:     p.Modules,
	}, script.Options{Limits: lim, Print: hooks.Print})
	if err != nil {
		obs.cancelOpen()
		se := errorOf(err)
		if timedOut(ctx, runCtx) {
			se.Code, se.Reason, se.Message = registry.Invalid, "", script.TimeLimitMessage
		}
		if se.Steps == 0 {
			se.Steps = res.Steps
		}
		return Result{Steps: res.Steps}, se
	}
	return Result{Value: res.Value, Steps: res.Steps, Digest: res.Digest, Modules: res.Modules}, nil
}

// observer reports an Exec's capability calls and tracks the jobs they
// run, so a failed or cancelled script can stop them.
type observer struct {
	hooks Hooks
	mu    sync.Mutex
	jobs  []*registry.Job
}

func (o *observer) track(j *registry.Job) {
	o.mu.Lock()
	// Only non-terminal jobs matter to cancelOpen; retaining every
	// finished job (and, through it, its result and history) for the
	// whole Exec would grow without bound in a chatty script.
	kept := make([]*registry.Job, 0, len(o.jobs)+1)
	for _, old := range o.jobs {
		if !old.Status().State.Terminal() {
			kept = append(kept, old)
		}
	}
	o.jobs = append(kept, j)
	o.mu.Unlock()
}

// started reports a jobs.start that created its job.
func (o *observer) started(thread *sl.Thread, job *registry.Job) {
	o.emit(CallEvent{Ref: job.Capability(), Pos: script.PositionOf(thread.CallFrame(1).Pos), JobID: job.ID()})
}

// rejected reports a call settled before any job existed (snapshot
// rejection, a failed Start).
func (o *observer) rejected(thread *sl.Thread, ref string, err error) {
	o.emit(CallEvent{Ref: ref, Pos: script.PositionOf(thread.CallFrame(1).Pos), Done: true, Err: registry.Info(err)})
}

func (o *observer) cancelOpen() {
	o.mu.Lock()
	jobs := append([]*registry.Job(nil), o.jobs...)
	o.mu.Unlock()
	for _, j := range jobs {
		if !j.Status().State.Terminal() {
			j.Cancel()
		}
	}
}

func (o *observer) emit(ev CallEvent) {
	if o.hooks.Call != nil {
		o.hooks.Call(ev)
	}
}

// invoke is invokeVia with call events and job tracking: Start + Wait,
// reporting the call site.
func (o *observer) invoke(thread *sl.Thread, ctx context.Context, v *registry.View, ref string, args sl.Tuple, kwargs []sl.Tuple) (sl.Value, error) {
	pos := script.PositionOf(thread.CallFrame(1).Pos)
	started := time.Now()
	in, err := values.KwargsToJSON(ref, args, kwargs)
	if err != nil {
		o.emit(CallEvent{Ref: ref, Pos: pos, Done: true, Err: &registry.ErrorInfo{Code: registry.Invalid, Message: err.Error()}})
		return nil, err
	}
	if entry, ok := v.Get(ref); ok && entry.Superseded() {
		warnf(thread, "warning: %s is superseded by %s", entry.Name, strings.Join(entry.SupersededBy, ", "))
	}
	job, err := v.Start(ctx, ref, in)
	if err != nil {
		o.emit(CallEvent{Ref: ref, Pos: pos, Done: true, Err: registry.Info(err), Elapsed: time.Since(started)})
		return nil, err
	}
	o.track(job)
	o.emit(CallEvent{Ref: job.Capability(), Pos: pos, JobID: job.ID()})
	out, err := job.Wait(ctx)
	ev := CallEvent{Ref: job.Capability(), Pos: pos, JobID: job.ID(), Done: true, Elapsed: time.Since(started)}
	if err != nil {
		ev.Err = registry.Info(err)
		o.emit(ev)
		return nil, err
	}
	o.emit(ev)
	return values.FromJSON(out)
}
