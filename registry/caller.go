package registry

import "context"

// Caller identifies who is calling. A projection fills it in; the
// application maps it to whatever session concept it owns. The registry only
// carries the identity.
type Caller struct {
	ID      string            `json:"id"`               // "user:phil", "agent:triage-7", "script:so_risk.star"
	Kind    string            `json:"kind"`             // "user", "agent", "script", "service"
	Session string            `json:"session"`          // the conversation or connection this call belongs to
	Parent  *Caller           `json:"parent,omitempty"` // who started this caller, e.g. the user behind an agent
	Attrs   map[string]string `json:"attrs,omitempty"`  //
}

type ctxKey int

const (
	callerKey ctxKey = iota
	viewKey
	jobKey
)

// WithCaller returns a context carrying c.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// CallerFrom returns the caller in ctx, if a projection set one.
func CallerFrom(ctx context.Context) (Caller, bool) {
	if ctx == nil {
		return Caller{}, false
	}
	c, ok := ctx.Value(callerKey).(Caller)
	return c, ok
}

// WithView returns a context carrying v, marking the view calls made with
// this context came in on. Hosts use it to hand a view to projections that
// only receive a context (for example Starlark globals). The registry sets
// it on every handler context itself.
func WithView(ctx context.Context, v *View) context.Context {
	return context.WithValue(ctx, viewKey, v)
}

// ViewFrom returns the view this call came in on, or nil outside a call.
// Nested capability calls must go through it: a call never gets wider access
// than the view it arrived on.
func ViewFrom(ctx context.Context) *View {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(viewKey).(*View)
	return v
}

// jobCtx is what Progress and JobIDFrom read from a handler context.
// life is the job's own cancellable context (cancelled only by
// Job.Cancel), which jobs the handler starts are tied to.
type jobCtx struct {
	id       string
	progress func(Update)
	life     context.Context
}

// JobIDFrom returns the trusted active job ID for application lease and
// operation glue.
func JobIDFrom(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	jc, ok := ctx.Value(jobKey).(*jobCtx)
	if !ok || jc == nil {
		return "", false
	}
	return jc.id, true
}

// Progress reports progress for the job running this handler. It also counts
// as a heartbeat. Outside a job it does nothing. Invalid updates (negative
// counters, or Done past a known Total) and updates after the job reaches a
// terminal state are ignored: a progress callback can never revive or
// complete a job.
func Progress(ctx context.Context, u Update) {
	if ctx == nil {
		return
	}
	jc, ok := ctx.Value(jobKey).(*jobCtx)
	if !ok || jc == nil || jc.progress == nil {
		return
	}
	jc.progress(u)
}
