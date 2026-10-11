package classify

import (
	"testing"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

func TestDetectSeverity(t *testing.T) {
	cases := []struct {
		level, msg string
		want       model.Severity
	}{
		{"ERROR", "anything", model.SeverityError},
		{"warn", "", model.SeverityWarning},
		{"fatal", "", model.SeverityCritical},
		{"3", "", model.SeverityError},  // syslog
		{"6", "", model.SeverityInfo},   // syslog
		{"50", "", model.SeverityError}, // pino
		{"30", "", model.SeverityInfo},  // pino
		{"60", "", model.SeverityCritical},
		{"", "2026-10-11 10:00:00 ERROR something broke", model.SeverityError},
		{"", "[WARN] disk at 85%", model.SeverityWarning},
		{"", "level=debug msg=hello", model.SeverityDebug},
		{"", `{"level":"error"}`, model.SeverityError},
		{"", "E1011 04:10:03.123456 1 controller.go:88] failed to sync", model.SeverityError},
		{"", "I1011 04:10:03.123456 1 controller.go:88] synced", model.SeverityInfo},
		{"", "INFO request failed for user", model.SeverityInfo}, // explicit level wins over keywords
		{"", "kernel panic - not syncing", model.SeverityCritical},
		{"", "request failed with exception", model.SeverityError},
		{"", "this API is deprecated", model.SeverityWarning},
		{"", "all good", model.SeverityInfo},
		{"bogus", "all good", model.SeverityInfo},
	}
	for _, c := range cases {
		if got := DetectSeverity(c.level, c.msg); got != c.want {
			t.Errorf("DetectSeverity(%q, %q) = %s, want %s", c.level, c.msg, got, c.want)
		}
	}
}

func rule(name string, conds ...config.Condition) config.Rule {
	return config.Rule{Name: name, Enabled: true, Match: config.MatchAll, Conditions: conds, SeverityMode: config.SeveritySet}
}

func cond(field, op, value string) config.Condition {
	return config.Condition{Field: field, Op: op, Value: value}
}

func TestRuleSemantics(t *testing.T) {
	db := rule("db", cond("message", config.OpContains, "postgres"))
	db.Category = "database"
	db.Tags = []string{"db"}
	net := rule("net", cond("message", config.OpRegex, `timeout|refused`))
	net.Category = "network"
	net.Severity = model.SeverityError
	net.SeverityMode = config.SeverityAtLeast
	net.Tags = []string{"net"}
	quiet := rule("quiet", cond("message", config.OpContains, "expected"))
	quiet.Severity = model.SeverityInfo // "set" can lower
	quiet.Stop = true
	never := rule("never", cond("message", config.OpContains, "expected"))
	never.Category = "unreachable"
	drop := rule("drop", cond("message", config.OpStartsWith, "GET /healthz"))
	drop.Drop = true

	e := New([]config.Rule{drop, db, net, quiet, never})

	ev := model.Event{Message: "postgres connection refused", Level: "warn", SourceID: "s"}
	if !e.Classify(&ev) {
		t.Fatal("dropped unexpectedly")
	}
	if ev.Category != "database" {
		t.Errorf("first category should win, got %q", ev.Category)
	}
	if ev.Severity != model.SeverityError {
		t.Errorf("at_least should raise warning to error, got %s", ev.Severity)
	}
	if len(ev.Tags) != 2 || ev.Classifier != ByRules || len(ev.MatchedRules) != 2 {
		t.Errorf("tags=%v classifier=%s matched=%v", ev.Tags, ev.Classifier, ev.MatchedRules)
	}

	ev = model.Event{Message: "expected timeout during shutdown", Level: "error"}
	e.Classify(&ev)
	if ev.Severity != model.SeverityInfo {
		t.Errorf("set mode should lower severity, got %s", ev.Severity)
	}
	if ev.Category != "network" {
		t.Errorf("category = %q, want network (stop prevents later rules)", ev.Category)
	}

	ev = model.Event{Message: "GET /healthz 200"}
	if e.Classify(&ev) {
		t.Error("health check should be dropped")
	}

	ev = model.Event{Message: "nothing interesting"}
	e.Classify(&ev)
	if ev.Category != model.Uncategorized || ev.Classifier != ByHeuristic || ev.Fingerprint == "" {
		t.Errorf("got category=%q classifier=%q fp=%q", ev.Category, ev.Classifier, ev.Fingerprint)
	}
}

func TestMatchAnyAndScopes(t *testing.T) {
	r := rule("5xx", cond("status", config.OpRegex, `^5\d\d$`), cond("http.status", config.OpGreater, "499"))
	r.Match = config.MatchAny
	r.Category = "http"
	r.SourceIDs = []string{"web"}
	e := New([]config.Rule{r})

	ev := model.Event{SourceID: "web", Message: "x", Fields: map[string]any{"http": map[string]any{"status": float64(503)}}}
	e.Classify(&ev)
	if ev.Category != "http" {
		t.Errorf("nested numeric gt should match, got %q", ev.Category)
	}
	ev = model.Event{SourceID: "other", Message: "x", Fields: map[string]any{"status": "500"}}
	e.Classify(&ev)
	if ev.Category != model.Uncategorized {
		t.Errorf("rule scoped to another source should not match, got %q", ev.Category)
	}
}

func TestOperators(t *testing.T) {
	ev := &model.Event{Message: "Hello World", Level: "INFO", Fields: map[string]any{"n": "42", "user": map[string]any{"id": "u1"}}}
	cases := []struct {
		c    config.Condition
		want bool
	}{
		{cond("message", config.OpContains, "world"), true},
		{config.Condition{Field: "message", Op: config.OpContains, Value: "world", CaseSensitive: true}, false},
		{cond("message", config.OpNotContains, "bye"), true},
		{cond("level", config.OpEquals, "info"), true},
		{cond("level", config.OpNotEquals, "info"), false},
		{cond("message", config.OpEndsWith, "WORLD"), true},
		{cond("user.id", config.OpEquals, "u1"), true},
		{cond("fields.user.id", config.OpEquals, "u1"), true},
		{cond("missing.field", config.OpMissing, ""), true},
		{cond("n", config.OpExists, ""), true},
		{cond("n", config.OpLess, "50"), true},
		{cond("n", config.OpGreater, "50"), false},
		{cond("missing", config.OpNotRegex, "x"), true},
		{cond("missing", config.OpRegex, ".*"), false},
	}
	for _, c := range cases {
		r := rule("r", c.c)
		r.Category = "hit"
		e := New([]config.Rule{r})
		copyEv := *ev
		e.Classify(&copyEv)
		if got := copyEv.Category == "hit"; got != c.want {
			t.Errorf("%+v: matched=%v want %v", c.c, got, c.want)
		}
	}
}

func TestFingerprintGroupsVariants(t *testing.T) {
	a := Fingerprint("s", "payment failed for order 1234 from 10.0.0.1:443 at 2026-10-11T10:00:00Z id=3f2b9c1e-1111-2222-3333-444455556666")
	b := Fingerprint("s", "payment failed for order 98 from 192.168.1.20:8080 at 2026-10-12T11:22:33Z id=aaaaaaaa-bbbb-cccc-dddd-eeeeffff0000")
	if a != b {
		t.Errorf("variants should share a fingerprint: %s vs %s", a, b)
	}
	if Fingerprint("other", "payment failed for order 1") == Fingerprint("s", "payment failed for order 1") {
		t.Error("different sources should not share a fingerprint")
	}
	if Fingerprint("s", "payment failed") == Fingerprint("s", "payment succeeded") {
		t.Error("different messages should differ")
	}
}

func TestDefaultRulesCompileAndClassify(t *testing.T) {
	c := config.Defaults()
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	e := New(c.Rules)
	if len(e.rules) == 0 {
		t.Fatal("no default rules compiled")
	}
	cases := map[string]string{
		"panic: runtime error: invalid memory address":   "application",
		"kernel: Out of memory: Killed process 1 (java)": "resource",
		"login failed for user bob":                      "security",
		"ERROR: deadlock detected":                       "database",
		"dial tcp 10.0.0.1:5432: connection refused":     "network",
		"deployment v1.2 rolled out":                     "deployment",
	}
	for msg, want := range cases {
		ev := model.Event{Message: msg}
		e.Classify(&ev)
		if ev.Category != want {
			t.Errorf("%q: category %q, want %q (rules %v)", msg, ev.Category, want, ev.MatchedRules)
		}
	}
}
