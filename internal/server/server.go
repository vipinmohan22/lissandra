// Package server exposes Laya's HTTP surface: the admin UI and REST API,
// the push ingest endpoint, health checks and Prometheus metrics.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/engine"
	"github.com/vipinmohan22/layalogs/internal/events"
	"github.com/vipinmohan22/layalogs/internal/importer"
	"github.com/vipinmohan22/layalogs/internal/notify"
	"github.com/vipinmohan22/layalogs/internal/pipeline"
	"github.com/vipinmohan22/layalogs/internal/poller"
)

// Options configures the server.
type Options struct {
	Version        string
	AdminToken     string
	PublicURL      string // fallback when Settings.PublicURL is empty
	MaxIngestBytes int64
	MaxImportBytes int64 // upload cap for log file analysis

	MaxImportDecompressed int64 // cap on decompressed input per analysis
	UI                    fs.FS // contents served at /
}

// Server wires HTTP handlers to Laya's components.
type Server struct {
	opts       Options
	log        *slog.Logger
	cfg        *config.Store
	store      *events.Store
	pipeline   *pipeline.Pipeline
	dispatcher *notify.Dispatcher
	laya       *engine.Classifier
	pollers    *poller.Manager
	imports    *importer.Registry
	started    time.Time

	// sources is a cheap snapshot for the hot ingest path.
	sources atomic.Pointer[map[string]config.Source]
}

// New builds a Server and subscribes it to config changes.
func New(opts Options, log *slog.Logger, cfg *config.Store, store *events.Store, p *pipeline.Pipeline,
	d *notify.Dispatcher, laya *engine.Classifier, pm *poller.Manager, imports *importer.Registry) *Server {
	if opts.MaxIngestBytes <= 0 {
		opts.MaxIngestBytes = 10 << 20
	}
	if opts.MaxImportBytes <= 0 {
		opts.MaxImportBytes = 100 << 20
	}
	if opts.MaxImportDecompressed <= 0 {
		opts.MaxImportDecompressed = 512 << 20
	}
	s := &Server{opts: opts, log: log, cfg: cfg, store: store, pipeline: p, dispatcher: d, laya: laya, pollers: pm, imports: imports, started: time.Now()}
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

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	want := []byte(s.opts.AdminToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), want) != 1 {
			writeError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		next.ServeHTTP(w, r)
	})
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
