package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/importer"
	"github.com/vipinmohan22/layalogs/internal/ingest"
	"github.com/vipinmohan22/layalogs/internal/model"
)

const (
	// importDeadline bounds one analysis, including processing; the socket
	// deadlines are a little longer so the report can still be returned.
	importDeadline = 45 * time.Minute
	socketDeadline = 50 * time.Minute

	// maxImportRecordBytes caps one JSON record: decoding inflates it
	// several times in memory.
	maxImportRecordBytes = 1 << 20

	maxFormParts     = 8        // non-file multipart parts allowed before the file
	maxFormPartBytes = 4 << 10  // per part
	maxFormBytes     = 64 << 10 // all of them together

	// uploadSourceName names events analyzed with default parsing. The file
	// name is kept separately (Event.File) because it is user-controlled.
	uploadSourceName = "file upload"
)

// ImportFormats lists the formats accepted for file analysis.
var ImportFormats = []string{config.FormatAuto, config.FormatText, config.FormatNDJSON, config.FormatJSON, ingest.FormatCSV}

func (s *Server) importRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/imports", s.createImport)
	mux.HandleFunc("GET /api/imports", s.listImports)
	mux.HandleFunc("GET /api/imports/{id}", s.getImport)
	mux.HandleFunc("GET /api/imports/{id}/events", s.listImportEvents)
	mux.HandleFunc("GET /api/imports/{id}/events/{eid}", s.getImportEvent)
	mux.HandleFunc("GET /api/imports/{id}/patterns", s.listImportPatterns)
	mux.HandleFunc("DELETE /api/imports/{id}", s.deleteImport)
}

// importParams are the options of an upload, from the query string or from
// multipart form fields sent before the file.
type importParams struct {
	id, name, source, format, tz string
}

func (p *importParams) set(key, value string) {
	switch key {
	case "id":
		p.id = value
	case "name":
		p.name = value
	case "source":
		p.source = value
	case "format":
		p.format = value
	case "tz":
		p.tz = value
	}
}

// createImport analyzes an uploaded log file. The body is either
// multipart/form-data with a "file" part, or the raw file (gzip is detected).
// Processing streams while the upload is read; the response is the final
// report. Nothing is stored as live events and no notifications are sent.
//
//	curl -F file=@app.log -H "Authorization: Bearer $TOKEN" https://laya/api/imports
//	curl --data-binary @app.log.gz -H "Authorization: Bearer $TOKEN" "https://laya/api/imports?name=app.log.gz&source=<id>&tz=Europe/Berlin"
//
// Clients may pass ?id=<16 hex chars> to follow the import (GET
// /api/imports/{id}) while the upload is still in flight.
func (s *Server) createImport(w http.ResponseWriter, r *http.Request) {
	limit := s.opts.MaxImportBytes
	if r.ContentLength > limit {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file is larger than the %d MB upload limit; gzip it, or use `laya analyze` locally", limit>>20))
		return
	}
	var p importParams
	q := r.URL.Query()
	for _, k := range []string{"id", "name", "source", "format", "tz"} {
		if q.Has(k) {
			p.set(k, q.Get(k))
		}
	}

	// Take a slot before reading the body or extending deadlines, so idle
	// or junk uploads can't hold connections open without one.
	im, err := s.imports.Begin(p.id, cleanFileName(p.name), time.Now())
	switch {
	case errors.Is(err, importer.ErrBusy):
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	case errors.Is(err, importer.ErrBadID):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, importer.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer s.imports.Done(im)
	fail := func(status int, msg string) {
		importer.Fail(im, msg)
		writeError(w, status, msg)
	}

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(socketDeadline))
	_ = rc.SetWriteDeadline(time.Now().Add(socketDeadline))

	r.Body = http.MaxBytesReader(w, r.Body, limit)
	var file io.Reader = r.Body
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		part, err := findFilePart(r, &p)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		defer part.Close()
		if p.name == "" {
			p.name = part.FileName()
		}
		file = part
	}
	name := cleanFileName(p.name)

	format := p.format
	if format == "" {
		format = config.FormatAuto
	}
	if !contains(ImportFormats, format) {
		fail(http.StatusBadRequest, fmt.Sprintf("unknown format %q (use %s)", format, strings.Join(ImportFormats, ", ")))
		return
	}
	loc := time.UTC
	if p.tz != "" {
		if loc, err = time.LoadLocation(p.tz); err != nil {
			fail(http.StatusBadRequest, fmt.Sprintf("unknown time zone %q", p.tz))
			return
		}
	}
	src := config.Source{Name: uploadSourceName, Type: config.SourcePush, Parse: config.Parse{Format: config.FormatAuto}}
	if p.source != "" {
		found, ok := s.lookupSource(p.source)
		if !ok {
			fail(http.StatusBadRequest, "unknown source "+strconv.Quote(p.source))
			return
		}
		src = found
	}
	if p.format == "" && p.source != "" {
		format = "" // use the source's own format
	}

	engine := s.pipeline.Engine()
	err = importer.Run(r.Context(), im, uploadReader{file, limit}, importer.Options{
		FileName:       name,
		Source:         src,
		Format:         format,
		Location:       loc,
		MaxBytes:       s.opts.MaxImportDecompressed,
		MaxRecordBytes: maxImportRecordBytes,
		Deadline:       time.Now().Add(importDeadline),
		Workers:        1, // leave the other cores to live ingest
	}, importer.Deps{
		Classify: func(ev *model.Event) bool { return s.pipeline.ClassifyOffline(engine, ev) },
		Routes:   s.dispatcher.Matches,
		Busy:     s.pipeline.Busy,
	})

	rep := im.Report(true)
	status := http.StatusCreated
	var unsupported *ingest.UnsupportedError
	switch {
	case errors.As(err, &unsupported):
		status = http.StatusUnsupportedMediaType
	case err != nil:
		s.log.Info("log file analysis failed", "file", name, "err", err)
		status = http.StatusBadRequest
	}
	writeJSON(w, status, rep)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// findFilePart reads multipart fields up to the "file" part, collecting
// options sent as fields before it. It allows only a few small fields.
func findFilePart(r *http.Request, p *importParams) (*multipart.Part, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("invalid multipart body: %w", err)
	}
	total := 0
	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errors.New(`no "file" part in the upload`)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid multipart body: %w", err)
		}
		if part.FormName() == "file" {
			return part, nil
		}
		if i >= maxFormParts {
			part.Close()
			return nil, errors.New(`too many form fields before the "file" part`)
		}
		value, err := io.ReadAll(io.LimitReader(part, maxFormPartBytes+1))
		part.Close()
		if err != nil {
			return nil, fmt.Errorf("invalid multipart body: %w", err)
		}
		if total += len(value); len(value) > maxFormPartBytes || total > maxFormBytes {
			return nil, errors.New("form fields too large")
		}
		p.set(part.FormName(), strings.TrimSpace(string(value)))
	}
}

// cleanFileName keeps the base name of an uploaded file for display only
// (it is never used as a path): no directories, control or bidi-formatting
// characters, at most 200 bytes.
func cleanFileName(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		name = "upload.log"
	}
	return model.Truncate(name, 200)
}

// uploadReader turns http.MaxBytesError into an early, truncated stop: the
// part of the file read so far is still analyzed.
type uploadReader struct {
	r     io.Reader
	limit int64
}

func (u uploadReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		err = &importer.StopError{Reason: importer.StopUploadLimit,
			Err: fmt.Errorf("the file is larger than the %d MB upload limit; only the first part was analyzed", u.limit>>20)}
	}
	return n, err
}

func (s *Server) listImports(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.imports.List())
}

func (s *Server) findImport(w http.ResponseWriter, r *http.Request) (*importer.Import, bool) {
	im, ok := s.imports.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "import not found (analyses are kept in memory and reset on restart)")
	}
	return im, ok
}

func (s *Server) getImport(w http.ResponseWriter, r *http.Request) {
	if im, ok := s.findImport(w, r); ok {
		writeJSON(w, http.StatusOK, im.Report(true))
	}
}

// listImportEvents returns retained events. The JSON listing carries
// excerpts without fields (open one event for everything);
// ?format=ndjson downloads every match in full.
func (s *Server) listImportEvents(w http.ResponseWriter, r *http.Request) {
	im, ok := s.findImport(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	query := importer.Query{
		MinSeverity: model.Severity(q.Get("min_severity")),
		Category:    q.Get("category"),
		Fingerprint: q.Get("fingerprint"),
		Text:        q.Get("q"),
	}
	if q.Get("format") == "ndjson" {
		query.Limit = -1
		evs, _ := im.Events(query)
		rep := im.Report(false)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="laya-%s-events.ndjson"`, rep.ID))
		bw := bufio.NewWriter(w)
		enc := json.NewEncoder(bw)
		enc.SetEscapeHTML(false)
		for i := range evs {
			if enc.Encode(&evs[i]) != nil {
				return
			}
		}
		bw.Flush()
		return
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	query.Offset, query.Limit = max(offset, 0), min(max(limit, 1), 200)
	if limit == 0 {
		query.Limit = 50
	}
	evs, total := im.Events(query)
	for i := range evs {
		evs[i].Message = model.Truncate(evs[i].Message, 2048)
		evs[i].Fields = nil
	}
	resp := map[string]any{"total": total, "events": evs, "running": im.Running()}
	if query.Fingerprint != "" {
		// How many events the pattern had in the file, versus kept to browse.
		if p := im.Patterns(importer.PatternQuery{Fingerprint: query.Fingerprint, Limit: 1}); len(p) == 1 {
			resp["pattern"] = p[0]
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getImportEvent(w http.ResponseWriter, r *http.Request) {
	im, ok := s.findImport(w, r)
	if !ok {
		return
	}
	ev, ok := im.Event(r.PathValue("eid"))
	if !ok {
		writeError(w, http.StatusNotFound, "event not found in this analysis")
		return
	}
	routes := s.dispatcher.Matches(&ev)
	if routes == nil {
		routes = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev, "routes": routes})
}

func (s *Server) listImportPatterns(w http.ResponseWriter, r *http.Request) {
	im, ok := s.findImport(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	writeJSON(w, http.StatusOK, im.Patterns(importer.PatternQuery{
		MinSeverity: model.Severity(q.Get("min_severity")),
		Category:    q.Get("category"),
		Text:        q.Get("q"),
		Limit:       limit,
	}))
}

func (s *Server) deleteImport(w http.ResponseWriter, r *http.Request) {
	switch err := s.imports.Delete(r.PathValue("id")); {
	case errors.Is(err, importer.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, importer.ErrRunning):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
