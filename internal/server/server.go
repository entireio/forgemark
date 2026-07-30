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

// shutdownDrain bounds how long a graceful shutdown waits for the active run to
// cancel, persist, and clean up. It exceeds a session's 30s ref-deletion budget
// (bench.deleteRef) plus margin so cleanup finishes before we return, while
// still capping a wedged run so ctrl-c can't hang forever.
const shutdownDrain = 40 * time.Second

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

	// Every /api route (reads included) carries the rebinding guard: a page that
	// rebinds a hostname to loopback can otherwise read run status, live events,
	// and history — which name targets and remotes — through a spoofed Host. The
	// static file server stays open so the SPA itself loads.
	s.mux.HandleFunc("POST /api/runs", s.requireLocalOrigin(s.handleStartRun))
	s.mux.HandleFunc("GET /api/runs", s.requireLocalOrigin(s.handleListRuns))
	s.mux.HandleFunc("GET /api/runs/{id}", s.requireLocalOrigin(s.handleGetRun))
	s.mux.HandleFunc("POST /api/runs/{id}/cancel", s.requireLocalOrigin(s.handleCancelRun))
	s.mux.HandleFunc("GET /api/runs/{id}/events", s.requireLocalOrigin(s.handleRunEvents))
	s.mux.HandleFunc("GET /api/history", s.requireLocalOrigin(s.handleHistory))
	s.mux.HandleFunc("GET /api/history/{file}", s.requireLocalOrigin(s.handleHistoryDoc))
	// Discovery execs the operator's CLIs and returns their identities; it is
	// loopback-only on top of the origin guard, never reachable on an exposed bind.
	s.mux.HandleFunc("GET /api/local/suggest", s.requireLocalOrigin(s.requireLoopbackBind(s.handleLocalSuggest)))
	return s
}

// Handler exposes the mux for httptest.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe blocks until ctx is cancelled or the listener fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{Addr: s.addr, Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	// drain stops new starts and cancels+waits the active run so we never return
	// while remote load continues or results/refs are unpersisted. Used on both
	// exit paths: a normal ctx cancellation and a listener/accept failure — the
	// latter must drain too, or an embedded caller leaks the run and a CLI exit
	// skips session-ref cleanup and result persistence.
	drain := func() {
		// deleteRef runs on a detached 30s context, so wait beyond that so cleanup
		// finishes before we give up.
		s.mgr.beginShutdown()
		s.mgr.stopActive(shutdownDrain)
	}
	select {
	case err := <-errc:
		// The listener already returned, so there's no srv.Shutdown to do — just
		// drain the run before propagating the error.
		drain()
		return err
	case <-ctx.Done():
		// Stop the active run first: cancel it and wait for its coordinator to
		// persist and clean up. This also ends the load and closes the SSE
		// streams, so the HTTP shutdown below completes promptly instead of
		// blocking on long-lived event connections.
		drain()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	}
}

// requireLocalOrigin guards mutating endpoints, which can drive runs and pull
// CLI-sourced credentials, against DNS rebinding and cross-origin abuse. Two
// checks:
//
//   - Host: on a loopback deployment the request Host must be a loopback
//     literal. This defeats DNS rebinding — a page at evil.example that rebinds
//     to 127.0.0.1 still sends Host: evil.example, so it's rejected. The port
//     isn't checked, so any loopback port (and httptest) works. On an
//     intentionally non-loopback -addr the operator opted into exposure (and
//     was warned), so this check is skipped there.
//   - Origin: a present Origin must match the Host exactly. Browsers send
//     Origin on cross-origin requests including "simple" no-preflight POSTs; a
//     loopback-hostname check alone would accept any other local dev server (a
//     different port is still cross-origin). No Origin is curl or a same-origin
//     old browser, fine for a loopback control panel.
func (s *Server) requireLocalOrigin(next http.HandlerFunc) http.HandlerFunc {
	loopback := s.loopbackBound()
	return func(w http.ResponseWriter, r *http.Request) {
		if loopback && !IsLoopbackHost(hostnameOnly(r.Host)) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host == "" || u.Host != r.Host {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// requireLoopbackBind refuses an endpoint outright on a non-loopback -addr,
// regardless of Origin. It guards capabilities that must never be reachable
// across a network at all — here, CLI-credential discovery, which execs the
// operator's gh/glab/entire logins and returns their identities. On the
// default loopback bind it is a pass-through.
func (s *Server) requireLoopbackBind(next http.HandlerFunc) http.HandlerFunc {
	if s.loopbackBound() {
		return next
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "this endpoint reads local CLI credentials and is disabled on a non-loopback -addr", http.StatusForbidden)
	}
}

// loopbackBound reports whether the server is bound to an explicit loopback
// address (the default, safe deployment). A wildcard (":8377") or a real
// interface is treated as exposed: the DNS-rebinding guard doesn't apply and
// pasted secrets are refused.
func (s *Server) loopbackBound() bool {
	h, _, err := net.SplitHostPort(s.addr)
	return err == nil && h != "" && IsLoopbackHost(h)
}

// hostnameOnly strips an optional :port from a Host header value.
func hostnameOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// IsLoopbackHost reports whether host names the loopback interface. An empty
// host is NOT loopback: in a listen address it means wildcard bind (all
// interfaces), exactly the case the serve warning must treat as exposed.
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
	// No credential is materialized on a non-loopback bind. A pasted secret would
	// cross the network in cleartext; a secret_source is worse — it would spend
	// the operator's CLI credential (gh/glab/entire) on a target chosen by any
	// client that can reach this port. So an exposed bind runs demo:// targets
	// only; real-forge benchmarking requires the default loopback bind, where the
	// credential stays on the machine, held in memory for the run.
	if !s.loopbackBound() {
		for _, t := range req.Targets {
			if t.Secret != "" || t.SecretSource != "" {
				httpError(w, http.StatusBadRequest,
					"credentials are refused on a non-loopback -addr: a pasted secret would cross the network in cleartext, and a secret_source would spend the operator's CLI credential on behalf of any reachable client. Bind to loopback (the default) to benchmark real forges; only demo:// targets run on an exposed bind")
				return
			}
		}
	}
	run, warnings, err := s.mgr.start(req)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, errRunActive):
			code = http.StatusConflict
		case errors.Is(err, errShuttingDown):
			code = http.StatusServiceUnavailable
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
