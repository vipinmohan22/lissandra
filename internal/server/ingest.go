package server

import (
	"compress/gzip"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/ingest"
	"github.com/vipinmohan22/lissandra/internal/pipeline"
)

// handleIngest accepts logs for a push source:
//
//	POST /ingest/{source id or name}
//	Authorization: Bearer <ingest token>   (or X-Lissandra-Token, or ?token=)
//
// Bodies may be JSON (object or array), NDJSON or plain text, optionally
// gzip-encoded.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	src, ok := s.lookupSource(r.PathValue("id"))
	if !ok || src.Type != config.SourcePush {
		writeError(w, http.StatusNotFound, "unknown source")
		return
	}
	if !src.Enabled {
		writeError(w, http.StatusForbidden, "source is disabled")
		return
	}
	token := r.Header.Get("X-Lissandra-Token")
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		token = bearer
	}
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(src.IngestToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid ingest token")
		return
	}

	var body io.Reader = http.MaxBytesReader(w, r.Body, s.opts.MaxIngestBytes)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid gzip body")
			return
		}
		defer gz.Close()
		// Bound the decompressed size too.
		body = io.LimitReader(gz, 4*s.opts.MaxIngestBytes+1)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(data)) > 4*s.opts.MaxIngestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "decompressed payload too large")
		return
	}

	evs, err := ingest.Parse(data, r.Header.Get("Content-Type"), &src, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.pipeline.Submit(evs)
	if errors.Is(err, pipeline.ErrQueueFull) {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "queue full, retry later", "accepted": n, "rejected": len(evs) - n})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": n})
}

func (s *Server) lookupSource(key string) (config.Source, bool) {
	m := s.sources.Load()
	if m == nil {
		return config.Source{}, false
	}
	if src, ok := (*m)[key]; ok {
		return src, true
	}
	for _, src := range *m {
		if src.Name == key {
			return src, true
		}
	}
	return config.Source{}, false
}
