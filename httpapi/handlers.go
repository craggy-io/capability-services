package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/craggy-io/capability-services/registry"
)

// The wire form of a capability is registry.Descriptor — the full
// projection (every serializable Capability field plus live state).
// The HTTP listing adds nothing to it and drops nothing from it.
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	out := h.v.Descriptors(r.URL.Query().Get("all") == "1")
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": out})
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	entries := h.v.List()
	ok := true
	caps := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if !e.Ready {
			ok = false
		}
		c := map[string]any{"ref": e.Capability.Ref(), "ready": e.Ready}
		if e.CheckError != "" {
			c["error"] = e.CheckError
		}
		caps = append(caps, c)
	}
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": ok, "capabilities": caps})
}

func (h *handler) invoke(w http.ResponseWriter, r *http.Request, route string) {
	ref := refFromPath(route)
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeErr(w, registry.WrapCode(registry.Invalid, err))
		return
	}
	in := json.RawMessage(body)
	if len(in) == 0 {
		in = json.RawMessage("{}")
	}
	entry, ok := h.v.Get(ref)
	if !ok {
		writeErr(w, registry.Errorf(registry.NotFound, "capability %q not found", ref))
		return
	}
	if entry.Superseded() {
		w.Header().Set(supersededHeader, strings.Join(entry.SupersededBy, ", "))
	}
	ctx := r.Context()
	if entry.Long {
		job, err := h.v.Start(ctx, ref, in)
		if err != nil {
			writeErr(w, err)
			return
		}
		if wait := parsePreferWait(r.Header.Get("Prefer")); wait > 0 {
			wctx, cancel := context.WithTimeout(ctx, wait)
			out, werr := job.Wait(wctx)
			cancel()
			if werr == nil {
				writeResult(w, out)
				return
			}
			if job.Status().State.Terminal() {
				writeErr(w, werr)
				return
			}
			// Still running after the wait: fall back to 202.
		}
		w.Header().Set("Location", "/v1/jobs/"+job.ID())
		writeJSON(w, http.StatusAccepted, jobEnvelope(job, false))
		return
	}
	out, err := h.v.Invoke(ctx, ref, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeResult(w, out)
}

func writeResult(w http.ResponseWriter, out json.RawMessage) {
	writeJSON(w, http.StatusOK, map[string]json.RawMessage{"result": out})
}

func parsePreferWait(prefer string) time.Duration {
	for _, part := range strings.Split(prefer, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "wait=") {
			if secs, err := strconv.ParseFloat(strings.TrimPrefix(part, "wait="), 64); err == nil && secs > 0 {
				return time.Duration(secs * float64(time.Second))
			}
		}
	}
	return 0
}

func jobEnvelope(job *registry.Job, withResult bool) map[string]any {
	env := map[string]any{"job": job.ID(), "status": job.Status()}
	if withResult {
		if res, ok := job.Result(); ok {
			env["result"] = res
		}
	}
	return env
}

func (h *handler) job(w http.ResponseWriter, r *http.Request, route string) {
	rest := strings.TrimPrefix(route, "jobs/")
	isEvents := strings.HasSuffix(rest, "/events")
	id := strings.TrimSuffix(rest, "/events")
	job, ok := h.v.JobFor(r.Context(), id)
	if !ok {
		writeErr(w, registry.Errorf(registry.NotFound, "job %q not found", id))
		return
	}
	switch {
	case isEvents && r.Method == http.MethodGet:
		h.jobEvents(w, r, job)
	case r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, jobEnvelope(job, true))
	case r.Method == http.MethodDelete:
		job.Cancel()
		writeJSON(w, http.StatusOK, jobEnvelope(job, false))
	default:
		writeErr(w, registry.Errorf(registry.NotFound, "no route for %s %s", r.Method, r.URL.Path))
	}
}

func (h *handler) jobEvents(w http.ResponseWriter, r *http.Request, job *registry.Job) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, registry.Errorf(registry.Internal, "streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	for ev := range job.Events(r.Context()) {
		data, _ := json.Marshal(ev)
		kind := "progress"
		if ev.Update == nil {
			kind = string(ev.State)
		}
		if ev.State.Terminal() && ev.Update == nil {
			kind = string(ev.State)
		}
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, kind, data)
		fl.Flush()
	}
}
