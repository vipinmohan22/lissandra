package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinmohan22/layalogs/internal/importer"
	"github.com/vipinmohan22/layalogs/internal/model"
)

const sampleLog = "2026-10-11T10:00:00Z INFO started\n" +
	"2026-10-11T10:00:05Z ERROR payment failed: connection refused to db:5432\n" +
	"2026-10-11T10:00:06Z ERROR payment failed: connection refused to db:5433\n" +
	"2026-10-11T10:01:00Z WARN login failed for user bob\n"

func multipartBody(t *testing.T, fields map[string]string, name, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", name)
	io.WriteString(fw, content)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func authHeaders(extra map[string]string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + adminToken}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func TestImportMultipartAndQueries(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.do("POST", "/api/imports", sampleLog, nil); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated upload: %d", code)
	}

	body, ct := multipartBody(t, map[string]string{"tz": "UTC"}, "../../etc/app\u202egol.log", sampleLog)
	code, raw := e.do("POST", "/api/imports?id=00112233445566aa", body.Bytes(), authHeaders(map[string]string{"Content-Type": ct}))
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, raw)
	}
	var rep importer.Report
	json.Unmarshal(raw, &rep)
	if rep.ID != "00112233445566aa" || rep.Events != 4 || rep.Status != importer.StatusDone || rep.SourceName != "file upload" {
		t.Fatalf("report %+v", rep)
	}
	if rep.FileName != "appgol.log" {
		t.Errorf("file name not sanitized: %q", rep.FileName)
	}
	if rep.Timeline == nil || len(rep.TopPatterns) == 0 {
		t.Error("detail fields missing")
	}

	// Live events are untouched.
	var live []model.Event
	e.admin("GET", "/api/events", nil, &live)
	if len(live) != 0 {
		t.Errorf("analysis leaked %d events into the live store", len(live))
	}

	var list struct {
		Total   int           `json:"total"`
		Events  []model.Event `json:"events"`
		Running bool          `json:"running"`
		Pattern *importer.Pattern
	}
	e.admin("GET", "/api/imports/"+rep.ID+"/events?min_severity=error&limit=1", nil, &list)
	if list.Total != 2 || len(list.Events) != 1 || list.Running || list.Events[0].Fields != nil {
		t.Errorf("events listing %+v", list)
	}
	fp := list.Events[0].Fingerprint
	e.admin("GET", "/api/imports/"+rep.ID+"/events?fingerprint="+fp, nil, &list)
	if list.Pattern == nil || list.Pattern.Count != 2 {
		t.Errorf("pattern info with fingerprint filter: %+v", list.Pattern)
	}

	var detail struct {
		Event  model.Event `json:"event"`
		Routes []string    `json:"routes"`
	}
	if code := e.admin("GET", "/api/imports/"+rep.ID+"/events/"+list.Events[0].ID, nil, &detail); code != 200 || detail.Event.Line != 2 || detail.Routes == nil {
		t.Errorf("event detail %d %+v", code, detail)
	}
	if code := e.admin("GET", "/api/imports/"+rep.ID+"/events/nope", nil, nil); code != 404 {
		t.Errorf("missing event: %d", code)
	}

	code, nd := e.do("GET", "/api/imports/"+rep.ID+"/events?format=ndjson&min_severity=warning", nil, authHeaders(nil))
	if code != 200 || strings.Count(string(nd), "\n") != 3 {
		t.Errorf("ndjson download %d: %q", code, nd)
	}

	var pats []importer.Pattern
	e.admin("GET", "/api/imports/"+rep.ID+"/patterns?category=uncategorized", nil, &pats)
	if len(pats) != 1 || pats[0].Category != "uncategorized" {
		t.Errorf("patterns %+v", pats)
	}

	var reports []importer.Report
	e.admin("GET", "/api/imports", nil, &reports)
	if len(reports) != 1 || reports[0].Timeline != nil {
		t.Errorf("list should hold summaries: %+v", reports)
	}
	if code := e.admin("POST", "/api/imports?id=00112233445566aa", "x", nil); code != http.StatusConflict {
		t.Errorf("duplicate id: %d", code)
	}
	if code := e.admin("DELETE", "/api/imports/"+rep.ID, nil, nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code := e.admin("GET", "/api/imports/"+rep.ID, nil, nil); code != 404 {
		t.Errorf("deleted import still there: %d", code)
	}
}

func TestImportRawGzipWithSource(t *testing.T) {
	e := newEnv(t)
	var sources []struct{ ID, Name string }
	e.admin("GET", "/api/sources", nil, &sources)
	var gz bytes.Buffer
	gzw := newGzip(&gz)
	io.WriteString(gzw, sampleLog)
	gzw.Close()
	code, raw := e.do("POST", "/api/imports?name=app.log.gz&tz=Europe/Berlin&source="+sources[0].ID, gz.Bytes(), authHeaders(nil))
	var rep importer.Report
	json.Unmarshal(raw, &rep)
	if code != 201 || !rep.Compressed || rep.SourceName != sources[0].Name || rep.TimeZone != "Europe/Berlin" || rep.Events != 4 {
		t.Errorf("%d %+v", code, rep)
	}
}

func TestImportValidationAndLimits(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		query string
		body  string
		want  int
	}{
		{"?format=xml", sampleLog, 400},
		{"?tz=Mars/Olympus", sampleLog, 400},
		{"?source=nope", sampleLog, 400},
		{"?id=XYZ", sampleLog, 400},
		{"", "PK\x03\x04 a zip", http.StatusUnsupportedMediaType},
	}
	for _, c := range cases {
		code, raw := e.do("POST", "/api/imports"+c.query, c.body, authHeaders(nil))
		if code != c.want {
			t.Errorf("%q: %d %s", c.query, code, raw)
		}
	}
	// Rejected requests leave no running imports behind.
	var reports []importer.Report
	e.admin("GET", "/api/imports", nil, &reports)
	for _, r := range reports {
		if r.Status == importer.StatusRunning {
			t.Errorf("import left running: %+v", r)
		}
	}

	// Too many form fields before the file.
	fields := map[string]string{}
	for i := 0; i < 12; i++ {
		fields[fmt.Sprint("f", i)] = "v"
	}
	body, ct := multipartBody(t, fields, "a.log", sampleLog)
	if code, _ := e.do("POST", "/api/imports", body.Bytes(), authHeaders(map[string]string{"Content-Type": ct})); code != 400 {
		t.Errorf("too many fields: %d", code)
	}
}

func TestImportSizeLimits(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.MaxImportBytes = 4096 })
	big := strings.Repeat("2026-10-11T10:00:00Z ERROR something failed badly\n", 400)

	// Declared too large: rejected before reading.
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/imports", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("declared too large: %d", resp.StatusCode)
	}

	// Chunked (no length): analyzed up to the limit, marked partial.
	req, _ = http.NewRequest("POST", e.srv.URL+"/api/imports", io.MultiReader(strings.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var rep importer.Report
	json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if resp.StatusCode != 201 || !rep.Truncated || rep.StopReason != importer.StopUploadLimit || rep.Events == 0 {
		t.Errorf("chunked over limit: %d %+v", resp.StatusCode, rep)
	}
}

// slowBody sends a little, then waits until released.
type slowBody struct {
	sent    bool
	release chan struct{}
}

func (s *slowBody) Read(p []byte) (int, error) {
	if !s.sent {
		s.sent = true
		return copy(p, "2026-10-11T10:00:00Z INFO first\n"), nil
	}
	<-s.release
	return 0, io.EOF
}

func TestImportOneAtATime(t *testing.T) {
	e := newEnv(t)
	slow := &slowBody{release: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/imports?id=aaaaaaaaaaaaaaaa", slow)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	// Wait until the first import is visible as running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var rep importer.Report
		if e.admin("GET", "/api/imports/aaaaaaaaaaaaaaaa", nil, &rep) == 200 && rep.Status == importer.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first import never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := e.admin("POST", "/api/imports", sampleLog, nil); code != http.StatusTooManyRequests {
		t.Errorf("second concurrent import: %d", code)
	}
	if code := e.admin("DELETE", "/api/imports/aaaaaaaaaaaaaaaa", nil, nil); code != http.StatusConflict {
		t.Errorf("delete while running: %d", code)
	}
	close(slow.release)
	wg.Wait()
	// The slot is released: the next import works.
	if code := e.admin("POST", "/api/imports", sampleLog, nil); code != http.StatusCreated {
		t.Errorf("import after release: %d", code)
	}
}

func TestInfoExposesImportLimits(t *testing.T) {
	e := newEnv(t)
	var info struct {
		Imports struct {
			MaxUploadBytes int64    `json:"max_upload_bytes"`
			Formats        []string `json:"formats"`
		} `json:"imports"`
	}
	e.admin("GET", "/api/info", nil, &info)
	if info.Imports.MaxUploadBytes != 100<<20 || len(info.Imports.Formats) != 5 {
		t.Errorf("info %+v", info.Imports)
	}
}

func TestCleanFileName(t *testing.T) {
	cases := map[string]string{
		"app.log":                 "app.log",
		`C:\logs\app.log`:         "app.log",
		"../../etc/passwd":        "passwd",
		"a\x00b\nc.log":           "abc.log",
		"in\u202evoice\u2066.log": "invoice.log",
		"":                        "upload.log",
		"/":                       "upload.log",
		strings.Repeat("é", 300):  strings.Repeat("é", 100) + "…",
	}
	for in, want := range cases {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInvalidRequestsKeepFinishedReports(t *testing.T) {
	e := newEnv(t) // keeps 3
	for i := 0; i < 3; i++ {
		if code := e.admin("POST", "/api/imports", sampleLog, nil); code != http.StatusCreated {
			t.Fatalf("upload %d: %d", i, code)
		}
	}
	// Bad query options are rejected before an import is created.
	for i := 0; i < 3; i++ {
		if code := e.admin("POST", "/api/imports?tz=Europe/Berlinn", sampleLog, nil); code != 400 {
			t.Errorf("bad tz: %d", code)
		}
	}
	// Bad multipart options do create a failed import, which goes first.
	body, ct := multipartBody(t, map[string]string{"format": "xml"}, "a.log", sampleLog)
	if code, _ := e.do("POST", "/api/imports", body.Bytes(), authHeaders(map[string]string{"Content-Type": ct})); code != 400 {
		t.Errorf("bad multipart format: %d", code)
	}
	e.admin("POST", "/api/imports", sampleLog, nil)
	var reports []importer.Report
	e.admin("GET", "/api/imports", nil, &reports)
	done := 0
	for _, r := range reports {
		if r.Status == importer.StatusDone {
			done++
		}
	}
	if len(reports) != 3 || done != 3 {
		t.Errorf("finished reports pushed out: %d listed, %d done", len(reports), done)
	}
}

// pacedBody sends a multipart upload slowly until the server stops reading.
type pacedBody struct {
	head   []byte
	chunks int
	sent   int
}

func (b *pacedBody) Read(p []byte) (int, error) {
	if len(b.head) > 0 {
		n := copy(p, b.head)
		b.head = b.head[n:]
		return n, nil
	}
	if b.sent >= b.chunks {
		return 0, io.EOF
	}
	b.sent++
	time.Sleep(50 * time.Millisecond)
	return copy(p, bytes.Repeat([]byte{'z'}, min(len(p), 32<<10))), nil
}

func TestEarlyStopDoesNotDrainMultipartUpload(t *testing.T) {
	e := newEnv(t)
	head := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.zip\"\r\nContent-Type: application/octet-stream\r\n\r\nPK\x03\x04"
	body := &pacedBody{head: []byte(head), chunks: 200} // ~10 s if drained
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/imports", body)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType || time.Since(start) > 3*time.Second {
		t.Errorf("status %d after %v", resp.StatusCode, time.Since(start))
	}
	if n := e.imports.Active(); n != 0 {
		t.Errorf("slot still held: %d", n)
	}
}
