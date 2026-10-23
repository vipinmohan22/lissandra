package engine

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// stateBudget caps the text sent per event. The english checkpoint reads
// about 460 tokens of state; a full window costs about three times a short
// line, so the most telling lines go first and the rest is cut early.
const stateBudget = 1200

// severityLevels describes model.Severities, lowest first, for the score
// question.
var severityLevels = []string{
	"debug: diagnostic detail for developers",
	"info: normal operation",
	"notice: unusual but expected",
	"warning: degraded, or likely to cause problems if ignored",
	"error: an operation failed",
	"critical: outage, data loss, security breach or crash needing immediate attention",
}

// buildRequest builds the questions for ev: which category (always, when
// there are categories) and, when asked, how severe.
func buildRequest(ev *model.Event, cats []config.Category, checkpoint string, askSeverity bool) Request {
	q := map[string]Question{}
	if len(cats) > 0 {
		q[qCategory] = categoryQuestion(cats)
	}
	if askSeverity {
		q[qSeverity] = Question{Type: TypeScore, Instructions: "How severe is this log event?", Criteria: severityLevels}
	}
	return Request{State: buildState(ev), Questions: q, Model: checkpoint}
}

// categoryQuestion asks which category an event belongs to. Categories
// without a description are offered by name alone, which the engine tells
// apart better than long descriptions it can only read the start of.
func categoryQuestion(cats []config.Category) Question {
	crit := make(criteria, 0, len(cats)+1)
	for _, c := range cats {
		crit = append(crit, label{c.Name, c.Description})
	}
	crit = append(crit, label{model.Uncategorized, "none of the above"})
	return Question{Type: TypeChoice, Instructions: "Which category does this log event belong to?", Criteria: crit}
}

// label is one choice option; an empty desc is sent as null (name only).
type label struct{ name, desc string }

// criteria marshals as a JSON object in the configured order (Go maps
// would sort it), so the engine sees categories as the operator lists them.
type criteria []label

func (c criteria) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, l := range c {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(l.name)
		v := []byte("null")
		if l.desc != "" {
			v, _ = json.Marshal(l.desc)
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// skipField lists structured fields that tell the engine nothing about the
// category: the level (sent once above), times, and ids of requests, traces,
// sessions and users, which differ per event and would make every event
// look new to the answer cache.
var skipField = map[string]bool{
	"level": true, "severity": true, "lvl": true, "log.level": true, "levelname": true, "loglevel": true, "log_level": true,
	"@timestamp": true, "timestamp": true, "time": true, "ts": true, "date": true, "datetime": true, "_time": true,
	"eventtime": true, "__realtime_timestamp": true, "@version": true,
	"id": true, "uuid": true, "guid": true, "requestid": true, "traceid": true, "spanid": true, "sessionid": true,
	"correlationid": true, "userid": true, "uid": true, "pid": true, "tid": true, "thread": true, "goroutine": true,
	"trace.id": true, "span.id": true, "transaction.id": true, "dd.trace_id": true, "dd.span_id": true,
	"user": true, "username": true, "user.name": true, "email": true, "client_ip": true, "remote_addr": true, "ip": true,
}

// keepField lists id-like fields that name a kind of event rather than
// one event (Windows event ids, detection rules, CVEs, ATT&CK techniques):
// they say what happened, and their numbers are normalized in the cache key.
var keepField = map[string]bool{
	"eventid": true, "event_id": true, "winlog.event_id": true, "event.code": true, "rule.id": true, "rule_id": true, "ruleid": true,
	"signature_id": true, "signatureid": true, "sid": true, "vulnerability.id": true, "cve": true, "cve_id": true,
	"threat.technique.id": true, "technique_id": true, "error_id": true, "errorid": true, "msgid": true, "message_id": true,
}

// isSkipped reports whether a field is left out of the state: one of
// skipField, or named like an id of a request, trace or user ("request_id",
// "traceId", "x-request-id"), unless keepField names it. An all-caps key
// ending in ID (PAID, VALID) is a word, not an id.
func isSkipped(key string) bool {
	lk := strings.ToLower(key)
	if keepField[lk] {
		return false
	}
	return skipField[lk] || strings.HasSuffix(lk, "_id") || strings.HasSuffix(lk, "-id") || strings.HasSuffix(lk, ".id") ||
		len(key) > 2 && key != strings.ToUpper(key) && (strings.HasSuffix(key, "Id") || strings.HasSuffix(key, "ID"))
}

// rootCause matches lines that name what went wrong in a multi-line record:
// Java's "Caused by:", exception and error class names followed by their
// message (not stack frames such as Go's "main.handleError(...)"), Node's
// "[cause]:", .NET's "---> " inner exceptions, Go panics.
var rootCause = regexp.MustCompile(`^(?:\[cause\]: |---> )?(?:Caused by: |panic: |fatal error: |(?:[A-Za-z_][\w.$]*)?(?:Exception|Error|Panic)(?::|\s\(|$))`)

// pyCause finds the exception line of the last Python traceback in lines:
// the first unindented line after its frames, such as
// "psycopg2.errors.UniqueViolation: duplicate key" or "httpx.ReadTimeout".
// It returns -1 when there is none.
func pyCause(lines []string) int {
	cause, in := -1, false
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "Traceback (most recent call last):"):
			in = true
		case in && strings.TrimSpace(l) != "" && l[0] != ' ' && l[0] != '\t':
			cause, in = i, false
		}
	}
	return cause
}

// buildState renders ev as compact text for the engine: the first line of
// the message, the root cause of a stack trace (which would otherwise be
// cut off at the end), level and source, structured fields, then the rest
// of the message, within stateBudget.
func buildState(ev *model.Event) string {
	lines := strings.Split(strings.TrimSpace(ev.Message), "\n")
	var parts []string
	parts = append(parts, strings.TrimSpace(lines[0]))
	cause := pyCause(lines)
	for i := len(lines) - 1; i > 0 && cause < 0; i-- {
		if rootCause.MatchString(strings.TrimSpace(lines[i])) {
			cause = i
		}
	}
	if cause > 0 {
		parts = append(parts, strings.TrimSpace(lines[cause]))
	} else {
		cause = -1 // the first line is already first
	}
	// The source name is left out: it isn't part of what happened, and
	// files read as a source share that source's answers.
	if ev.Level != "" {
		parts = append(parts, "level: "+ev.Level)
	}
	if len(ev.Fields) > 0 {
		keys := slices.Sorted(func(yield func(string) bool) {
			for k := range ev.Fields {
				if !yield(k) {
					return
				}
			}
		})
		var kv []string
		for _, k := range keys {
			if isSkipped(k) {
				continue
			}
			v := ev.Fields[k]
			switch v.(type) {
			case map[string]any, []any:
				b, _ := json.Marshal(v)
				kv = append(kv, k+"="+model.Truncate(string(b), 200))
			default:
				kv = append(kv, k+"="+model.Truncate(model.ToString(v), 200))
			}
		}
		if len(kv) > 0 {
			parts = append(parts, strings.Join(kv, " "))
		}
	}
	for i := 1; i < len(lines); i++ {
		if i != cause {
			if t := strings.TrimRight(lines[i], " \t\r"); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return clip(strings.Join(parts, "\n"), stateBudget)
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// TrainingRecord is one line of laya-train JSONL: the state and question
// Lissandra sends for an event, and the answer to learn.
type TrainingRecord struct {
	State     string              `json:"state"`
	Questions map[string]Question `json:"questions"`
	Expected  map[string]string   `json:"expected"`
}

// TrainingRecord builds a laya-train record from a categorized event,
// with the same state and category question Lissandra asks the engine. ok is
// false when ev's category is not one of the configured categories.
func (c *Classifier) TrainingRecord(ev *model.Event) (TrainingRecord, bool) {
	c.mu.Lock()
	cats := c.categories
	c.mu.Unlock()
	if len(cats) == 0 || !slices.ContainsFunc(cats, func(x config.Category) bool { return x.Name == ev.Category }) {
		return TrainingRecord{}, false
	}
	return TrainingRecord{
		State:     buildState(ev),
		Questions: map[string]Question{qCategory: categoryQuestion(cats)},
		Expected:  map[string]string{qCategory: ev.Category},
	}, true
}
