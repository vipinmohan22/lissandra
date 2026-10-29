// Package ingest turns raw log payloads (JSON, NDJSON or plain text) into
// events, and serves the push endpoint senders POST logs to.
package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// Limits applied to every payload.
const (
	MaxEventsPerPayload = 10000
	MaxMessageBytes     = 32 * 1024
)

var (
	messageKeys   = []string{"message", "msg", "log", "MESSAGE", "@message", "text", "short_message", "textPayload", "body", "line", "event"}
	levelKeys     = []string{"level", "severity", "lvl", "log.level", "levelname", "loglevel", "log_level", "@level", "@l", "severityText", "SeverityText", "PRIORITY", "priority"}
	timestampKeys = []string{"@timestamp", "timestamp", "time", "ts", "date", "datetime", "_time", "eventTime", "__REALTIME_TIMESTAMP"}
	wrapperKeys   = []string{"records", "Records", "logs", "events", "entries", "items", "data", "results", "messages", "hits.hits"}
)

// Parse converts body into events for src. contentType is the request's
// Content-Type header, used only by the "auto" format.
func Parse(body []byte, contentType string, src *config.Source, now time.Time) ([]model.Event, error) {
	p := src.Parse
	var recs []any
	var err error
	switch p.Format {
	case config.FormatJSON:
		recs, err = parseJSON(body, p.RecordsPath)
	case config.FormatNDJSON:
		recs = parseNDJSON(body)
	case config.FormatText:
		recs = parseText(body)
	default:
		recs, err = parseAuto(body, contentType, p.RecordsPath)
	}
	if err != nil {
		return nil, err
	}
	if len(recs) > MaxEventsPerPayload {
		recs = recs[:MaxEventsPerPayload]
	}
	events := make([]model.Event, 0, len(recs))
	for _, r := range recs {
		if ev, ok := finalize(r, src, now); ok {
			events = append(events, ev)
		}
	}
	return events, nil
}

func parseAuto(body []byte, contentType, recordsPath string) ([]any, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "ndjson") || strings.Contains(ct, "jsonlines") || strings.Contains(ct, "json-seq") {
		return parseNDJSON(trimmed), nil
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if recs, err := parseJSON(trimmed, recordsPath); err == nil {
			return recs, nil
		}
		if trimmed[0] == '{' {
			return parseNDJSON(trimmed), nil
		}
	}
	return parseText(trimmed), nil
}

func decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after JSON document")
	}
	return v, nil
}

func parseJSON(body []byte, recordsPath string) ([]any, error) {
	doc, err := decode(body)
	if err != nil {
		return nil, err
	}
	return Records(doc, recordsPath)
}

// Records extracts the list of log records from a decoded JSON document,
// using recordsPath when given and common wrapper keys otherwise.
func Records(doc any, recordsPath string) ([]any, error) {
	if recordsPath != "" {
		v, ok := model.Lookup(doc, recordsPath)
		if !ok {
			return nil, errors.New("records path " + strconv.Quote(recordsPath) + " not found in response")
		}
		doc = v
	} else if obj, ok := doc.(map[string]any); ok {
		for _, k := range wrapperKeys {
			if v, ok := model.Lookup(obj, k); ok {
				if arr, ok := v.([]any); ok {
					doc = arr
					break
				}
			}
		}
	}
	switch t := doc.(type) {
	case []any:
		return t, nil
	case nil:
		return nil, nil
	default:
		return []any{t}, nil
	}
}

func parseNDJSON(body []byte) []any {
	var out []any
	sp := lineSplitter{decode: true}
	collect := func(rec any, _ int) { out = append(out, rec) }
	for _, line := range strings.Split(string(body), "\n") {
		sp.push(line, collect)
	}
	sp.flush(collect)
	return out
}

// parseText splits plain text into one record per line, folding indented
// continuation lines (stack traces) into the preceding record.
func parseText(body []byte) []any {
	var out []any
	sp := lineSplitter{fold: true}
	collect := func(rec any, _ int) { out = append(out, rec) }
	for _, line := range strings.Split(string(body), "\n") {
		sp.push(line, collect)
	}
	sp.flush(collect)
	return out
}

// toEvent turns a record into an event. hasTime reports whether the
// record carried a timestamp; otherwise the event's Timestamp is now.
func toEvent(rec any, p config.Parse, now time.Time, tp timeParser) (ev model.Event, hasTime bool) {
	ev = model.Event{Timestamp: now}
	switch r := rec.(type) {
	case string:
		ev.Message = r
		if ts, ok := tp.textTime(r); ok {
			ev.Timestamp, hasTime = ts, true
		}
	case map[string]any:
		if src, ok := r["_source"].(map[string]any); ok { // Elasticsearch / OpenSearch hits
			r = src
		}
		fields := make(map[string]any, len(r))
		for k, v := range r {
			fields[k] = v
		}
		key, v, hasMessage := pick(r, p.MessageField, messageKeys)
		if hasMessage {
			ev.Message = model.ToString(v)
			delete(fields, key)
		} else {
			b, _ := json.Marshal(r)
			ev.Message = string(b)
		}
		if key, v, ok := pick(r, p.LevelField, levelKeys); ok {
			ev.Level = model.ToString(v)
			// A number is a level only on a scale the record's shape names:
			// a sensor's or a game's "level": 60 is not pino's fatal.
			if n, err := strconv.Atoi(strings.TrimSpace(ev.Level)); err == nil && p.LevelField == "" && !numericLevelOK(key, n, r, hasMessage) {
				ev.Level = ""
			}
		} else if lv, ok := mongoLevel(r); ok && p.LevelField == "" {
			ev.Level = lv
		}
		if _, v, ok := pick(r, p.TimestampField, timestampKeys); ok {
			if ts, ok := tp.valueTime(v); ok {
				ev.Timestamp, hasTime = ts, true
			}
		}
		ev.Fields = fields
	default:
		ev.Message = model.ToString(r)
	}
	ev.Message = model.Truncate(strings.TrimSpace(ev.Message), MaxMessageBytes)
	return ev, hasTime
}

// numericLevelOK reports whether n, the value of the level key key in r,
// is a log level: syslog's 0-7 (journald's PRIORITY, GELF's level) in a
// record with a message, pino's and bunyan's 10-60 and Monolog's 100-600
// only in a record shaped as theirs. A sensor's or a game's "level": 60 is
// not pino's fatal; a record without a message is data, and so is one
// with a reading's fields. pino's base, time and message key are often
// configured away (base: null, isoTime, messageKey "message").
func numericLevelOK(key string, n int, r map[string]any, hasMessage bool) bool {
	has := func(k string) bool { _, ok := r[k]; return ok }
	switch {
	case key == "PRIORITY" || key == "priority":
		return n >= 0 && n <= 7
	case key != "level" && key != "severity" && key != "lvl":
		return false
	case n >= 0 && n <= 7:
		return hasMessage
	case has("channel") && (has("datetime") || has("level_name")): // Monolog
		switch n {
		case 100, 200, 250, 300, 400, 500, 550, 600:
			return true
		}
		return false
	}
	if n%10 != 0 || n < 10 || n > 60 { // pino, bunyan
		return false
	}
	return has("pid") && has("hostname") || has("v") && has("name") ||
		(has("msg") || has("message")) && !has("unit") && !has("value") && !has("reading") && !has("sensor")
}

// mongoLevel reads the severity of MongoDB 4.4+'s structured log
// ({"t":{"$date":…},"s":"W","c":"CONTROL",…}): F, E, W, I or D1-D5.
func mongoLevel(r map[string]any) (string, bool) {
	t, ok := r["t"].(map[string]any)
	if !ok {
		return "", false
	}
	if _, ok := t["$date"]; !ok {
		return "", false
	}
	s, ok := r["s"].(string)
	if !ok || len(s) == 0 || len(s) > 2 || !strings.ContainsRune("FEWID", rune(s[0])) || len(s) == 2 && (s[0] != 'D' || s[1] < '1' || s[1] > '5') {
		return "", false
	}
	return s, true
}

// pick finds a field by explicit path, or by the first matching candidate.
func pick(rec map[string]any, explicit string, candidates []string) (string, any, bool) {
	if explicit != "" {
		v, ok := model.Lookup(rec, explicit)
		return explicit, v, ok && v != nil
	}
	for _, k := range candidates {
		if v, ok := model.Lookup(rec, k); ok && v != nil {
			if s, isStr := v.(string); isStr && s == "" {
				continue
			}
			return k, v, true
		}
	}
	return "", nil, false
}

func toTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		return ParseTime(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return epoch(f)
	case float64:
		return epoch(t)
	}
	return time.Time{}, false
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z0700",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"02/Jan/2006:15:04:05 -0700",
	time.RFC1123Z,
	time.RFC1123,
	time.UnixDate,
	"Jan 2, 2006 @ 15:04:05.999999999", // Kibana CSV exports
}

// ParseTime parses common timestamp formats, including epoch numbers in
// seconds, milliseconds, microseconds or nanoseconds. Times without a zone
// are taken as UTC.
func ParseTime(s string) (time.Time, bool) {
	return parseTimeIn(s, time.UTC)
}

// parseTimeIn is ParseTime with times that lack a zone taken in loc.
func parseTimeIn(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return epoch(f)
	}
	for _, l := range timeLayouts {
		if ts, err := time.ParseInLocation(l, s, loc); err == nil {
			return sane(ts)
		}
	}
	return time.Time{}, false
}

func epoch(f float64) (time.Time, bool) {
	var ts time.Time
	switch {
	case f > 1e17:
		ts = time.Unix(0, int64(f))
	case f > 1e14:
		ts = time.UnixMicro(int64(f))
	case f > 1e11:
		ts = time.UnixMilli(int64(f))
	default:
		sec := int64(f)
		ts = time.Unix(sec, int64((f-float64(sec))*1e9))
	}
	return sane(ts)
}

func sane(ts time.Time) (time.Time, bool) {
	if ts.Year() < 2000 || ts.Year() > 2200 {
		return time.Time{}, false
	}
	return ts.UTC(), true
}
