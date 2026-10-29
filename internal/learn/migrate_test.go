package learn

import (
	"testing"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/config"
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
	if rw != 5 || rm != 2 || len(c.Rules) != 4 {
		t.Fatalf("rewrote %d, removed %d: %+v", rw, rm, c.Rules)
	}
	// the approved rule wins and takes the first one's place
	if c.Rules[0].ID != "own" || c.Rules[1].ID != "b" || c.Rules[2].ID != "d" || c.Rules[3].ID != "e" {
		t.Errorf("order %s %s %s %s", c.Rules[0].ID, c.Rules[1].ID, c.Rules[2].ID, c.Rules[3].ID)
	}
	want := classify.Pattern("[req_e3LK2IIpMb] could not get JDBC connection")
	if c.Rules[1].Conditions[0].Value != want || c.Rules[1].Name != ruleName("database", want) || c.Rules[1].Description == "" {
		t.Errorf("survivor %+v", c.Rules[1])
	}
	if got := c.Rules[3].Conditions[0].Value; got != "upstream <id> failed" {
		t.Errorf("an edited value is renormalized, not replaced by its example: %q", got)
	}
	if rw, rm := MigratePatterns(c); rw != 0 || rm != 0 {
		t.Errorf("second run changed %d/%d", rw, rm)
	}
}
