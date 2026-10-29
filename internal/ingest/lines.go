package ingest

import (
	"bytes"
	"regexp"
	"strings"
)

var continuation = regexp.MustCompile(`^(\s+\S|at\s|Caused by:|\.\.\. \d+ more)`)

// Records whose next lines are not indented, which fold joins all the same:
var (
	// java.util.logging's SimpleFormatter: a header line, then the level and
	// the message ("Oct 11, 2026 10:00:00 AM org.example.Foo run" + "SEVERE: …").
	julHeader = regexp.MustCompile(`^[A-Z][a-z]{2} \d{1,2}, \d{4} \d{1,2}:\d\d:\d\d [AP]M \S+(?: \S+)?$`)
	julLevel  = regexp.MustCompile(`^(?:SEVERE|WARNING|INFO|CONFIG|FINE|FINER|FINEST): `)
	// A Go panic or runtime fatal error and its goroutine dump.
	goPanic = regexp.MustCompile(`^(?:panic: |fatal error: )`)
	goTrace = regexp.MustCompile(`^(?:\[signal |goroutine \d+ \[|created by |exit status \d+$|\[recovered\]|panic: |\.\.\.additional frames elided\.\.\.$|[\w\-./]+\.(?:\(\*?[\w\[\]]+\)\.)?[\w\[\]·.-]+\(.*\)$)`)
	// A Python traceback after the record that logged it, its chained
	// tracebacks and its closing exception line; a Java exception header
	// after the record that logged it ("… with root cause" + "java.lang.…").
	pyTraceback = regexp.MustCompile(`^(?:Traceback \(most recent call last\):|During handling of the above exception, another exception occurred:|The above exception was the direct cause of the following exception:)$`)
	pyException = regexp.MustCompile(`^(?:[A-Za-z_][\w]*\.)*[A-Z]\w*(?:Error|Exception|Warning|Exit|Interrupt|Iteration)(?::|$)`)
	javaExc     = regexp.MustCompile(`^(?:[a-z_$][\w$]*\.)+[A-Z][\w$]*(?:Exception|Error|Throwable)(?::|$)`)
	loggedIssue = regexp.MustCompile(`\b(?:ERROR|CRITICAL|FATAL|SEVERE|WARN|WARNING|EXCEPTION)\b|\bwith root cause$`)

	pyTracebackHead = []byte("Traceback (most recent call last):")
)

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
			cont = continuation.MatchString(line) || s.joins(line)
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

// joins reports whether line, not indented, continues the pending record
// all the same (see julHeader and the patterns after it).
func (s *lineSplitter) joins(line string) bool {
	p := s.pending
	first, last := p, p
	if i := bytes.IndexByte(p, '\n'); i >= 0 {
		first, last = p[:i], p[bytes.LastIndexByte(p, '\n')+1:]
	}
	switch {
	case s.conts == 0 && julLevel.MatchString(line):
		return julHeader.Match(first)
	case goPanic.Match(first):
		return goTrace.MatchString(line)
	case pyTraceback.MatchString(line):
		if bytes.Contains(p, pyTracebackHead) {
			// A chain marker joins its traceback; a new traceback joins
			// only right after one, not after an unrelated traceback.
			return !strings.HasPrefix(line, "Traceback") || pyTraceback.Match(last)
		}
		return s.conts == 0 && loggedIssue.Match(first)
	case pyException.MatchString(line) && bytes.Contains(p, pyTracebackHead):
		return len(last) > 0 && (last[0] == ' ' || last[0] == '\t')
	case javaExc.MatchString(line):
		// after the record that logged it, or JUL's header and level line
		// (logger.log(SEVERE, msg, thrown))
		return s.conts == 0 && loggedIssue.Match(first) || s.conts == 1 && julHeader.Match(first) && julLevel.Match(last)
	}
	return false
}

// flush emits the pending text record, if any.
func (s *lineSplitter) flush(emit func(rec any, startLine int)) {
	if s.has {
		s.has = false
		emit(string(s.pending), s.start)
	}
}
