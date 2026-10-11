// Package ingest turns raw log payloads (JSON, NDJSON or plain text) into
// events, and serves the push endpoint senders POST logs to.
package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

// Limits applied to every payload.
const (
	MaxEventsPerPayload = 10000
	MaxMessageBytes     = 32 * 1024
)

var (
	messageKeys   = []string{"message", "msg", "log", "MESSAGE", "text", "short_message", "textPayload", "body", "line", "event"}
	levelKeys     = []string{"level", "severity", "lvl", "log.level", "levelname", "loglevel", "log_level", "PRIORITY", "priority"}
	timestampKeys = []string{"@timestamp", "timestamp", "time", "ts", "date", "datetime", "_time", "eventTime", "__REALTIME_TIMESTAMP"}
	wrapperKeys   = []string{"records", "logs", "events", "entries", "items", "data", "results", "messages", "hits.hits"}
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
		ev := toEvent(r, p, now)
		if ev.Message == "" {
			continue
		}
		ev.ID = model.NewID()
		ev.SourceID = src.ID
		ev.SourceName = src.Name
		ev.ReceivedAt = now
		for _, t := range src.Tags {
			ev.AddTag(t)
		}
		events = append(events, ev)
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
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if line[0] == '{' || line[0] == '[' {
			if v, err := decode(line); err == nil {
				out = append(out, v)
				continue
			}
		}
		out = append(out, string(line))
	}
	return out
}

var continuation = regexp.MustCompile(`^(\s+\S|at\s|Caused by:|\.\.\. \d+ more)`)

// parseText splits plain text into one record per line, folding indented
// continuation lines (stack traces) into the preceding record.
func parseText(body []byte) []any {
	var out []any
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(out) > 0 && continuation.MatchString(line) {
			prev := out[len(out)-1].(string)
			if len(prev) < MaxMessageBytes {
				out[len(out)-1] = prev + "\n" + line
			}
			continue
		}
		out = append(out, line)
	}
	return out
}

var leadingTimestamp = regexp.MustCompile(`^\[?(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?)\]?\s`)

func toEvent(rec any, p config.Parse, now time.Time) model.Event {
	ev := model.Event{Timestamp: now}
	switch r := rec.(type) {
	case string:
		ev.Message = r
		if m := leadingTimestamp.FindStringSubmatch(r); m != nil {
			if ts, ok := ParseTime(strings.Replace(m[1], ",", ".", 1)); ok {
				ev.Timestamp = ts
			}
		}
	case map[string]any:
		if src, ok := r["_source"].(map[string]any); ok { // Elasticsearch / OpenSearch hits
			r = src
		}
		fields := make(map[string]any, len(r))
		for k, v := range r {
			fields[k] = v
		}
		if key, v, ok := pick(r, p.MessageField, messageKeys); ok {
			ev.Message = model.ToString(v)
			delete(fields, key)
		} else {
			b, _ := json.Marshal(r)
			ev.Message = string(b)
		}
		if _, v, ok := pick(r, p.LevelField, levelKeys); ok {
			ev.Level = model.ToString(v)
		}
		if _, v, ok := pick(r, p.TimestampField, timestampKeys); ok {
			if ts, ok := toTime(v); ok {
				ev.Timestamp = ts
			}
		}
		ev.Fields = fields
	default:
		ev.Message = model.ToString(r)
	}
	ev.Message = model.Truncate(strings.TrimSpace(ev.Message), MaxMessageBytes)
	return ev
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
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"02/Jan/2006:15:04:05 -0700",
	time.RFC1123Z,
	time.RFC1123,
	time.UnixDate,
}

// ParseTime parses common timestamp formats, including epoch numbers in
// seconds, milliseconds, microseconds or nanoseconds.
func ParseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return epoch(f)
	}
	for _, l := range timeLayouts {
		if ts, err := time.Parse(l, s); err == nil {
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
