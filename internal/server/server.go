// Package server exposes the HTTP API and serves the embedded SPA.
//
// API (all JSON unless noted; errors are {"error": "message"} with 4xx/5xx):
//
//	POST /api/upload                multipart/form-data, field "file" (one file)
//	                                → 200 recipe.Source (blob is probed; info stored)
//	GET  /api/sources/{hash}        → recipe.Source
//	POST /api/still                 {"src": hash, "ops": [...], "output": {...}, "t": 1.5, "maxW": 480}
//	                                → image/png (Cache-Control: private, max-age=3600)
//	POST /api/jobs                  recipe.Recipe → 202 jobs.Job (503 + Retry-After while shutting down)
//	GET  /api/jobs/{id}             → jobs.Job
//	DELETE /api/jobs/{id}           cancel → 204
//	GET  /api/jobs/{id}/events      text/event-stream of jobs.Event ("event: progress|done|error",
//	                                "data: <json>"); closes after done/error
//	GET  /api/results/{recipeHash}  → jobs.Result (manifest)
//	GET  /out/{recipeHash}/{name}   result file; ?dl=1 adds Content-Disposition: attachment;
//	                                Cache-Control: public, max-age=31536000, immutable
//	GET  /api/capabilities          {"tools": {name: version}, "limits": {"emote","sticker","attachment"},
//	                                "rulesVersion": "...", "version": "...", "concurrency": N,
//	                                "maxUploadBytes": N, "formats": ["gif","webp"]}
//	GET  /healthz                   "ok"
//	GET  /*                         embedded SPA: real files as-is; extension-less paths fall back
//	                                to index.html (client routes); paths with a file extension or
//	                                under /assets/ that do not exist are 404; a plain "frontend
//	                                not built" notice when index.html is not embedded
//
// Uploads are streamed to the store (never buffered in memory) and capped
// at Config.MaxUploadBytes (413). A file ffprobe ran on but could not read
// (non-zero exit, unparsable output, no video stream) is a 422 and is not
// kept; when ffprobe itself could not run the upload is a 500 (504 when
// probing timed out) and the blob is kept, so a re-upload dedupes and
// re-probes it. Unknown /api paths are JSON 404s. Result files are served
// with strict name validation; ?dl=1 names the download after the source.
//
// Cross-site protection: state-changing requests (POST/DELETE) that a
// browser marks as coming from another site — Sec-Fetch-Site: cross-site,
// or an Origin whose host is not this server's — are refused with 403, and
// the JSON endpoints (/api/still, /api/jobs) require Content-Type
// application/json (415 otherwise). The SPA's own requests and header-less
// clients such as curl are unaffected.
//
// Lifecycle: NewServer returns a *Server whose Shutdown cancels every render
// accepted through POST /api/jobs (and any preview pre-warming), ends open
// SSE streams and waits for the pipelines to exit, so a graceful stop leaves
// no orphaned ffmpeg and no scratch dir behind. While shutting down,
// POST /api/jobs answers 503.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// Config for the HTTP layer.
type Config struct {
	MaxUploadBytes int64 // e.g. 2 GiB
	Version        string
}

// Defaults.
const (
	// DefaultMaxUploadBytes applies when Config.MaxUploadBytes <= 0.
	DefaultMaxUploadBytes = 2 << 30
	// maxJSONBody bounds JSON request bodies (recipes, still requests).
	maxJSONBody = 1 << 20
	// ssePingInterval is the SSE keep-alive comment cadence.
	ssePingInterval = 15 * time.Second
	// probeScanFrames is the alpha-scan budget for uploads.
	probeScanFrames = 60
	// stillTimeout bounds one preview render.
	stillTimeout = 60 * time.Second
	// sseShutdownGrace is how long an open SSE stream keeps forwarding
	// events after Shutdown begins, so the client sees the job's terminal
	// "cancelled" event instead of a bare EOF, before the stream is ended.
	sseShutdownGrace = 2 * time.Second
	// notBuiltNotice is served when the SPA is not embedded.
	notBuiltNotice = "ez-local-gif: frontend not built (run npm run build in web/)"
)

// probeTimeout bounds ffprobe + alpha scan per upload (a variable so tests
// can shorten it).
var probeTimeout = 2 * time.Minute

// ErrShuttingDown is returned by Shutdown when the context expired before
// every background pipeline had exited.
var ErrShuttingDown = errors.New("server: shutting down")

// Server is the root http.Handler plus lifecycle control (see Shutdown).
// Its zero value is not usable; construct it with NewServer.
type Server struct {
	cfg   Config
	st    *store.Store
	jm    *jobs.Manager
	tools ffrun.Tools
	ui    fs.FS
	h     http.Handler // route table wrapped in securityHeaders

	// ctx is the server lifetime: Shutdown cancels it. Preview pre-warming
	// runs under it, open SSE streams select on it, and every job accepted
	// through POST /api/jobs has a watcher goroutine that cancels the job
	// when it ends.
	ctx    context.Context
	cancel context.CancelFunc

	// mu serialises bg.Add against Shutdown's Wait (a WaitGroup must not
	// have Add and Wait race when the counter may be zero); it also makes
	// "no new background work after Shutdown began" exact.
	mu sync.Mutex
	// bg tracks background goroutines (preview pre-warming, job watchers)
	// so Shutdown — and tests — can wait for them.
	bg sync.WaitGroup
}

// New returns the root handler. Callers that want a graceful stop should
// use NewServer and call Shutdown.
func New(cfg Config, st *store.Store, jm *jobs.Manager, tools ffrun.Tools, ui fs.FS) http.Handler {
	return NewServer(cfg, st, jm, tools, ui)
}

// NewServer wires the handler dependencies and returns the root handler
// with lifecycle control.
func NewServer(cfg Config, st *store.Store, jm *jobs.Manager, tools ffrun.Tools, ui fs.FS) *Server {
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = DefaultMaxUploadBytes
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, st: st, jm: jm, tools: tools, ui: ui, ctx: ctx, cancel: cancel}
	s.h = s.handler()
	return s
}

// ServeHTTP dispatches to the API / SPA routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.h.ServeHTTP(w, r)
}

// Shutdown begins the graceful stop and waits, until ctx expires, for the
// server's background work to finish: every unfinished job accepted through
// POST /api/jobs is cancelled (killing its ffmpeg/gifsicle and removing its
// scratch dir), preview pre-warming is cancelled, and open SSE streams
// forward the terminal event and end. It does not touch the listener: run
// it alongside http.Server.Shutdown, which drains ordinary requests.
// Shutdown is idempotent and safe to call concurrently. It returns
// ErrShuttingDown (wrapping ctx.Err()) if the wait ran out.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: background work still running: %w", ErrShuttingDown, ctx.Err())
	}
}

// shuttingDown reports whether Shutdown has begun.
func (s *Server) shuttingDown() bool { return s.ctx.Err() != nil }

// spawn runs fn on a goroutine tracked by bg and reports true; once
// Shutdown has begun it runs nothing and reports false, so callers can
// undo whatever fn was going to own.
func (s *Server) spawn(fn func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown() {
		return false
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn()
	}()
	return true
}

// handler builds the route table.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/capabilities", s.handleCapabilities)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	mux.HandleFunc("GET /api/sources/{hash}", s.handleGetSource)
	mux.HandleFunc("POST /api/still", s.handleStill)
	mux.HandleFunc("POST /api/jobs", s.handleCreateJob)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleCancelJob)
	mux.HandleFunc("GET /api/jobs/{id}/events", s.handleJobEvents)
	mux.HandleFunc("GET /api/results/{hash}", s.handleGetResult)
	mux.HandleFunc("GET /out/{hash}/{name}", s.handleOutFile)
	// Anything else under /api is a JSON 404 (never the SPA fallback).
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such API endpoint")
	})
	mux.HandleFunc("/out/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	mux.HandleFunc("/", s.handleSPA)

	return securityHeaders(sameOriginGuard(mux))
}

// securityHeaders adds conservative defaults for a LAN tool.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

// sameOriginGuard refuses state-changing requests that a browser marks as
// coming from another site, so a web page the user happens to have open
// cannot drive the API on the LAN (classic CSRF; the API has no cookies,
// but a LAN address is reachable from any tab). See isCrossSite.
func sameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isCrossSite(r) {
			writeError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isCrossSite decides the guard for one request. Safe methods (GET, HEAD,
// OPTIONS) are never guarded. For the rest:
//
//   - Sec-Fetch-Site, which browsers stamp on every request and scripts
//     cannot forge, is authoritative when present: same-origin (the SPA)
//     and none (user-initiated) pass, cross-site is refused. same-site is
//     not good enough on its own — another port on the same host counts as
//     same-site — so it falls through to the Origin check.
//   - Otherwise Origin, which browsers always send on POST/DELETE, must name
//     this server (its host must match the Host header). Non-browser clients
//     such as curl send neither header and pass.
//
// Trusting a same-origin Sec-Fetch-Site over Origin keeps the SPA working
// behind a reverse proxy that rewrites Host to the upstream address.
func isCrossSite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "same-origin", "none":
		return false
	case "cross-site":
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	return !originMatchesHost(origin, r.Host)
}

// originMatchesHost reports whether the Origin header names the host the
// request was addressed to. Hosts compare case-insensitively; a port that
// is the default for the origin's scheme is ignored on either side. Opaque
// origins ("null") and non-http(s) schemes never match.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	var def string
	switch strings.ToLower(u.Scheme) {
	case "http":
		def = "80"
	case "https":
		def = "443"
	default:
		return false
	}
	return strings.EqualFold(stripDefaultPort(u.Host, def), stripDefaultPort(host, def))
}

// stripDefaultPort removes an explicit ":port" from hostport when it is
// def, keeping IPv6 brackets intact.
func stripDefaultPort(hostport, def string) string {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil || p != def {
		return hostport
	}
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

// ---- helpers ----------------------------------------------------------------

// writeJSON encodes v with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("server: encode response: %v", err)
	}
}

// writeError writes {"error": msg}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON reads a bounded JSON body into v and reports whether it
// succeeded; on failure the error response has already been written. The
// request must declare Content-Type application/json (an optional charset
// parameter is fine) — 415 otherwise, which also keeps HTML forms, whose
// Content-Type browsers pin to urlencoded/multipart, off these endpoints —
// and the body must decode (400, naming what was expected).
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, what string) bool {
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+what+": "+errText(err))
		return false
	}
	return true
}

// isJSONContentType accepts "application/json" with any parameters
// (charset=utf-8 typically), case-insensitively.
func isJSONContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// errText trims an error for a client-facing message.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok"))
}
