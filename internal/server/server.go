// Package server exposes Lissandra's HTTP surface: the admin UI and REST API,
// the push ingest endpoint, health checks and Prometheus metrics.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/lissandra/internal/auth"
	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine"
	"github.com/vipinmohan22/lissandra/internal/events"
	"github.com/vipinmohan22/lissandra/internal/importer"
	"github.com/vipinmohan22/lissandra/internal/learn"
	"github.com/vipinmohan22/lissandra/internal/notify"
	"github.com/vipinmohan22/lissandra/internal/pipeline"
	"github.com/vipinmohan22/lissandra/internal/poller"
)

// Options configures the server.
type Options struct {
	Version        string
	Auth           *auth.Store // the admin token; nil accepts AdminToken alone (tests)
	AdminToken     string
	OnNomad        bool   // running as a Nomad allocation, for the UI's hints
	PublicURL      string // fallback when Settings.PublicURL is empty
	MaxIngestBytes int64
	MaxImportBytes int64 // upload cap for log file analysis

	MaxImportDecompressed int64 // cap on decompressed input per analysis
	UI                    fs.FS // contents served at /
}

// Server wires HTTP handlers to Lissandra's components.
type Server struct {
	opts       Options
	log        *slog.Logger
	cfg        *config.Store
	store      *events.Store
	pipeline   *pipeline.Pipeline
	dispatcher *notify.Dispatcher
	classifier *engine.Classifier
	learner    *learn.Learner // may be nil
	pollers    *poller.Manager
	imports    *importer.Registry
	auth       *auth.Store
	started    time.Time

	// sources is a cheap snapshot for the hot ingest path.
	sources atomic.Pointer[map[string]config.Source]
}

// New builds a Server and subscribes it to config changes.
func New(opts Options, log *slog.Logger, cfg *config.Store, store *events.Store, p *pipeline.Pipeline,
	d *notify.Dispatcher, classifier *engine.Classifier, learner *learn.Learner, pm *poller.Manager, imports *importer.Registry) *Server {
	if opts.MaxIngestBytes <= 0 {
		opts.MaxIngestBytes = 10 << 20
	}
	if opts.MaxImportBytes <= 0 {
		opts.MaxImportBytes = 100 << 20
	}
	if opts.MaxImportDecompressed <= 0 {
		opts.MaxImportDecompressed = 512 << 20
	}
	if opts.Auth == nil {
		opts.Auth = auth.Static(opts.AdminToken)
	}
	s := &Server{opts: opts, log: log, cfg: cfg, store: store, pipeline: p, dispatcher: d, classifier: classifier, learner: learner, pollers: pm, imports: imports,
		auth: opts.Auth, started: time.Now()}
	cfg.Subscribe(func(c *config.Config) {
		m := make(map[string]config.Source, len(c.Sources))
		for _, src := range c.Sources {
			m[src.ID] = src
		}
		s.sources.Store(&m)
	})
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /ingest/{id}", s.handleIngest)

	api := http.NewServeMux()
	s.routes(api)
	mux.Handle("/api/", s.requireAdmin(api))

	if s.opts.UI != nil {
		static := http.FileServerFS(s.opts.UI)
		mux.Handle("/", securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			if r.URL.Path == "/" || r.URL.Path == "/index.html" {
				w.Header().Set("Cache-Control", "no-cache")
			}
			static.ServeHTTP(w, r)
		})))
	}
	return s.logRequests(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// requireAdmin checks the admin token on every request, against the token
// active now, so a change applies from the next request.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !s.auth.Check(bearer(r)) {
			writeError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer returns the request's bearer token, or "".
func bearer(r *http.Request) string {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(got)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (used to
// extend deadlines for long uploads).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			return
		}
		lvl := slog.LevelDebug
		if rec.status >= 400 {
			lvl = slog.LevelInfo
		}
		s.log.Log(r.Context(), lvl, "http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

// publicURL returns the externally reachable base URL, from settings, the
// environment, or the request itself.
func (s *Server) publicURL(r *http.Request, c *config.Config) string {
	if c.Settings.PublicURL != "" {
		return c.Settings.PublicURL
	}
	if s.opts.PublicURL != "" {
		return strings.TrimRight(s.opts.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeErrorCode adds a machine-readable code for the UI to act on.
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// apiError is an error a config update returns to be answered with its own
// status and code rather than a 400.
type apiError struct {
	status    int
	code, msg string
}

func (e *apiError) Error() string { return e.msg }

// readBody reads a JSON request body (up to 2 MiB) for decoding later.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	case err != nil:
		writeError(w, http.StatusBadRequest, "reading the request: "+err.Error())
	case len(bytes.TrimSpace(data)) == 0:
		writeError(w, http.StatusBadRequest, "request body is empty")
	default:
		return data, true
	}
	return nil, false
}

// readStrictJSON decodes a JSON body (up to 2 MiB) into v, refusing
// unknown fields and anything after the value, for the endpoints that
// change the admin token and setup state.
func readStrictJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&json.RawMessage{}) != io.EOF {
		err = errors.New("unexpected data after the JSON value")
	}
	var mbe *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &mbe):
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	case errors.Is(err, io.EOF):
		writeErrorCode(w, http.StatusBadRequest, "invalid_json", "request body is empty")
	default:
		writeErrorCode(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error())
	}
	return false
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		case errors.Is(err, io.EOF):
			writeError(w, http.StatusBadRequest, "request body is empty")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return false
	}
	return true
}
