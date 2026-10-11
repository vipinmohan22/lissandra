package ingest

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

var streamNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func streamAll(t *testing.T, input string, opts StreamOptions) ([]model.Event, StreamStats, error) {
	t.Helper()
	src := &config.Source{ID: "s", Name: "file", Parse: config.Parse{Format: config.FormatAuto}}
	var evs []model.Event
	stats, err := Stream(strings.NewReader(input), src, streamNow, opts, func(ev model.Event) error {
		evs = append(evs, ev)
		return nil
	})
	return evs, stats, err
}

func messages(evs []model.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Message
	}
	return out
}

func TestStreamAnchoredGrouping(t *testing.T) {
	logback := "2026-10-11 10:00:00,001 INFO  [main] started\n" +
		"2026-10-11 10:00:05,120 ERROR [http-1] Request failed\n" +
		"java.lang.IllegalStateException: boom\n" +
		"\tat com.acme.Pay.charge(Pay.java:42)\n" +
		"Caused by: java.sql.SQLException: Connection refused\n" +
		"\t... 12 more\n" +
		"2026-10-11 10:00:06,000 WARN  [http-2] login failed\n"
	evs, stats, err := streamAll(t, logback, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Mode != "text-anchored" || len(evs) != 3 {
		t.Fatalf("mode %s, %d events: %q", stats.Mode, len(evs), messages(evs))
	}
	if !strings.Contains(evs[1].Message, "IllegalStateException") || !strings.Contains(evs[1].Message, "Caused by") || evs[1].Line != 2 {
		t.Errorf("exception not folded into its log line: %+v", evs[1])
	}
	if evs[2].Line != 7 || stats.FoldedLines != 4 || stats.Lines != 7 {
		t.Errorf("line accounting: line=%d folded=%d lines=%d", evs[2].Line, stats.FoldedLines, stats.Lines)
	}

	python := "2026-10-11 10:00:01 ERROR job failed\nTraceback (most recent call last):\n  File \"w.py\", line 1\nValueError: bad\n2026-10-11 10:00:02 INFO next\n2026-10-11 10:00:03 INFO more\n"
	evs, _, _ = streamAll(t, python, StreamOptions{})
	if len(evs) != 3 || !strings.HasSuffix(evs[0].Message, "ValueError: bad") {
		t.Errorf("python traceback: %q", messages(evs))
	}

	// Without enough timestamped lines, indent folding applies.
	plain := "start\nERROR boom\n  at x\nnext line\n"
	evs, stats, _ = streamAll(t, plain, StreamOptions{})
	if stats.Mode != "text" || len(evs) != 3 {
		t.Errorf("indent mode: %s %q", stats.Mode, messages(evs))
	}
}

func TestStreamContinuationCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("first\n")
	for i := 0; i < maxContinuationLines+50; i++ {
		b.WriteString("  frame\n")
	}
	b.WriteString("second\n")
	evs, stats, err := streamAll(t, b.String(), StreamOptions{Format: config.FormatText})
	if err != nil || len(evs) != 2 {
		t.Fatalf("got %d events, err %v", len(evs), err)
	}
	if stats.DroppedLines != 50 || len(evs[0].Message) > MaxMessageBytes+10 {
		t.Errorf("dropped=%d len=%d", stats.DroppedLines, len(evs[0].Message))
	}
}

func TestStreamTimestampFormats(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	cases := []struct {
		line string
		want time.Time
		loc  *time.Location
	}{
		{"2026-10-11T10:15:02.5Z x", time.Date(2026, 10, 11, 10, 15, 2, 5e8, time.UTC), nil},
		{"2026-10-11 10:15:02,123 ERROR x", time.Date(2026, 10, 11, 10, 15, 2, 123e6, time.UTC), nil},
		{"2026-10-11 10:15:02 x", time.Date(2026, 10, 11, 8, 15, 2, 0, time.UTC), berlin}, // zone-less, read in Berlin (CEST)
		{"2026-10-11T10:15:02+05:30 x", time.Date(2026, 10, 11, 4, 45, 2, 0, time.UTC), berlin},
		{"[2026-10-11 10:15:02] local.ERROR: x", time.Date(2026, 10, 11, 10, 15, 2, 0, time.UTC), nil},
		{"<34>1 2026-10-11T10:15:02Z host app - - x", time.Date(2026, 10, 11, 10, 15, 2, 0, time.UTC), nil},
		{"2026/10/11 10:15:02 [error] x", time.Date(2026, 10, 11, 10, 15, 2, 0, time.UTC), nil},
		{"Oct 11 10:15:02 web1 sshd[1]: x", time.Date(2026, 10, 11, 10, 15, 2, 0, time.UTC), nil},
		{"Dec 31 23:59:59 web1 cron: x", time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC), nil}, // year rolls back
		{"[Sun Oct 11 10:15:02.123456 2026] [error] x", time.Date(2026, 10, 11, 10, 15, 2, 0, time.UTC), nil},
		{`10.0.0.1 - - [11/Oct/2026:10:15:02 +0200] "GET / HTTP/1.1" 200`, time.Date(2026, 10, 11, 8, 15, 2, 0, time.UTC), nil},
		{"E1011 10:15:02.123456 1 c.go:1] x", time.Date(2026, 10, 11, 10, 15, 2, 123456e3, time.UTC), nil},
	}
	for _, c := range cases {
		evs, _, err := streamAll(t, c.line+"\n", StreamOptions{Location: c.loc})
		if err != nil || len(evs) != 1 {
			t.Fatalf("%q: %v %d", c.line, err, len(evs))
		}
		if !evs[0].Timestamp.Equal(c.want) || evs[0].TimeSource != "" {
			t.Errorf("%q: got %v (%q), want %v", c.line, evs[0].Timestamp, evs[0].TimeSource, c.want)
		}
	}
}

func TestStreamInferredAndMissingTimes(t *testing.T) {
	evs, stats, _ := streamAll(t, "no time here\n2026-10-11T10:00:00Z first\n{\"msg\":\"json without time\"}\n", StreamOptions{})
	if len(evs) != 3 {
		t.Fatalf("%q", messages(evs))
	}
	if evs[0].TimeSource != model.TimeNone || !evs[0].Timestamp.IsZero() {
		t.Errorf("record before any timestamp: %+v", evs[0])
	}
	if evs[2].TimeSource != model.TimeInferred || !evs[2].Timestamp.Equal(evs[1].Timestamp) {
		t.Errorf("record after a timestamp should inherit it: %+v", evs[2])
	}
	if stats.TimeParsed != 1 || stats.TimeInferred != 1 || stats.TimeMissing != 1 {
		t.Errorf("stats %+v", stats)
	}
}

func TestStreamAutoDetection(t *testing.T) {
	cases := []struct {
		name, input, mode string
		events            int
	}{
		{"bracketed text log", "[2026-10-11 10:00:00] local.ERROR: deadlock\n[2026-10-11 10:00:01] local.INFO: ok\n", "text", 2},
		{"apache error log", "[Sun Oct 11 10:00:00 2026] [error] boom\n[Sun Oct 11 10:00:01 2026] [notice] ok\n", "text", 2},
		{"ndjson", "{\"msg\":\"a\"}\n{\"msg\":\"b\"}\nplain text line\n", "ndjson", 3},
		{"ndjson with corrupt first line", "{\"msg\":\"a\"\n{\"msg\":\"b\"}\n", "text", 2},
		{"json array", "[{\"msg\":\"a\"},{\"msg\":\"b\"}]", "json", 2},
		{"pretty object stream", "{\n \"msg\": \"a\"\n}\n{\n \"msg\": \"b\"\n}\n", "json", 2},
		{"single object with wrapper", `{"logs":[{"msg":"a"},{"msg":"b"},{"msg":"c"}]}`, "json", 3},
		{"empty", "", "text", 0},
		{"BOM + JSON array", "\ufeff[{\"msg\":\"a\"}]", "json", 1},
	}
	for _, c := range cases {
		evs, stats, err := streamAll(t, c.input, StreamOptions{})
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if stats.Mode != c.mode || len(evs) != c.events {
			t.Errorf("%s: mode %q events %d (%q), want %q %d", c.name, stats.Mode, len(evs), messages(evs), c.mode, c.events)
		}
	}
	// A first NDJSON line longer than the peek window still decodes.
	big := `{"msg":"` + strings.Repeat("x", 100<<10) + `"}` + "\n" + `{"msg":"b"}` + "\n"
	evs, stats, err := streamAll(t, big, StreamOptions{})
	if err != nil || len(evs) != 2 || stats.Mode != "ndjson" {
		t.Errorf("100KB first line: %v mode=%s n=%d", err, stats.Mode, len(evs))
	}
}

func TestStreamJSONStreaming(t *testing.T) {
	es := `{
  "took": 3,
  "hits": {
    "total": 2,
    "hits": [
      {"_source": {"message": "a", "level": "error"}},
      {"_source": {"message": "b"}}
    ]
  }
}`
	evs, _, err := streamAll(t, es, StreamOptions{})
	if err != nil || len(evs) != 2 || evs[0].Level != "error" {
		t.Fatalf("es export: %v %q", err, messages(evs))
	}
	if evs[0].Line != 6 || evs[1].Line != 7 {
		t.Errorf("line numbers %d %d, want 6 7", evs[0].Line, evs[1].Line)
	}

	cloudtrail := `{"Records":[{"eventName":"ConsoleLogin","message":"login"},{"eventName":"x","message":"y"}]}`
	if evs, _, _ := streamAll(t, cloudtrail, StreamOptions{}); len(evs) != 2 {
		t.Errorf("CloudTrail Records not expanded: %q", messages(evs))
	}

	src := &config.Source{Parse: config.Parse{Format: config.FormatJSON, RecordsPath: "data.items"}}
	var got []string
	_, err = Stream(strings.NewReader(`{"meta":{"n":2},"data":{"items":[{"msg":"x"},{"msg":"y"}]}}`), src, streamNow, StreamOptions{}, func(ev model.Event) error {
		got = append(got, ev.Message)
		return nil
	})
	if err != nil || strings.Join(got, ",") != "x,y" {
		t.Errorf("records_path: %v %v", err, got)
	}
	_, err = Stream(strings.NewReader(`{"other":[1]}`), src, streamNow, StreamOptions{}, func(model.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "records path") {
		t.Errorf("missing records path should error, got %v", err)
	}
}

func TestStreamJSONErrorsAndLimits(t *testing.T) {
	// Malformed after some records: those records are kept.
	evs, _, err := streamAll(t, "[{\"msg\":\"a\"},\n{\"msg\":\"b\"},\n{\"msg\": oops}]", StreamOptions{Format: config.FormatJSON})
	var syn *SyntaxError
	if !errors.As(err, &syn) || len(evs) != 2 || syn.Line != 3 {
		t.Errorf("syntax error: %v (%T) line=%v events=%d", err, err, syn, len(evs))
	}
	// Malformed from the start with explicit json: plain error.
	_, _, err = streamAll(t, "not json", StreamOptions{Format: config.FormatJSON})
	if err == nil || errors.As(err, &syn) {
		t.Errorf("explicit json on text: %v", err)
	}
	// One record over the cap.
	huge := `[{"msg":"` + strings.Repeat("y", 3<<20) + `"}]`
	_, _, err = streamAll(t, huge, StreamOptions{Format: config.FormatJSON, MaxRecordBytes: 1 << 20})
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("record cap: %v", err)
	}
	if evs, _, err := streamAll(t, huge, StreamOptions{Format: config.FormatJSON, MaxRecordBytes: -1}); err != nil || len(evs) != 1 {
		t.Errorf("no cap: %v %d", err, len(evs))
	}
}

func TestStreamLongLines(t *testing.T) {
	long := strings.Repeat("z", 3<<20)
	evs, stats, err := streamAll(t, "first\n"+long+"\nlast\n", StreamOptions{Format: config.FormatText})
	if err != nil || len(evs) != 3 || stats.LongLines != 1 || stats.Lines != 3 {
		t.Fatalf("err %v events %d stats %+v", err, len(evs), stats)
	}
	if len(evs[1].Message) > MaxMessageBytes+4 || evs[2].Message != "last" || evs[2].Line != 3 {
		t.Errorf("long line handling: len=%d last=%q line=%d", len(evs[1].Message), evs[2].Message, evs[2].Line)
	}
	// A valid 70KB NDJSON record decodes (lines up to 1MB keep their JSON).
	rec := `{"level":"warn","msg":"` + strings.Repeat("m", 70<<10) + `"}`
	evs, _, _ = streamAll(t, rec+"\n"+rec+"\n", StreamOptions{Format: config.FormatNDJSON})
	if len(evs) != 2 || evs[0].Level != "warn" {
		t.Errorf("70KB NDJSON: %d events, level %q", len(evs), evs[0].Level)
	}
}

func TestStreamCSV(t *testing.T) {
	csv := "\"@timestamp\",\"@message\",\"level\"\n\"2026-10-11 10:00:00.000\",\"multi\nline message\",\"ERROR\"\n\"2026-10-11 10:00:01.000\",\"second\",\"INFO\"\n"
	evs, stats, err := streamAll(t, csv, StreamOptions{FileName: "export.CSV"})
	if err != nil || stats.Mode != "csv" || len(evs) != 2 {
		t.Fatalf("%v %s %q", err, stats.Mode, messages(evs))
	}
	if evs[0].Message != "multi\nline message" || evs[0].Level != "ERROR" || evs[0].Line != 2 || evs[1].Line != 4 {
		t.Errorf("csv records: %+v / line %d", evs[0], evs[1].Line)
	}
	if !evs[1].Timestamp.Equal(time.Date(2026, 10, 11, 10, 0, 1, 0, time.UTC)) {
		t.Errorf("csv timestamp %v", evs[1].Timestamp)
	}
}

func utf16Bytes(s string, bigEndian, bom bool) []byte {
	units := utf16.Encode([]rune(s))
	var b bytes.Buffer
	if bom {
		units = append([]uint16{0xFEFF}, units...)
	}
	for _, u := range units {
		if bigEndian {
			b.WriteByte(byte(u >> 8))
			b.WriteByte(byte(u))
		} else {
			b.WriteByte(byte(u))
			b.WriteByte(byte(u >> 8))
		}
	}
	return b.Bytes()
}

func TestStreamEncodings(t *testing.T) {
	text := "2026-10-11T10:00:00Z ERROR café 𝄞 failed\r\n2026-10-11T10:00:01Z INFO done\r\n"
	src := &config.Source{Parse: config.Parse{Format: config.FormatAuto}}
	for _, c := range []struct {
		name     string
		data     []byte
		encoding string
	}{
		{"utf-8 bom", append([]byte("\xef\xbb\xbf"), text...), "utf-8-bom"},
		{"utf-16le bom", utf16Bytes(text, false, true), "utf-16le"},
		{"utf-16be bom", utf16Bytes(text, true, true), "utf-16be"},
		{"utf-16le no bom", utf16Bytes(text, false, false), "utf-16le"},
		{"utf-16be no bom", utf16Bytes(text, true, false), "utf-16be"},
		{"nul padding", append(make([]byte, 4096), text...), "utf-8"},
	} {
		var evs []model.Event
		stats, err := Stream(bytes.NewReader(c.data), src, streamNow, StreamOptions{}, func(ev model.Event) error {
			evs = append(evs, ev)
			return nil
		})
		if err != nil || stats.Encoding != c.encoding || len(evs) != 2 {
			t.Errorf("%s: err=%v encoding=%s events=%q", c.name, err, stats.Encoding, messages(evs))
			continue
		}
		if evs[0].Message != "2026-10-11T10:00:00Z ERROR café 𝄞 failed" || evs[0].Timestamp.IsZero() {
			t.Errorf("%s: message %q ts %v", c.name, evs[0].Message, evs[0].Timestamp)
		}
	}
}

func TestStreamRejectsArchivesAndBinary(t *testing.T) {
	src := &config.Source{Parse: config.Parse{Format: config.FormatAuto}}
	tarHeader := make([]byte, 600)
	copy(tarHeader, "app.log")
	copy(tarHeader[257:], "ustar")
	binary := bytes.Repeat([]byte{0x7f, 'E', 'L', 'F', 0, 1, 2, 0, 0, 0}, 100)
	for name, data := range map[string][]byte{
		"zip":    []byte("PK\x03\x04rest"),
		"xz":     []byte("\xfd7zXZ\x00rest"),
		"zstd":   []byte("\x28\xb5\x2f\xfdrest"),
		"tar":    tarHeader,
		"gz-gz":  []byte("\x1f\x8b\x08\x00"),
		"binary": binary,
	} {
		_, err := Stream(bytes.NewReader(data), src, streamNow, StreamOptions{}, func(model.Event) error { return nil })
		var ue *UnsupportedError
		if !errors.As(err, &ue) {
			t.Errorf("%s: want UnsupportedError, got %v", name, err)
		}
	}
}

func TestStreamEmitErrorStops(t *testing.T) {
	src := &config.Source{Parse: config.Parse{Format: config.FormatAuto}}
	stop := errors.New("stop")
	n := 0
	_, err := Stream(strings.NewReader("a\nb\nc\n"), src, streamNow, StreamOptions{}, func(model.Event) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Errorf("err %v after %d events", err, n)
	}
}

func TestHTTPParseUnchangedForTimestampedTraces(t *testing.T) {
	// HTTP ingest keeps indent folding (no anchoring) and ISO-only times.
	body := "2026-10-11 10:00:05,120 ERROR Request failed\njava.lang.IllegalStateException: boom\n\tat x(X.java:1)\nOct 11 10:15:02 host x\n"
	evs, err := Parse([]byte(body), "text/plain", &config.Source{ID: "s", Name: "s", Parse: config.Parse{Format: config.FormatText}}, streamNow)
	if err != nil || len(evs) != 3 {
		t.Fatalf("%v %q", err, messages(evs))
	}
	if evs[1].Message != "java.lang.IllegalStateException: boom\n\tat x(X.java:1)" || !evs[1].Timestamp.Equal(streamNow) {
		t.Errorf("HTTP folding changed: %+v", evs[1])
	}
	if !evs[2].Timestamp.Equal(streamNow) {
		t.Errorf("HTTP ingest should not parse syslog times: %v", evs[2].Timestamp)
	}
	if evs[0].Line != 0 || evs[0].TimeSource != "" {
		t.Errorf("HTTP events must not carry file fields: %+v", evs[0])
	}
}

// errAfter returns data, then err.
type errAfter struct {
	data []byte
	err  error
}

func (e *errAfter) Read(p []byte) (int, error) {
	if len(e.data) == 0 {
		return 0, e.err
	}
	n := copy(p, e.data)
	e.data = e.data[n:]
	return n, nil
}

func TestStreamJSONWrapperPriority(t *testing.T) {
	// Splunk results export: an empty "messages" array comes first, but
	// "results" ranks higher, as in HTTP ingest.
	splunk := `{"preview":false,"init_offset":0,"messages":[],"fields":[{"name":"message"}],"results":[{"message":"disk full"},{"message":"oom"}]}`
	evs, _, err := streamAll(t, splunk, StreamOptions{})
	if err != nil || strings.Join(messages(evs), ",") != "disk full,oom" {
		t.Errorf("splunk export: %v %q", err, messages(evs))
	}
	warned := `{"messages":[{"type":"WARN","text":"search truncated"}],"results":[{"message":"a"}]}`
	evs, _, _ = streamAll(t, warned, StreamOptions{})
	if strings.Join(messages(evs), ",") != "a" {
		t.Errorf("lower-ranked array chosen: %q", messages(evs))
	}
	// HTTP ingest picks the same arrays.
	for body, want := range map[string]string{splunk: "disk full,oom", warned: "a"} {
		src := &config.Source{Parse: config.Parse{Format: config.FormatJSON}}
		hevs, err := Parse([]byte(body), "", src, streamNow)
		if err != nil || strings.Join(messages(hevs), ",") != want {
			t.Errorf("HTTP parse: %v %q, want %q", err, messages(hevs), want)
		}
	}

	// A large records array is streamed even if a preferred key follows.
	var b strings.Builder
	b.WriteString(`{"messages":[`)
	for i := 0; i < 5000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"message":"row ` + strings.Repeat("x", 80) + `"}`)
	}
	b.WriteString(`],"records":[{"message":"late"}]}`)
	evs, _, err = streamAll(t, b.String(), StreamOptions{MaxRecordBytes: 64 << 10})
	if err != nil || len(evs) != 5000 {
		t.Errorf("large array: %v %d", err, len(evs))
	}
}

func TestStreamJSONRecordCapCoversWholeObject(t *testing.T) {
	// An object keyed by id is one record: its size is capped as a whole,
	// not per key.
	var b strings.Builder
	b.WriteString(`{`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"evt-` + strings.Repeat("9", i+1) + `":{"msg":"` + strings.Repeat("y", 50<<10) + `"}`)
	}
	b.WriteString(`}`)
	_, _, err := streamAll(t, b.String(), StreamOptions{Format: config.FormatJSON, MaxRecordBytes: 1 << 20})
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("keyed object over the cap: %v", err)
	}
	// "hits" holding an array (not an object with "hits") is envelope, capped too.
	arr := `{"hits":[` + strings.TrimSuffix(strings.Repeat(`{"msg":"`+strings.Repeat("z", 50<<10)+`"},`, 40), ",") + `]}`
	_, _, err = streamAll(t, arr, StreamOptions{Format: config.FormatJSON, MaxRecordBytes: 1 << 20})
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("non-records array over the cap: %v", err)
	}
	// Streamed records don't count toward the envelope.
	big := `{"meta":{"n":1},"records":[` + strings.TrimSuffix(strings.Repeat(`{"msg":"`+strings.Repeat("z", 50<<10)+`"},`, 40), ",") + `],"tail":"x"}`
	evs, _, err := streamAll(t, big, StreamOptions{Format: config.FormatJSON, MaxRecordBytes: 1 << 20})
	if err != nil || len(evs) != 40 {
		t.Errorf("streamed records: %v %d", err, len(evs))
	}
}

func TestStreamJSONRecordsPathLikeHTTP(t *testing.T) {
	cases := []struct {
		path, body string
		want       []string
	}{
		{"data.result.0.values", `{"data":{"result":[{"values":[{"msg":"a"},{"msg":"b"}]},{"values":[{"msg":"c"}]}]}}`, []string{"a", "b"}},
		{"data", `{"data":{"msg":"single"}}`, []string{"single"}},
		{"data", `{"data":null}`, nil},
		{"a.b", `{"a.b":[{"msg":"dotted"}]}`, []string{"dotted"}},
	}
	for _, c := range cases {
		src := &config.Source{Parse: config.Parse{Format: config.FormatJSON, RecordsPath: c.path}}
		var got []string
		_, err := Stream(strings.NewReader(c.body), src, streamNow, StreamOptions{}, func(ev model.Event) error {
			got = append(got, ev.Message)
			return nil
		})
		if err != nil || strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v %q", c.path, err, got)
		}
		hevs, err := Parse([]byte(c.body), "", src, streamNow)
		if err != nil || strings.Join(messages(hevs), ",") != strings.Join(c.want, ",") {
			t.Errorf("%s over HTTP: %v %q", c.path, err, messages(hevs))
		}
	}
}

func TestStreamJSONArrayLineNumbersAcrossBuffers(t *testing.T) {
	// Element starts land on every position relative to the decoder's
	// buffer refills, including right after a comma.
	for _, shape := range []string{`{"msg":"m%d"}`, `  {"msg": "m%d"}`, "  {\n    \"msg\": \"m%d\"\n  }"} {
		var b strings.Builder
		b.WriteString("[\n")
		var want []int
		line := 2
		for i := 0; i < 20000; i++ {
			if i > 0 {
				b.WriteString(",\n")
			}
			el := fmt.Sprintf(shape, i)
			want = append(want, line)
			line += strings.Count(el, "\n") + 1
			b.WriteString(el)
		}
		b.WriteString("\n]\n")
		evs, stats, err := streamAll(t, b.String(), StreamOptions{})
		if err != nil || len(evs) != len(want) || stats.Lines != line {
			t.Fatalf("%v events=%d lines=%d, want %d", err, len(evs), stats.Lines, line)
		}
		for i, ev := range evs {
			if ev.Line != want[i] {
				t.Errorf("%q: record %d at line %d, want %d", shape, i, ev.Line, want[i])
				break
			}
		}
	}
}

func TestStreamAutoDetectTextThatLooksLikeJSON(t *testing.T) {
	dmesg := "[    0.000000] Linux version 5.15.0 (gcc)\n[    0.000000] Command line: ro quiet\n[    1.234567] usb 1-1: new device\n"
	evs, stats, err := streamAll(t, dmesg, StreamOptions{})
	if err != nil || stats.Mode == "json" || len(evs) != 3 {
		t.Errorf("dmesg: %v mode=%s %q", err, stats.Mode, messages(evs))
	}
	pm2 := "[0] server listening\n[1] worker started\n"
	if evs, stats, err := streamAll(t, pm2, StreamOptions{}); err != nil || stats.Mode == "json" || len(evs) != 2 {
		t.Errorf("pm2: %v mode=%s %q", err, stats.Mode, messages(evs))
	}
	banner := "{\n  \"version\": 1\n}\n2026-10-11 10:15:02 INFO started\n2026-10-11 10:15:03 ERROR failed\n"
	evs, stats, err = streamAll(t, banner, StreamOptions{})
	if err != nil || stats.Mode == "json" || len(evs) < 2 || !strings.Contains(evs[len(evs)-1].Message, "ERROR failed") {
		t.Errorf("JSON banner then text: %v mode=%s %q", err, stats.Mode, messages(evs))
	}
	// Real JSON still detected.
	for _, doc := range []string{`[{"msg":"a"},{"msg":"b"}]`, "{\n \"msg\": \"a\"\n}\n{\n \"msg\": \"b\"\n}\n", `[]`} {
		if _, stats, err := streamAll(t, doc, StreamOptions{}); err != nil || stats.Mode != "json" {
			t.Errorf("%q: %v mode=%s", doc, err, stats.Mode)
		}
	}
}

func TestStreamCSVBoundedRecords(t *testing.T) {
	var b strings.Builder
	b.WriteString("time,level,message\n2026-10-11T10:00:00Z,ERROR,\"quoted\" then text\n")
	for i := 0; i < 3000; i++ {
		b.WriteString("2026-10-11T10:00:01Z,INFO,row\n")
	}
	evs, _, err := streamAll(t, b.String(), StreamOptions{FileName: "x.csv", MaxRecordBytes: -1})
	var syn *SyntaxError
	if !errors.As(err, &syn) || syn.Line != 2 || len(evs) != 0 {
		t.Errorf("unbalanced quote: %v %d", err, len(evs))
	}
	_, _, err = streamAll(t, b.String(), StreamOptions{FileName: "x.csv", MaxRecordBytes: 16 << 10})
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("csv record cap: %v", err)
	}
	// Gzipped CSV keeps its type (the importer passes the original name).
	if _, stats, _ := streamAll(t, "a,message\n1,x\n", StreamOptions{FileName: "export.csv.gz"}); stats.Mode != "csv" {
		t.Errorf(".csv.gz mode %s", stats.Mode)
	}
}

func TestStreamMoreTimestampStyles(t *testing.T) {
	cases := map[string]time.Time{
		"2026-10-11 10:15:02 +0200 ERROR boom":     time.Date(2026, 10, 11, 8, 15, 2, 0, time.UTC),
		"2026-10-11 10:15:02.123+0200 ERROR boom":  time.Date(2026, 10, 11, 8, 15, 2, 123e6, time.UTC),
		"2026-10-11 10:15:02 -05:00 ERROR boom":    time.Date(2026, 10, 11, 15, 15, 2, 0, time.UTC),
		"2026-10-11T10:15:02.5+05:30 ERROR boom":   time.Date(2026, 10, 11, 4, 45, 2, 5e8, time.UTC),
		"[2026-10-11 10:15:02 +0000] ERROR boom":   time.Date(2026, 10, 11, 10, 15, 2, 0, time.UTC),
		"2026-10-11 10:15:02 UTC ERROR boom":       time.Date(2026, 10, 11, 10, 15, 2, 0, time.UTC),
		"2026-10-11 10:15:02,250 +0100 ERROR boom": time.Date(2026, 10, 11, 9, 15, 2, 25e7, time.UTC),
	}
	for line, want := range cases {
		ts, ok := timeParser{extended: true}.textTime(line)
		if !ok || !ts.Equal(want) {
			t.Errorf("%q: %v %v, want %v", line, ok, ts.UTC(), want)
		}
	}
	csv := "\"@timestamp\",message\n\"Oct 11, 2026 @ 10:15:02.123\",x\n"
	evs, _, err := streamAll(t, csv, StreamOptions{FileName: "kibana.csv"})
	if err != nil || len(evs) != 1 || !evs[0].Timestamp.Equal(time.Date(2026, 10, 11, 10, 15, 2, 123e6, time.UTC)) {
		t.Errorf("kibana timestamp: %v %+v", err, evs)
	}
}

func TestStreamKeepsPendingRecordOnReadError(t *testing.T) {
	stop := errors.New("upload limit")
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("2026-10-11 10:00:00 INFO filler\n")
	}
	b.WriteString("2026-10-11 10:00:01 ERROR c\njava.lang.X: boom\n\tat a.b(C.java:1)\n")
	src := &config.Source{Parse: config.Parse{Format: config.FormatAuto}}
	var evs []model.Event
	_, err := Stream(&errAfter{data: []byte(b.String()), err: stop}, src, streamNow, StreamOptions{}, func(ev model.Event) error {
		evs = append(evs, ev)
		return nil
	})
	if !errors.Is(err, stop) || len(evs) != 41 || !strings.Contains(evs[40].Message, "java.lang.X") {
		t.Errorf("pending record lost: %v %d", err, len(evs))
	}
	// Under 512 bytes, then an error: the bytes that arrived are read.
	evs = nil
	_, err = Stream(&errAfter{data: []byte("2026-10-11 10:00:01 ERROR tiny\n"), err: stop}, src, streamNow, StreamOptions{}, func(ev model.Event) error {
		evs = append(evs, ev)
		return nil
	})
	if !errors.Is(err, stop) || len(evs) != 1 {
		t.Errorf("short input: %v %d", err, len(evs))
	}
}

func TestStreamNULPaddingOverWindow(t *testing.T) {
	for _, pad := range []int{64, 65536, 65536 + 10, 3*65536 + 63} {
		in := strings.Repeat("\x00", pad) + "2026-10-11 10:00:00 INFO a\n2026-10-11 10:00:01 INFO b\n"
		evs, _, err := streamAll(t, in, StreamOptions{})
		if err != nil || len(evs) != 2 || !strings.HasPrefix(evs[0].Message, "2026") || evs[0].TimeSource != "" {
			t.Errorf("pad %d: %v %q", pad, err, messages(evs))
		}
	}
}
