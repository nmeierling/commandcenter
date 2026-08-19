package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/devcc/agent/internal/model"
)

// streamEvents pushes status changes to the dashboard. Pushing beats polling
// here: the agent already refreshes every project on a timer, so re-deriving
// the same state per client per request would multiply the cost of the
// expensive parts (docker compose ps, port scans) by the number of open tabs.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}
	sseHeaders(w)

	events, cancel := s.mgr.Subscribe()
	defer cancel()

	// Send the current state immediately so a reconnecting client is correct
	// before the next refresh tick.
	writeEvent(w, flusher, "snapshot", map[string]any{
		"projects": s.mgr.Views(),
		"agent":    s.mgr.Status(),
	})

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			if !writeEvent(w, flusher, string(ev.Type), ev) {
				return
			}
		case <-heartbeat.C:
			// Comment frames keep proxies and idle-timeout logic from closing
			// the connection.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// streamLogs follows a project's logs.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, ok := s.mgr.View(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown project")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	buf, err := s.mgr.Logs(id)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	tail := intParam(r, "tail", 200, 1, 5000)

	// Subscribe before replaying the tail, so nothing emitted in between is
	// lost. Duplicates are possible at the seam; the client drops repeats by
	// sequence number.
	lines, cancel := buf.Subscribe(512)
	defer cancel()

	if v.Runtime == model.RuntimeCompose {
		// Attach a `docker compose logs -f` for the life of this request.
		if err := s.mgr.FollowComposeLogs(r.Context(), id, tail); err != nil {
			buf.Append("agent", "could not follow container logs: "+err.Error())
		}
	}

	sseHeaders(w)
	for _, l := range buf.Tail(tail) {
		if !writeEvent(w, flusher, "line", logLine{Seq: l.Seq, Stream: l.Stream, Text: l.Text, At: l.At}) {
			return
		}
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case l, open := <-lines:
			if !open {
				return
			}
			if !writeEvent(w, flusher, "line", logLine{Seq: l.Seq, Stream: l.Stream, Text: l.Text, At: l.At}) {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func sseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Without this, some proxies buffer the stream into uselessness.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func writeEvent(w http.ResponseWriter, f http.Flusher, name string, payload any) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return false
	}
	f.Flush()
	return true
}
