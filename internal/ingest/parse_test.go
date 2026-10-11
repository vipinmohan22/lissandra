package ingest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
)

func src(p config.Parse) *config.Source {
	return &config.Source{ID: "s1", Name: "svc", Type: config.SourcePush, Parse: p, Tags: []string{"prod"}}
}

var now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func TestParseJSONObject(t *testing.T) {
	evs, err := Parse([]byte(`{"level":"error","message":"boom","time":"2026-10-11T10:00:00Z","svc":"api"}`), "application/json", src(config.Parse{Format: config.FormatAuto}), now)
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %d events, err %v", len(evs), err)
	}
	ev := evs[0]
	if ev.Message != "boom" || ev.Level != "error" || ev.SourceName != "svc" || ev.ID == "" {
		t.Errorf("unexpected event %+v", ev)
	}
	if !ev.Timestamp.Equal(time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("timestamp = %v", ev.Timestamp)
	}
	if _, ok := ev.Fields["message"]; ok {
		t.Error("message key should be removed from fields")
	}
	if ev.Fields["svc"] != "api" || !ev.HasTag("prod") {
		t.Errorf("fields=%v tags=%v", ev.Fields, ev.Tags)
	}
}

func TestParseWrappersAndRecordsPath(t *testing.T) {
	body := []byte(`{"data":{"items":[{"text":"a","sev":"warn","ts":1791720000000},{"text":"b"}]}}`)
	evs, err := Parse(body, "", src(config.Parse{Format: config.FormatJSON, RecordsPath: "data.items", MessageField: "text", LevelField: "sev", TimestampField: "ts"}), now)
	if err != nil || len(evs) != 2 {
		t.Fatalf("got %d, err %v", len(evs), err)
	}
	if evs[0].Level != "warn" || evs[0].Timestamp.UnixMilli() != 1791720000000 {
		t.Errorf("event 0 = %+v", evs[0])
	}
	if evs[1].Timestamp != now {
		t.Errorf("missing timestamp should default to receive time, got %v", evs[1].Timestamp)
	}

	// Auto-detected wrapper key and Elasticsearch hits.
	evs, _ = Parse([]byte(`{"hits":{"hits":[{"_source":{"message":"es hit","@timestamp":"2026-10-11T09:00:00.123Z"}}]}}`), "", src(config.Parse{}), now)
	if len(evs) != 1 || evs[0].Message != "es hit" || evs[0].Timestamp.Hour() != 9 {
		t.Errorf("es hits: %+v", evs)
	}

	if _, err := Parse([]byte(`{"x":1}`), "", src(config.Parse{Format: config.FormatJSON, RecordsPath: "nope"}), now); err == nil {
		t.Error("missing records path should error")
	}
}

func TestParseNDJSONAndFallbacks(t *testing.T) {
	body := []byte("{\"msg\":\"one\",\"level\":30}\nnot json at all\n\n{\"log\":\"three\",\"date\":1791720000.5}\n")
	evs, err := Parse(body, "application/x-ndjson", src(config.Parse{}), now)
	if err != nil || len(evs) != 3 {
		t.Fatalf("got %d, err %v", len(evs), err)
	}
	if evs[0].Message != "one" || evs[0].Level != "30" {
		t.Errorf("ev0 %+v", evs[0])
	}
	if evs[1].Message != "not json at all" {
		t.Errorf("ev1 %+v", evs[1])
	}
	if evs[2].Message != "three" || evs[2].Timestamp.Unix() != 1791720000 || evs[2].Timestamp.Nanosecond() != 5e8 {
		t.Errorf("ev2 %+v", evs[2])
	}
}

func TestParseTextFoldsStackTraces(t *testing.T) {
	body := []byte("2026-10-11 08:00:00,123 ERROR boom\n\tat com.acme.A.b(A.java:1)\n    at com.acme.C.d(C.java:2)\nCaused by: java.io.IOException\nINFO next line\n")
	evs, err := Parse(body, "text/plain", src(config.Parse{}), now)
	if err != nil || len(evs) != 2 {
		t.Fatalf("got %d events, err %v: %+v", len(evs), err, evs)
	}
	if want := "2026-10-11 08:00:00,123 ERROR boom\n\tat com.acme.A.b(A.java:1)\n    at com.acme.C.d(C.java:2)\nCaused by: java.io.IOException"; evs[0].Message != want {
		t.Errorf("folded message = %q", evs[0].Message)
	}
	if evs[0].Timestamp.Hour() != 8 || evs[0].Timestamp.Nanosecond() != 123e6 {
		t.Errorf("leading timestamp not parsed: %v", evs[0].Timestamp)
	}
}

func TestParseObjectWithoutMessage(t *testing.T) {
	evs, _ := Parse([]byte(`{"a":1,"b":"x"}`), "", src(config.Parse{}), now)
	if len(evs) != 1 || evs[0].Message != `{"a":1,"b":"x"}` {
		t.Errorf("whole record should become the message: %+v", evs)
	}
}

func TestParseTime(t *testing.T) {
	cases := map[string]int64{
		"2026-10-11T10:00:00Z":       1791712800,
		"2026-10-11T10:00:00+02:00":  1791705600,
		"2026-10-11 10:00:00":        1791712800,
		"11/Oct/2026:10:00:00 +0000": 1791712800,
		"1791712800":                 1791712800,
		"1791712800000":              1791712800,
		"1791712800000000":           1791712800,
		"1791712800000000000":        1791712800,
	}
	for in, want := range cases {
		ts, ok := ParseTime(in)
		if !ok || ts.Unix() != want {
			t.Errorf("ParseTime(%q) = %v, %v; want unix %d", in, ts, ok, want)
		}
	}
	if _, ok := ParseTime("12"); ok {
		t.Error("tiny epoch should be rejected as implausible")
	}
	if _, ok := ParseTime("yesterday"); ok {
		t.Error("garbage should not parse")
	}
}

// hclog's JSON, Serilog's CLEF and OpenTelemetry name the level otherwise.
func TestMoreLevelKeys(t *testing.T) {
	for _, line := range []string{
		`{"@level":"warn","@message":"agent: slow","@timestamp":"2026-10-11T10:00:00Z"}`,
		`{"@t":"2026-10-11T10:00:00Z","@mt":"slow","@l":"Warning"}`,
		`{"severityText":"WARN","body":"slow","timeUnixNano":"1"}`,
	} {
		var rec any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if ev, _ := toEvent(rec, config.Parse{}, time.Now(), timeParser{}); !strings.EqualFold(ev.Level[:4], "warn") {
			t.Errorf("level from %s: %q", line, ev.Level)
		}
	}
}
