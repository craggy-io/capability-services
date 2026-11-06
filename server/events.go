package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/craggy-io/foundation-kit/feed"
)

// eventsKeepAlive is how often an idle event stream sends a comment.
const eventsKeepAlive = 15 * time.Second

// Events serves hub as Server-Sent Events. A client resumes with the
// Last-Event-ID header or ?after=N. The stream opens with "reset" when the
// replay is incomplete (the client must reload everything), then "ready" with
// the hub's current sequence, then one "change" per entry: id is the entry's
// sequence and data its JSON ({"seq","time","value"}). The stream ends when the
// client falls too far behind; it reconnects with its last id.
func Events[T any](hub *feed.Hub[T]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
			return
		}
		after := int64(-1)
		last := r.Header.Get("Last-Event-ID")
		if last == "" {
			last = r.URL.Query().Get("after")
		}
		if last != "" {
			n, err := strconv.ParseInt(last, 10, 64)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "invalid", "Last-Event-ID must be a non-negative sequence")
				return
			}
			after = n
		}
		entries, complete := hub.Subscribe(r.Context(), after)
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if !complete {
			fmt.Fprint(w, "event: reset\ndata: {}\n\n")
		}
		fmt.Fprintf(w, "event: ready\ndata: {\"seq\":%d}\n\n", hub.Seq())
		flusher.Flush()

		tick := time.NewTicker(eventsKeepAlive)
		defer tick.Stop()
		for {
			select {
			case e, ok := <-entries:
				if !ok {
					return
				}
				data, err := json.Marshal(e)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "id: %d\nevent: change\ndata: %s\n\n", e.Seq, data)
				flusher.Flush()
			case <-tick.C:
				fmt.Fprint(w, ": keep-alive\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
}
