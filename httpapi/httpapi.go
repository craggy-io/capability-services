// Package httpapi is the HTTP projection: POST /v1/{ref as path}, job
// endpoints with SSE events, listings, health and a generated OpenAPI 3.1
// document. Standard library only. Auth is not part of this package — the
// application wraps the handler and supplies the Caller (option, request
// context, or Craggy-Session header).
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/craggy-io/capability-services/registry"
)

// Option configures the HTTP handler.
type Option func(*config)

type config struct {
	caller func(*http.Request) registry.Caller
	title  string
	ver    string
}

// Caller sets how a request's Caller is derived when the request context
// doesn't already carry one (registry.WithCaller by an auth wrapper).
func Caller(f func(*http.Request) registry.Caller) Option {
	return func(c *config) { c.caller = f }
}

// Info sets the OpenAPI document title and version.
func Info(title, version string) Option {
	return func(c *config) { c.title, c.ver = title, version }
}

type handler struct {
	v   *registry.View
	cfg config
}

// Handler returns an http.Handler serving the /v1 API for view v. It also
// works mounted under StripPrefix("/v1", ...).
func Handler(v *registry.View, opts ...Option) http.Handler {
	cfg := config{title: "capability-services", ver: "1"}
	for _, o := range opts {
		o(&cfg)
	}
	return &handler{v: v, cfg: cfg}
}

// The Craggy headers: a request's caller session, and the supersession
// notice on a superseded capability's response.
const (
	sessionHeader    = "Craggy-Session"
	supersededHeader = "Craggy-Superseded-By"
)

func (h *handler) callerCtx(r *http.Request) context.Context {
	ctx := r.Context()
	caller, has := registry.CallerFrom(ctx)
	if !has && h.cfg.caller != nil {
		caller = h.cfg.caller(r)
		has = true
	}
	if s := r.Header.Get(sessionHeader); s != "" {
		caller.Session = s
		has = true
	}
	if has {
		ctx = registry.WithCaller(ctx, caller)
	}
	return ctx
}

// route path after stripping an optional /v1 prefix; "" means root.
func (h *handler) route(p string) string {
	p = strings.TrimPrefix(p, "/v1")
	return strings.Trim(p, "/")
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := h.callerCtx(r)
	route := h.route(r.URL.Path)
	switch {
	case route == "" && r.Method == http.MethodGet:
		h.list(w, r)
	case route == "health" && r.Method == http.MethodGet:
		h.health(w, r)
	case route == "openapi.json" && r.Method == http.MethodGet:
		h.openapi(w, r)
	case isJobRoute(route, r.Method):
		h.job(w, r.WithContext(ctx), route)
	case r.Method == http.MethodPost && route != "":
		h.invoke(w, r.WithContext(ctx), route)
	default:
		writeErr(w, registry.Errorf(registry.NotFound, "no route for %s %s", r.Method, r.URL.Path))
	}
}

// Job routes are GET/DELETE under jobs/; POST under jobs/ is capability
// invocation (jobs.list etc.), so both coexist without ambiguity.
func isJobRoute(route, method string) bool {
	if !strings.HasPrefix(route, "jobs/") {
		return false
	}
	if method != http.MethodGet && method != http.MethodDelete {
		return false
	}
	return strings.TrimPrefix(route, "jobs/") != ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func statusFor(code registry.Code) int {
	switch code {
	case registry.Invalid:
		return http.StatusBadRequest
	case registry.Denied:
		return http.StatusForbidden
	case registry.NotFound:
		return http.StatusNotFound
	case registry.Conflict:
		return http.StatusConflict
	case registry.Cancelled:
		return 499
	case registry.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeErr(w http.ResponseWriter, err error) {
	info := registry.Info(err)
	writeJSON(w, statusFor(info.Code), registry.ErrorEnvelope{Error: info})
}

// refFromPath converts "storage/artifact/get" or ".../get@1" to a ref.
func refFromPath(route string) string {
	return strings.ReplaceAll(route, "/", ".")
}

// pathOf converts a capability name to its URL path form.
func pathOf(name string) string { return strings.ReplaceAll(name, ".", "/") }
