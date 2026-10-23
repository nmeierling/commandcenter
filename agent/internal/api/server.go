// Package api exposes the agent's local HTTP API and serves the dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/devcc/agent/internal/core"
	"github.com/devcc/agent/internal/logbuf"
	"github.com/devcc/agent/internal/model"
	"github.com/devcc/agent/internal/procmgr"
)

// Server wires the manager to HTTP.
type Server struct {
	mgr  *core.Manager
	auth *Auth
	ui   fs.FS
}

// New returns a server. ui may be nil when the agent is built without an
// embedded dashboard (API-only mode).
func New(mgr *core.Manager, auth *Auth, ui fs.FS) *Server {
	return &Server{mgr: mgr, auth: auth, ui: ui}
}

// Handler builds the complete routing tree.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()

	api.HandleFunc("GET /api/status", s.getStatus)
	api.HandleFunc("GET /api/projects", s.listProjects)
	api.HandleFunc("POST /api/projects/refresh", s.refreshProjects)
	api.HandleFunc("GET /api/projects/{id}", s.getProject)
	api.HandleFunc("GET /api/projects/{id}/status", s.getProjectStatus)
	api.HandleFunc("POST /api/projects/{id}/start", s.action("start"))
	api.HandleFunc("POST /api/projects/{id}/stop", s.action("stop"))
	api.HandleFunc("POST /api/projects/{id}/restart", s.action("restart"))
	// Per-unit control: one container, or one supervised app.
	api.HandleFunc("POST /api/projects/{id}/units/{unit}/start", s.unitAction("start"))
	api.HandleFunc("POST /api/projects/{id}/units/{unit}/kill", s.unitAction("kill"))
	api.HandleFunc("GET /api/projects/{id}/units/{unit}/candidates", s.substituteCandidates)
	api.HandleFunc("GET /api/projects/{id}/units/{unit}/options", s.unitOptions)
	api.HandleFunc("POST /api/projects/{id}/units/{unit}/substitute", s.setSubstitute)
	api.HandleFunc("GET /api/projects/{id}/logs", s.getLogs)
	api.HandleFunc("GET /api/projects/{id}/logs/stream", s.streamLogs)
	api.HandleFunc("GET /api/projects/{id}/health", s.getHealth)
	api.HandleFunc("POST /api/projects/{id}/categories", s.setCategories)
	api.HandleFunc("POST /api/projects/{id}/open", s.openProject)
	api.HandleFunc("POST /api/projects/{id}/ports/{port}/resolve", s.resolvePort)
	api.HandleFunc("GET /api/events", s.streamEvents)

	root := http.NewServeMux()
	root.Handle("/api/", s.auth.Middleware(api))
	root.Handle("/", s.uiHandler())
	return securityHeaders(root)
}

// securityHeaders keeps the dashboard from being embedded or sniffed. There is
// no CSP for the app itself beyond framing, because the UI is served from the
// same origin and loads no third-party code.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) uiHandler() http.Handler {
	if s.ui == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound,
				"this agent was built without the dashboard; run the Vite dev server, or rebuild with `make build`")
		})
	}
	files := http.FileServer(http.FS(s.ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Single-page app: unknown paths fall back to index.html rather than
		// 404, so deep links work after a reload.
		if r.URL.Path != "/" {
			if _, err := fs.Stat(s.ui, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-store")
		}
		files.ServeHTTP(w, r)
	})
}

// --- handlers --------------------------------------------------------------

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Status())
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"projects": s.mgr.Views()})
}

func (s *Server) refreshProjects(w http.ResponseWriter, r *http.Request) {
	s.mgr.Discover()
	s.mgr.RefreshAll(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"projects": s.mgr.Views()})
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	v, ok := s.mgr.View(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown project")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getProjectStatus(w http.ResponseWriter, r *http.Request) {
	v, err := s.mgr.RefreshOne(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v.State)
}

// actionRequest is the optional body on start/stop/restart. An empty body means
// "just do it, and tell me if something is in the way" — the agent will not
// stop anything or move any port unless one of these is set explicitly.
type actionRequest struct {
	// StopOwners authorises stopping whatever holds the conflicting ports.
	StopOwners bool `json:"stopOwners"`
	// Ports maps a declared host port to the one to use instead, as strings so
	// the JSON object keys stay valid.
	Ports map[string]int `json:"ports"`
	// ClearOverrides puts the project back on the ports it declares.
	ClearOverrides bool `json:"clearOverrides"`
}

// decodeActionOptions parses the optional request body shared by the project
// and unit lifecycle endpoints.
func decodeActionOptions(w http.ResponseWriter, r *http.Request) (core.ActionOptions, bool) {
	var body actionRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body)

	opts := core.ActionOptions{
		StopOwners:     body.StopOwners,
		ClearOverrides: body.ClearOverrides,
	}
	if len(body.Ports) > 0 {
		opts.PortOverrides = map[int]int{}
		for declared, chosen := range body.Ports {
			d, err := strconv.Atoi(declared)
			if err != nil || d <= 0 || d > 65535 || chosen <= 0 || chosen > 65535 {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("invalid port mapping %q -> %d", declared, chosen))
				return opts, false
			}
			opts.PortOverrides[d] = chosen
		}
	}
	return opts, true
}

func (s *Server) action(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts, ok := decodeActionOptions(w, r)
		if !ok {
			return
		}

		// The action runs to completion so the response carries the real
		// outcome; the UI stays responsive because status updates arrive over
		// SSE while the request is in flight.
		v, err := s.mgr.Action(r.Context(), r.PathValue("id"), name, opts)
		if err != nil {
			if errors.Is(err, core.ErrUnknownProject) {
				writeError(w, http.StatusNotFound, "unknown project")
				return
			}
			// A port conflict is not a failure, it is a question: hand the
			// dashboard everything it needs to ask the user what to do.
			var conflict *core.PortConflictError
			if errors.As(err, &conflict) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":    conflict.Error(),
					"conflict": conflict,
					"project":  v,
				})
				return
			}
			if errors.Is(err, core.ErrBusy) {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			// A failed start still returns the project, so the UI can show the
			// error next to the up-to-date card.
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":   err.Error(),
				"project": v,
			})
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// unitAction starts or kills a single container or app. A kill only ever
// happens because the user pressed the button on that row; a start still
// refuses on a port conflict and returns 409 so the UI can offer another port.
func (s *Server) unitAction(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts, ok := decodeActionOptions(w, r)
		if !ok {
			return
		}
		v, err := s.mgr.UnitAction(r.Context(), r.PathValue("id"), r.PathValue("unit"), name, opts)
		if err != nil {
			switch {
			case errors.Is(err, core.ErrUnknownProject):
				writeError(w, http.StatusNotFound, "unknown project")
			case errors.Is(err, core.ErrUnknownUnit):
				writeError(w, http.StatusNotFound, err.Error())
			case errors.Is(err, core.ErrBusy):
				writeError(w, http.StatusConflict, err.Error())
			default:
				var conflict *core.PortConflictError
				if errors.As(err, &conflict) {
					writeJSON(w, http.StatusConflict, map[string]any{
						"error":    conflict.Error(),
						"conflict": conflict,
						"project":  v,
					})
					return
				}
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":   err.Error(),
					"project": v,
				})
			}
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// substituteCandidates lists running units elsewhere that could provide this
// one — a database another project already has up, typically.
func (s *Server) substituteCandidates(w http.ResponseWriter, r *http.Request) {
	candidates := s.mgr.SubstituteCandidates(r.PathValue("id"), r.PathValue("unit"))
	if candidates == nil {
		candidates = []core.SubstituteCandidate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": candidates})
}

// unitOptions returns the choices available for a unit — the same shape a
// blocked start returns, so the dialog can also be opened on purpose to revisit
// a decision.
func (s *Server) unitOptions(w http.ResponseWriter, r *http.Request) {
	opts, err := s.mgr.UnitOptions(r.Context(), r.PathValue("id"), r.PathValue("unit"))
	if err != nil {
		switch {
		case errors.Is(err, core.ErrUnknownProject):
			writeError(w, http.StatusNotFound, "unknown project")
		case errors.Is(err, core.ErrUnknownUnit):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusUnprocessableEntity, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conflict": opts})
}

// setSubstitute records that a unit is provided by something else, or clears
// that choice with an empty target. It is persisted to the config file.
func (s *Server) setSubstitute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body)

	v, err := s.mgr.SetSubstitute(r.Context(), r.PathValue("id"), r.PathValue("unit"), body.Target)
	if err != nil {
		switch {
		case errors.Is(err, core.ErrUnknownProject):
			writeError(w, http.StatusNotFound, "unknown project")
		case errors.Is(err, core.ErrUnknownUnit):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusUnprocessableEntity, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, ok := s.mgr.View(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown project")
		return
	}
	tail := intParam(r, "tail", 200, 1, 5000)

	buf, err := s.mgr.Logs(id)
	if err != nil {
		writeManagerError(w, err)
		return
	}

	// For compose projects the containers hold the real logs, so pull a fresh
	// snapshot on request rather than tailing them in the background forever.
	if v.Runtime == model.RuntimeCompose && r.URL.Query().Get("source") != "agent" {
		out, err := s.mgr.ComposeLogs(r.Context(), id, tail)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"lines":   bufferLines(buf, tail),
				"warning": "could not read container logs: " + err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": splitLines(out, tail), "source": "compose"})
		return
	}

	// A supervised project's log file survives agent restarts, so prefer it
	// when the in-memory buffer is empty.
	lines := bufferLines(buf, tail)
	if len(lines) == 0 {
		if path := s.mgr.ProcessLogPath(id); path != "" {
			if fromFile, err := procmgr.TailFileSnapshot(path, tail); err == nil {
				for _, l := range fromFile {
					lines = append(lines, logLine{Stream: "stdout", Text: l})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "source": "agent"})
}

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	v, err := s.mgr.RefreshOne(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	if v.Health == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"message":    "no health check configured for this project",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"url":        v.Health.URL,
		"result":     v.State.Health,
	})
}

// setCategories replaces a project's category list, persisting it to the
// config file so the dashboard's tabs survive a restart.
func (s *Server) setCategories(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Categories []string `json:"categories"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	v, err := s.mgr.SetCategories(r.Context(), r.PathValue("id"), body.Categories)
	if err != nil {
		if errors.Is(err, core.ErrUnknownProject) {
			writeError(w, http.StatusNotFound, "unknown project")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) openProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body)

	opened, err := s.mgr.Open(r.Context(), r.PathValue("id"), body.URL)
	if err != nil {
		if errors.Is(err, core.ErrUnknownProject) {
			writeError(w, http.StatusNotFound, "unknown project")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opened": opened})
}

func (s *Server) resolvePort(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid port")
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body)

	msg, err := s.mgr.KillPortOwner(r.Context(), r.PathValue("id"), port, body.Force)
	if err != nil {
		if errors.Is(err, core.ErrUnknownProject) {
			writeError(w, http.StatusNotFound, "unknown project")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	v, _ := s.mgr.RefreshOne(r.Context(), r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"message": msg, "project": v})
}

// --- helpers ---------------------------------------------------------------

type logLine struct {
	Seq    int64     `json:"seq,omitempty"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
	At     time.Time `json:"at,omitempty"`
}

func bufferLines(buf *logbuf.Buffer, tail int) []logLine {
	out := []logLine{}
	for _, l := range buf.Tail(tail) {
		out = append(out, logLine{Seq: l.Seq, Stream: l.Stream, Text: l.Text, At: l.At})
	}
	return out
}

func splitLines(s string, tail int) []logLine {
	raw := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(raw) > tail {
		raw = raw[len(raw)-tail:]
	}
	out := make([]logLine, 0, len(raw))
	for i, l := range raw {
		if l == "" && len(raw) == 1 {
			continue
		}
		out = append(out, logLine{Seq: int64(i + 1), Stream: "container", Text: l})
	}
	return out
}

func intParam(r *http.Request, name string, def, minV, maxV int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The client went away mid-write; nothing useful to do.
		_ = err
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrUnknownProject):
		writeError(w, http.StatusNotFound, "unknown project")
	case errors.Is(err, core.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, core.ErrNotControllable):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// ListenAndServe runs the HTTP server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// No write timeout: SSE connections are long-lived by design.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return nil
}
