package learn

import (
	"strings"
	"testing"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

func legacyRule(id, src, status, cat, example string) config.Rule {
	p := classify.LegacyPattern(example)
	return config.Rule{ID: id, Name: ruleName(cat, p), Enabled: true, Match: config.MatchAll, Status: status, Category: cat,
		SourceIDs:  []string{src},
		Conditions: []config.Condition{{Field: "pattern", Op: config.OpEquals, Value: p}},
		Learned:    &config.Learned{Category: cat, SourceID: src, Example: example}}
}

func TestMigratePatterns(t *testing.T) {
	edited := legacyRule("e", "s1", config.RuleReview, "http", "GET /x took 5s")
	edited.Conditions[0].Value = "upstream x<n>k<n>p failed" // an operator's own value
	c := &config.Config{Rules: []config.Rule{
		{ID: "own", Name: "mine", Enabled: true, Match: config.MatchAll, Conditions: []config.Condition{{Field: "message", Op: config.OpContains, Value: "x"}}},
		legacyRule("a", "s1", config.RuleReview, "database", "[req_e3LK2IIpMb] could not get JDBC connection"),
		legacyRule("b", "s1", config.RuleActive, "database", "[req_QEPeJPPoTB] could not get JDBC connection"),
		legacyRule("c", "s1", config.RuleRejected, "", "[req_7Hq2xK9p] could not get JDBC connection"),
		legacyRule("d", "s2", config.RuleReview, "database", "[req_lwPqnqRTGs] could not get JDBC connection"), // other source
		edited,
	}}
	rw, rm := MigratePatterns(c)
	if rw != 5 || rm != 1 || len(c.Rules) != 5 {
		t.Fatalf("rewrote %d, removed %d: %+v", rw, rm, c.Rules)
	}
	// the approved rule wins and takes the first one's place; the rejected
	// one does something else (no category), so it stays
	if ids := []string{c.Rules[0].ID, c.Rules[1].ID, c.Rules[2].ID, c.Rules[3].ID, c.Rules[4].ID}; strings.Join(ids, " ") != "own b c d e" {
		t.Errorf("order %v", ids)
	}
	want := classify.Pattern("[req_e3LK2IIpMb] could not get JDBC connection")
	if c.Rules[1].Conditions[0].Value != want || c.Rules[1].Name != ruleName("database", want) || c.Rules[1].Description == "" {
		t.Errorf("survivor %+v", c.Rules[1])
	}
	if got := c.Rules[4].Conditions[0].Value; got != "upstream <id> failed" {
		t.Errorf("an edited value is renormalized, not replaced by its example: %q", got)
	}
	if rw, rm := MigratePatterns(c); rw != 0 || rm != 0 {
		t.Errorf("second run changed %d/%d", rw, rm)
	}
}

// Rules learned under pattern version 2 hold letters-only ids and pod
// suffixes it left alone; they migrate to one rule per problem.
func TestMigratePatternsVersion2(t *testing.T) {
	v2 := func(id, status, value, example string) config.Rule {
		return config.Rule{ID: id, Name: ruleName("database", value), Enabled: true, Match: config.MatchAll, Status: status, Category: "database",
			SourceIDs:  []string{"s1"},
			Conditions: []config.Condition{{Field: "pattern", Op: config.OpEquals, Value: value}},
			Learned:    &config.Learned{Category: "database", SourceID: "s1", Example: example}}
	}
	c := &config.Config{Rules: []config.Rule{
		v2("a", config.RuleReview, "[req_pxvjxvmupm] could not get jdbc connection", "[req_pxvjxvmUpm] could not get JDBC connection"),
		v2("b", config.RuleActive, "[req_ydpmkasyma] could not get jdbc connection", "[req_ydPmkasyma] could not get JDBC connection"),
		v2("c", config.RuleReview, "container web in pod web-<id>-ktfjq was oomkilled", ""),
		v2("d", config.RuleReview, "container web in pod web-<id>-rvfj<n> was oomkilled", ""),
	}}
	rw, rm := MigratePatterns(c)
	if rw != 4 || rm != 2 || len(c.Rules) != 2 {
		t.Fatalf("rewrote %d, removed %d: %+v", rw, rm, c.Rules)
	}
	if c.Rules[0].ID != "b" || c.Rules[0].Conditions[0].Value != classify.Pattern("[req_lvuXzawnWL] could not get JDBC connection") {
		t.Errorf("requests: %+v", c.Rules[0])
	}
	if c.Rules[1].ID != "c" || c.Rules[1].Conditions[0].Value != "container web in pod web-<id>-<id> was oomkilled" {
		t.Errorf("pods: %+v", c.Rules[1])
	}
	if rw, rm := MigratePatterns(c); rw != 0 || rm != 0 {
		t.Errorf("second run changed %d/%d", rw, rm)
	}
}

// Learned rules that merge into one pattern but do different things (an
// operator raised one to critical) are all kept: before the migration
// they all applied, and a merge would drop one's severity for good.
func TestMigrateKeepsRulesThatDiffer(t *testing.T) {
	r1 := legacyRule("a", "s1", config.RuleActive, "database", "[req_e3LK2IIpMb] could not get JDBC connection")
	r1.Severity, r1.SeverityMode = model.SeverityError, config.SeverityAtLeast
	r2 := legacyRule("b", "s1", config.RuleActive, "database", "[req_x9Fk2LmQ7z] could not get JDBC connection")
	r2.Severity, r2.SeverityMode = model.SeverityCritical, config.SeverityAtLeast
	c := &config.Config{Rules: []config.Rule{r1, r2}}
	classifyNew := func() model.Severity {
		ev := &model.Event{SourceID: "s1", Message: "[req_Zq8Lm2Wx4K] could not get JDBC connection"}
		classify.New(c.Rules).Classify(ev)
		return ev.Severity
	}
	if s := classifyNew(); s != model.SeverityCritical {
		t.Fatalf("before: %s", s)
	}
	if _, rm := MigratePatterns(c); rm != 0 || len(c.Rules) != 2 {
		t.Fatalf("removed %d: %+v", rm, c.Rules)
	}
	if s := classifyNew(); s != model.SeverityCritical {
		t.Errorf("after: %s", s)
	}
}
