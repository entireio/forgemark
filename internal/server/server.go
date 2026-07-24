// Package server is the forgemark web GUI: a localhost HTTP server that runs
// benchmark sweeps (one or many targets, level-aligned) over internal/bench
// and streams live per-second stats to an embedded single-page UI over SSE.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/entireio/forgemark/internal/results"
)

//go:embed static
var staticFiles embed.FS

type Server struct {
	addr string
	mgr  *RunManager
	mux  *http.ServeMux
}

// New builds the server. resultsDir is where finished runs are persisted and
// where history is read from (shared with the CLI's results/ by default).
func New(addr, resultsDir string) *Server {
	s := &Server{addr: addr, mgr: newRunManager(resultsDir), mux: http.NewServeMux()}

	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("embedded static assets missing: " + err.Error()) // build-time invariant
	}
	s.mux.Handle("GET /", http.FileServerFS(staticRoot))

	s.mux.HandleFunc("POST /api/runs", s.requireLocalOrigin(s.handleStartRun))
	s.mux.HandleFunc("GET /api/runs", s.handleListRuns)
	s.mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("POST /api/runs/{id}/cancel", s.requireLocalOrigin(s.handleCancelRun))
	s.mux.HandleFunc("GET /api/runs/{id}/events", s.handleRunEvents)
	s.mux.HandleFunc("GET /api/history", s.handleHistory)
	s.mux.HandleFunc("GET /api/history/{file}", s.handleHistoryDoc)
	s.mux.HandleFunc("GET /api/local/suggest", s.requireLocalOrigin(s.handleLocalSuggest))
	return s
}

// Handler exposes the mux for httptest.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe blocks until ctx is cancelled or the listener fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{Addr: s.addr, Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	}
}

// requireLocalOrigin guards mutating endpoints against DNS-rebinding/CSRF: the
// UI pastes credentials, so a hostile web page must not be able to drive this
// API. Browsers always send Origin on cross-origin POSTs; a request with no
// Origin header is curl or same-origin-old-browser, which is fine for a
// loopback tool.
func (s *Server) requireLocalOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !IsLoopbackHost(u.Hostname()) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// IsLoopbackHost reports whether host names the loopback interface. An empty
// host is NOT loopback: in a listen address it means wildcard bind (all
// interfaces), and in an Origin it means an unparseable header — both are
// exactly the cases the callers must treat as exposed.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("bad request body: %v", err))
		return
	}
	run, warnings, err := s.mgr.start(req)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errRunActive) {
			code = http.StatusConflict
		}
		httpError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": run.ID, "warnings": warnings})
}

func (s *Server) handleListRuns(w http.ResponseWriter, _ *http.Request) {
	runs := s.mgr.list()
	out := make([]runStatus, len(runs))
	for i, r := range runs {
		out[i] = r.status()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run := s.mgr.get(r.PathValue("id"))
	if run == nil {
		httpError(w, http.StatusNotFound, "no such run")
		return
	}
	writeJSON(w, http.StatusOK, run.status())
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	run := s.mgr.get(r.PathValue("id"))
	if run == nil {
		httpError(w, http.StatusNotFound, "no such run")
		return
	}
	run.cancel()
	writeJSON(w, http.StatusOK, map[string]any{"id": run.ID, "cancelling": true})
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	run := s.mgr.get(r.PathValue("id"))
	if run == nil {
		httpError(w, http.StatusNotFound, "no such run")
		return
	}
	serveSSE(w, r, run.log)
}

func (s *Server) handleHistory(w http.ResponseWriter, _ *http.Request) {
	list, err := results.List(s.mgr.resultsDir)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleHistoryDoc(w http.ResponseWriter, r *http.Request) {
	doc, err := results.Load(s.mgr.resultsDir, r.PathValue("file"))
	switch {
	case errors.Is(err, results.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such result file")
	case err != nil:
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, doc)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
