// Package classify assigns a severity, category and tags to log events using
// built-in heuristics followed by the operator's ordered rules.
package classify

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

// Classifier names recorded on events.
const (
	ByHeuristic = "heuristic"
	ByRules     = "rules"
	ByAI        = "ai"
)

// Engine is an immutable, compiled rule set. Build a new one when rules change.
type Engine struct {
	rules []compiledRule
}

type compiledRule struct {
	config.Rule
	conds []compiledCond
}

type compiledCond struct {
	config.Condition
	re    *regexp.Regexp
	value string
}

// New compiles rules. Invalid rules (which config validation should already
// have rejected) are skipped.
func New(rules []config.Rule) *Engine {
	e := &Engine{}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := compiledRule{Rule: r}
		ok := true
		for _, c := range r.Conditions {
			cc := compiledCond{Condition: c, value: c.Value}
			if !c.CaseSensitive {
				cc.value = strings.ToLower(c.Value)
			}
			if c.Op == config.OpRegex || c.Op == config.OpNotRegex {
				pat := c.Value
				if !c.CaseSensitive {
					pat = "(?i)" + pat
				}
				re, err := regexp.Compile(pat)
				if err != nil {
					ok = false
					break
				}
				cc.re = re
			}
			cr.conds = append(cr.conds, cc)
		}
		if ok {
			e.rules = append(e.rules, cr)
		}
	}
	return e
}

// Classify sets ev's severity, category, tags, matched rules and fingerprint.
// It returns false when a rule says the event should be dropped.
func (e *Engine) Classify(ev *model.Event) bool {
	ev.Severity = DetectSeverity(ev.Level, ev.Message)
	ev.Classifier = ByHeuristic
	ev.Category = ""
	ev.MatchedRules = nil

	for _, r := range e.rules {
		if len(r.SourceIDs) > 0 && !slices.Contains(r.SourceIDs, ev.SourceID) {
			continue
		}
		if !r.matches(ev) {
			continue
		}
		ev.MatchedRules = append(ev.MatchedRules, r.Name)
		ev.Classifier = ByRules
		if r.Drop {
			return false
		}
		for _, t := range r.Tags {
			ev.AddTag(t)
		}
		if r.Severity != "" {
			if r.SeverityMode == config.SeverityAtLeast {
				if !ev.Severity.AtLeast(r.Severity) {
					ev.Severity = r.Severity
				}
			} else {
				ev.Severity = r.Severity
			}
		}
		if ev.Category == "" && r.Category != "" {
			ev.Category = r.Category
		}
		if r.Stop {
			break
		}
	}
	if ev.Category == "" {
		ev.Category = model.Uncategorized
	}
	ev.Fingerprint = Fingerprint(ev.SourceID, ev.Message)
	return true
}

func (r *compiledRule) matches(ev *model.Event) bool {
	for _, c := range r.conds {
		m := c.matches(ev)
		if r.Match == config.MatchAny && m {
			return true
		}
		if r.Match != config.MatchAny && !m {
			return false
		}
	}
	return r.Match != config.MatchAny
}

func (c *compiledCond) matches(ev *model.Event) bool {
	raw, present := FieldValue(ev, c.Field)
	switch c.Op {
	case config.OpExists:
		return present
	case config.OpMissing:
		return !present
	case config.OpRegex:
		return present && c.re.MatchString(raw)
	case config.OpNotRegex:
		return !present || !c.re.MatchString(raw)
	case config.OpGreater, config.OpLess:
		if !present {
			return false
		}
		a, err1 := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		b, err2 := strconv.ParseFloat(strings.TrimSpace(c.Value), 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if c.Op == config.OpGreater {
			return a > b
		}
		return a < b
	}
	v := raw
	if !c.CaseSensitive {
		v = strings.ToLower(raw)
	}
	switch c.Op {
	case config.OpContains:
		return present && strings.Contains(v, c.value)
	case config.OpNotContains:
		return !present || !strings.Contains(v, c.value)
	case config.OpEquals:
		return present && v == c.value
	case config.OpNotEquals:
		return !present || v != c.value
	case config.OpStartsWith:
		return present && strings.HasPrefix(v, c.value)
	case config.OpEndsWith:
		return present && strings.HasSuffix(v, c.value)
	}
	return false
}

// FieldValue returns the string form of a named event field. Names other
// than the built-ins resolve as dot paths into ev.Fields (an optional
// "fields." prefix is accepted).
func FieldValue(ev *model.Event, name string) (string, bool) {
	switch name {
	case "message", "msg":
		return ev.Message, true
	case "level":
		return ev.Level, ev.Level != ""
	case "severity":
		return string(ev.Severity), ev.Severity != ""
	case "source":
		return ev.SourceName, true
	case "source_id":
		return ev.SourceID, true
	case "category":
		return ev.Category, ev.Category != ""
	case "tags":
		return strings.Join(ev.Tags, ","), len(ev.Tags) > 0
	}
	if ev.Fields == nil {
		return "", false
	}
	v, ok := model.Lookup(ev.Fields, strings.TrimPrefix(name, "fields."))
	if !ok {
		return "", false
	}
	return model.ToString(v), true
}

var (
	klogPrefix   = regexp.MustCompile(`^([IWEF])\d{4} `)
	leadingLevel = regexp.MustCompile(`(?i)(?:^|[\s\[(|:"'=<])(trace|debug|info|notice|warn|warning|error|err|fatal|crit|critical|panic|emerg|alert)(?:$|[\s\]):|"',>])`)
	kvLevel      = regexp.MustCompile(`(?i)\b(?:level|lvl|severity|loglevel)["']?\s*[=:]\s*["']?([a-z]+)`)
	kwCritical   = regexp.MustCompile(`(?i)\b(fatal|critical|emergency|panic)\b`)
	kwError      = regexp.MustCompile(`(?i)\b(error|errors? occurred|exception|failed|failure|severe)\b`)
	kwWarning    = regexp.MustCompile(`(?i)\b(warn|warning|deprecated|retrying)\b`)
)

// DetectSeverity infers a severity from an explicit level value when there
// is one, otherwise from the message text.
func DetectSeverity(level, message string) model.Severity {
	if level != "" {
		if s, ok := model.ParseSeverity(level); ok {
			return s
		}
		if n, err := strconv.Atoi(strings.TrimSpace(level)); err == nil {
			return numericSeverity(n)
		}
	}
	if m := klogPrefix.FindStringSubmatch(message); m != nil {
		switch m[1] {
		case "I":
			return model.SeverityInfo
		case "W":
			return model.SeverityWarning
		case "E":
			return model.SeverityError
		case "F":
			return model.SeverityCritical
		}
	}
	if m := kvLevel.FindStringSubmatch(message); m != nil {
		if s, ok := model.ParseSeverity(m[1]); ok {
			return s
		}
	}
	head := message
	if len(head) > 96 {
		head = head[:96]
	}
	if m := leadingLevel.FindStringSubmatch(head); m != nil {
		if s, ok := model.ParseSeverity(m[1]); ok {
			return s
		}
	}
	switch {
	case kwCritical.MatchString(message):
		return model.SeverityCritical
	case kwError.MatchString(message):
		return model.SeverityError
	case kwWarning.MatchString(message):
		return model.SeverityWarning
	}
	return model.SeverityInfo
}

// numericSeverity maps syslog priorities (0-7) and pino/bunyan levels
// (10-60) onto severities.
func numericSeverity(n int) model.Severity {
	if n >= 10 {
		switch {
		case n >= 60:
			return model.SeverityCritical
		case n >= 50:
			return model.SeverityError
		case n >= 40:
			return model.SeverityWarning
		case n >= 30:
			return model.SeverityInfo
		default:
			return model.SeverityDebug
		}
	}
	switch {
	case n <= 2:
		return model.SeverityCritical
	case n == 3:
		return model.SeverityError
	case n == 4:
		return model.SeverityWarning
	case n == 5:
		return model.SeverityNotice
	case n == 6:
		return model.SeverityInfo
	default:
		return model.SeverityDebug
	}
}
