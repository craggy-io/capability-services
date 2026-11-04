// Package mcp is the MCP projection, built on the tools package and the
// official MCP Go SDK. Tool definitions come from tools.Defs; runtime
// registration, supersession and readiness changes reach clients live via
// Watch (the SDK sends notifications/tools/list_changed itself).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/tools"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Option configures Serve.
type Option func(*config)

type config struct {
	caller  func(*mcpsdk.ServerSession) registry.Caller
	onClose func(registry.Caller)
}

// Caller sets how a session's registry.Caller is built (once per MCP
// session). Kind defaults to "agent" and Session to the MCP session ID
// when the returned Caller leaves them empty.
func Caller(f func(*mcpsdk.ServerSession) registry.Caller) Option {
	return func(c *config) { c.caller = f }
}

// OnClose is called with the session's Caller when an MCP session that made
// at least one call ends. The host uses it to release whatever it keeps per
// caller, for example ending its own session record for Caller.Session.
func OnClose(f func(registry.Caller)) Option {
	return func(c *config) { c.onClose = f }
}

var sessionSeq atomic.Uint64

type server struct {
	sdk *mcpsdk.Server
	v   *registry.View
	cfg config

	mu       sync.Mutex
	served   map[string]string // tool name -> fingerprint of what was added
	callers  map[*mcpsdk.ServerSession]registry.Caller
	watching map[*mcpsdk.ServerSession]bool
}

// Serve registers view v's tools on s and keeps them in sync until the
// returned stop function is called (which removes the tools it added).
func Serve(s *mcpsdk.Server, v *registry.View, opts ...Option) (stop func()) {
	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}
	sv := &server{
		sdk: s, v: v, cfg: cfg,
		served: map[string]string{}, callers: map[*mcpsdk.ServerSession]registry.Caller{},
		watching: map[*mcpsdk.ServerSession]bool{},
	}
	sv.sync()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range v.Watch(ctx) {
			sv.sync()
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			sv.mu.Lock()
			names := make([]string, 0, len(sv.served))
			for n := range sv.served {
				names = append(names, n)
			}
			sv.served = map[string]string{}
			sv.mu.Unlock()
			if len(names) > 0 {
				s.RemoveTools(names...)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func fingerprint(d tools.Def) string {
	schema, _ := json.Marshal(d.InputSchema)
	out, _ := json.Marshal(d.Entry.Output)
	return fmt.Sprintf("%s|%s|%s|%s|%d|%v|%v", d.Description, d.Entry.Title, schema, out,
		d.Entry.Effects, d.Entry.Long, d.Entry.Version)
}

func (sv *server) sync() {
	defs := tools.Defs(sv.v)
	want := make(map[string]tools.Def, len(defs))
	for _, d := range defs {
		want[d.Name] = d
	}
	sv.mu.Lock()
	var remove []string
	for name := range sv.served {
		if _, ok := want[name]; !ok {
			remove = append(remove, name)
			delete(sv.served, name)
		}
	}
	var add []tools.Def
	for name, d := range want {
		fp := fingerprint(d)
		if sv.served[name] != fp {
			// New or changed: remove first so re-Add can't collide.
			if _, existed := sv.served[name]; existed {
				remove = append(remove, name)
			}
			sv.served[name] = fp
			add = append(add, d)
		}
	}
	sv.mu.Unlock()
	if len(remove) > 0 {
		sv.sdk.RemoveTools(remove...)
	}
	for _, d := range add {
		sv.sdk.AddTool(sv.toolFor(d), sv.handlerFor(d))
	}
}

func (sv *server) toolFor(d tools.Def) *mcpsdk.Tool {
	e := d.Entry
	t := &mcpsdk.Tool{
		Name:        d.Name,
		Title:       e.Title,
		Description: d.Description,
		InputSchema: d.InputSchema,
		Annotations: &mcpsdk.ToolAnnotations{
			Title:           e.Title,
			ReadOnlyHint:    e.Effects.Has(registry.ReadOnly),
			DestructiveHint: boolPtr(e.Effects.Has(registry.Destructive)),
			IdempotentHint:  e.Effects.Has(registry.Idempotent),
			OpenWorldHint:   boolPtr(e.Effects.Has(registry.OpenWorld)),
		},
	}
	// The schema describes the capability's output, which the call returns
	// directly (as structured content, see resultOf). A Long capability
	// returns a job handle instead, so it advertises no schema: a client
	// that validates results against it would reject every call.
	if e.Output != nil && !e.Long {
		t.OutputSchema = e.Output
	}
	return t
}

// sessionCaller builds (once) and returns the Caller for a session, and
// arms the OnClose watcher the first time the session is seen.
func (sv *server) sessionCaller(ss *mcpsdk.ServerSession) registry.Caller {
	if ss == nil {
		return registry.Caller{Kind: "agent"}
	}
	sv.mu.Lock()
	if c, ok := sv.callers[ss]; ok {
		sv.mu.Unlock()
		return c
	}
	var c registry.Caller
	if sv.cfg.caller != nil {
		c = sv.cfg.caller(ss)
	}
	if c.Kind == "" {
		c.Kind = "agent"
	}
	if c.Session == "" {
		// Some transports give sessions no ID; fall back to a stable
		// per-session identifier so Caller.Session always correlates.
		c.Session = ss.ID()
		if c.Session == "" {
			c.Session = fmt.Sprintf("mcp-session-%d", sessionSeq.Add(1))
		}
	}
	if c.ID == "" {
		c.ID = "agent:" + c.Session
	}
	sv.callers[ss] = c
	armWatch := !sv.watching[ss] // always: forgetting the caller must not depend on OnClose
	if armWatch {
		sv.watching[ss] = true
	}
	sv.mu.Unlock()
	if armWatch {
		go func() {
			_ = ss.Wait()
			sv.mu.Lock()
			delete(sv.callers, ss)
			delete(sv.watching, ss)
			sv.mu.Unlock()
			if sv.cfg.onClose != nil {
				sv.cfg.onClose(c)
			}
		}()
	}
	return c
}

// resultOf is a tool call's result: the payload as the first text block
// (on failure a registry.ErrorEnvelope, whose code, reason and retryable a
// client reads by parsing it), the notes as a second, and the output as
// structured content when the tool advertises an output schema. The MCP
// spec wants the first block to be the structured content serialized;
// strict clients reject a result without it, which is why notes never
// share that block.
func resultOf(r tools.Result) *mcpsdk.CallToolResult {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: r.Text}},
		IsError: r.IsError,
	}
	if len(r.Notes) > 0 {
		notes := strings.TrimPrefix(tools.AppendNotes("", r.Notes...), "\n")
		res.Content = append(res.Content, &mcpsdk.TextContent{Text: notes})
	}
	if len(r.Structured) > 0 {
		res.StructuredContent = r.Structured
	}
	return res
}

// progressMessage renders a stage update for a progress notification.
// Stage counters and unit travel together in the message; the numeric
// progress is a monotonic notification sequence with no total, never the
// (resetting) stage counters.
func progressMessage(u registry.Update) string {
	msg := u.Stage
	if u.Message != "" {
		if msg != "" {
			msg += ": "
		}
		msg += u.Message
	}
	switch {
	case u.Total > 0:
		msg += fmt.Sprintf(" (%d/%d", u.Done, u.Total)
		if u.Unit != "" {
			msg += " " + u.Unit
		}
		msg += ")"
	case u.Done > 0:
		msg += fmt.Sprintf(" (%d", u.Done)
		if u.Unit != "" {
			msg += " " + u.Unit
		}
		msg += ")"
	}
	if msg == "" {
		msg = "working"
	}
	return msg
}

// progressInterval is the least time between two notifications for one
// stage. A job reporting per file (a 200k-file transfer) would otherwise
// send the client one notification per file.
const progressInterval = 250 * time.Millisecond

// forwardProgress streams a job's updates as notifications/progress until
// the job is terminal. Progress values are 1,2,3,... (monotonic, no total).
// A new stage is sent at once. Repeats within a stage are paced to
// progressInterval and read from the job's Status, which, unlike the
// event stream, never drops an update, so the client also gets the
// final count when the job ends.
func forwardProgress(ctx context.Context, ss *mcpsdk.ServerSession, token any, job *registry.Job) {
	if ss == nil || token == nil {
		return
	}
	var (
		seq   float64
		last  time.Time
		stage string
		sent  string
	)
	send := func(u registry.Update) {
		msg := progressMessage(u)
		if msg == sent {
			return
		}
		seq++
		last, stage, sent = time.Now(), u.Stage, msg
		_ = ss.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      seq,
			Message:       msg,
		})
	}
	latest := func() {
		if u := job.Status().Last; u.Stage != "" || u.Message != "" {
			send(u)
		}
	}
	tick := time.NewTicker(progressInterval)
	defer tick.Stop()
	events := job.Events(ctx)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				latest()
				return
			}
			if u := ev.Update; u != nil && (seq == 0 || u.Stage != stage || time.Since(last) >= progressInterval) {
				send(*u)
			}
		case <-tick.C:
			if time.Since(last) >= progressInterval {
				latest()
			}
		}
	}
}

// handlerFor serves one tool call. A call that ran (not a Long call's
// handle, which returns at once) carries the slow-call note when it took
// long enough for the model to want to know.
func (sv *server) handlerFor(d tools.Def) mcpsdk.ToolHandler {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		start := time.Now()
		r := sv.call(ctx, req)
		if r.Job == "" {
			r = r.Elapsed(time.Since(start))
		}
		return resultOf(r), nil
	}
}

func (sv *server) call(ctx context.Context, req *mcpsdk.CallToolRequest) tools.Result {
	caller := sv.sessionCaller(req.Session)
	ctx = registry.WithCaller(ctx, caller)
	args := req.Params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	token := req.Params.GetProgressToken()
	entry, ok := sv.v.Get(registry.FromToolName(req.Params.Name))
	if !ok || token == nil && !entry.Long {
		return tools.Do(ctx, sv.v, req.Params.Name, args)
	}
	job, err := sv.v.Start(ctx, entry.Name, args)
	if err != nil {
		return tools.ErrorResult(err)
	}
	if entry.Long {
		// Return the job handle now; the agent follows up with
		// jobs_wait. Progress forwarding continues in the background
		// while the session lives.
		if token != nil {
			go forwardProgress(context.WithoutCancel(ctx), req.Session, token, job)
		}
		return tools.Handle(entry, job)
	}
	// Blocking call with a progress token: run as a job and forward
	// updates while waiting.
	fwdDone := make(chan struct{})
	go func() {
		defer close(fwdDone)
		forwardProgress(ctx, req.Session, token, job)
	}()
	out, werr := job.Wait(ctx)
	<-fwdDone
	return tools.Outcome(entry, out, werr)
}
