package ingest

import (
	"regexp"
	"strings"
	"time"
)

// timeParser interprets timestamps. The zero value matches HTTP ingest:
// only ISO timestamps at the start of text records are recognised, and
// zone-less times are UTC. File analysis uses an extended parser that also
// knows common text log formats, a configurable zone for zone-less times,
// and a reference time for formats without a year.
type timeParser struct {
	loc      *time.Location // zone for timestamps without one (nil = UTC)
	ref      time.Time      // "now" for year inference
	extended bool
}

func (tp timeParser) location() *time.Location {
	if tp.loc == nil {
		return time.UTC
	}
	return tp.loc
}

var (
	// ISO 8601 / RFC 3339, optionally bracketed or after an RFC 5424 "<pri>1 " prefix.
	isoPrefix = regexp.MustCompile(`^(?:<\d{1,3}>\d? ?)?\[?(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?)( ?(?:Z|[+-]\d{2}:?\d{2}|UTC))?\]?(?:\s|$)`)
	// 2026/10/11 10:15:02 (nginx error log, Go's log package).
	slashPrefix = regexp.MustCompile(`^\[?(\d{4}/\d{2}/\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?)\]?(?:\s|$)`)
	// Oct 11 10:15:02 (RFC 3164 syslog: /var/log/syslog, messages, auth.log).
	syslogPrefix = regexp.MustCompile(`^(?:<\d{1,3}>)?([A-Z][a-z]{2} [ \d]\d \d{2}:\d{2}:\d{2})\s`)
	// [Mon Oct 11 10:15:02.123456 2026] (Apache error log).
	ctimeBracket = regexp.MustCompile(`^\[(?:[A-Z][a-z]{2} )?([A-Z][a-z]{2} [ \d]?\d \d{2}:\d{2}:\d{2})(?:\.\d+)? (\d{4})\]`)
	// [11/Oct/2026:10:15:02 +0000] anywhere near the start (access logs).
	clfBracket = regexp.MustCompile(`\[(\d{2}/[A-Z][a-z]{2}/\d{4}:\d{2}:\d{2}:\d{2} [+-]\d{4})\]`)
	// E1011 10:15:02.123456 (klog).
	klogPrefix2 = regexp.MustCompile(`^[IWEF](\d{2})(\d{2}) (\d{2}:\d{2}:\d{2}(?:\.\d+)?) `)
)

// textTime finds the timestamp of a text log record.
func (tp timeParser) textTime(line string) (time.Time, bool) {
	if m := isoPrefix.FindStringSubmatch(line); m != nil {
		s := strings.Replace(m[1], ",", ".", 1)
		if zone := strings.TrimSpace(m[2]); zone != "" && zone != "UTC" {
			s += zone
		} else if zone == "UTC" {
			s += "Z"
		}
		if ts, ok := parseTimeIn(s, tp.location()); ok {
			return ts, true
		}
	}
	if !tp.extended {
		return time.Time{}, false
	}
	head := line
	if len(head) > 120 {
		head = head[:120]
	}
	loc := tp.location()
	if m := slashPrefix.FindStringSubmatch(head); m != nil {
		if ts, err := time.ParseInLocation("2006/01/02 15:04:05.999999999", strings.NewReplacer("T", " ", ",", ".").Replace(m[1]), loc); err == nil {
			return sane(ts)
		}
	}
	if m := syslogPrefix.FindStringSubmatch(head); m != nil {
		if ts, err := time.ParseInLocation("Jan _2 15:04:05", m[1], loc); err == nil {
			return tp.withYear(ts)
		}
	}
	if m := ctimeBracket.FindStringSubmatch(head); m != nil {
		if ts, err := time.ParseInLocation("Jan _2 15:04:05 2006", m[1]+" "+m[2], loc); err == nil {
			return sane(ts)
		}
	}
	if m := klogPrefix2.FindStringSubmatch(head); m != nil {
		if ts, err := time.ParseInLocation("0102 15:04:05.999999999", m[1]+m[2]+" "+m[3], loc); err == nil {
			return tp.withYear(ts)
		}
	}
	if m := clfBracket.FindStringSubmatch(head); m != nil {
		if ts, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[1]); err == nil {
			return sane(ts)
		}
	}
	return time.Time{}, false
}

// startsWithTime reports whether a line begins with a recognised timestamp,
// which marks the start of a record in timestamp-anchored grouping.
func (tp timeParser) startsWithTime(line string) bool {
	if line == "" {
		return false
	}
	switch c := line[0]; {
	case c >= '0' && c <= '9', c == '[', c == '<', c >= 'A' && c <= 'Z':
	default:
		return false // cheap reject: indented lines, lowercase text, …
	}
	_, ok := tp.textTime(line)
	return ok
}

// withYear fills in the year for formats that omit it, choosing the year
// that puts the time no more than a day after the reference time.
func (tp timeParser) withYear(ts time.Time) (time.Time, bool) {
	ref := tp.ref
	if ref.IsZero() {
		ref = time.Now()
	}
	ref = ref.In(tp.location())
	ts = time.Date(ref.Year(), ts.Month(), ts.Day(), ts.Hour(), ts.Minute(), ts.Second(), ts.Nanosecond(), tp.location())
	if ts.After(ref.Add(24 * time.Hour)) {
		ts = ts.AddDate(-1, 0, 0)
	}
	return sane(ts)
}

// valueTime parses a structured timestamp field.
func (tp timeParser) valueTime(v any) (time.Time, bool) {
	if s, ok := v.(string); ok {
		return parseTimeIn(s, tp.location())
	}
	return toTime(v)
}
