package ingest

import (
	"regexp"
	"strings"
)

var continuation = regexp.MustCompile(`^(\s+\S|at\s|Caused by:|\.\.\. \d+ more)`)

// lineSplitter turns lines into log records.
//
//   - fold: indented continuation lines (stack traces) join the preceding
//     text record.
//   - startsRecord: when set (file analysis of timestamped logs), a text
//     line begins a new record only if startsRecord says so; every other
//     line joins the current record. This keeps unindented exception
//     headers and Python tracebacks with the log line they belong to.
//   - decode: lines holding a JSON value become decoded records
//     (objectsOnly limits that to lines starting with '{').
//   - maxCont: continuation lines beyond this many per record are dropped
//     (0 means no limit).
type lineSplitter struct {
	fold, decode, objectsOnly bool
	startsRecord              func(line string) bool
	maxCont                   int

	line    int // number of the last line pushed (1-based)
	pending []byte
	start   int // line the pending record started on
	conts   int // continuation lines in the pending record
	has     bool

	folded, dropped, jsonErrors int // diagnostics
}

// push consumes one line (without its trailing newline) and emits any
// records it completes, with the line number each record started on.
func (s *lineSplitter) push(line string, emit func(rec any, startLine int)) {
	s.line++
	line = strings.TrimRight(line, "\r")
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return
	}
	if s.decode && (trimmed[0] == '{' || (trimmed[0] == '[' && !s.objectsOnly)) {
		if v, err := decode([]byte(trimmed)); err == nil {
			s.flush(emit)
			emit(v, s.line)
			return
		}
		if trimmed[0] == '{' {
			s.jsonErrors++
		}
	}
	if !s.fold && s.startsRecord == nil {
		emit(trimmed, s.line)
		return
	}
	cont := s.has
	if cont {
		if s.startsRecord != nil {
			cont = !s.startsRecord(line)
		} else {
			cont = continuation.MatchString(line)
		}
	}
	if cont {
		s.conts++
		if s.maxCont > 0 && s.conts > s.maxCont {
			s.dropped++
			return
		}
		s.folded++
		if len(s.pending) < MaxMessageBytes {
			s.pending = append(s.pending, '\n')
			s.pending = append(s.pending, line...)
		}
		return
	}
	s.flush(emit)
	s.pending = append(s.pending[:0], line...)
	s.start, s.conts, s.has = s.line, 0, true
}

// flush emits the pending text record, if any.
func (s *lineSplitter) flush(emit func(rec any, startLine int)) {
	if s.has {
		s.has = false
		emit(string(s.pending), s.start)
	}
}
