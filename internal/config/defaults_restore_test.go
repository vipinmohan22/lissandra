package config

import (
	"slices"
	"testing"
)

func TestRestoreDefaultRules(t *testing.T) {
	c := Defaults()
	Normalize(c)
	// an install from before: no "Login failures", an old "Crash / panic",
	// an operator's own rule, and a default the operator disabled
	c.Rules = slices.DeleteFunc(c.Rules, func(r Rule) bool { return r.Name == "Login failures" })
	i := slices.IndexFunc(c.Rules, func(r Rule) bool { return r.Name == "Crash / panic" })
	c.Rules[i].Conditions = []Condition{cond("message", OpRegex, `(?i)(panic:|unhandled exception)`)}
	c.Rules[i].Enabled = false
	id := c.Rules[i].ID
	c.Rules = append(c.Rules, Rule{ID: "mine", Name: "Mine", Enabled: true, Match: MatchAll, Conditions: []Condition{cond("message", OpContains, "x")}})
	missing, changed := DefaultRuleChanges(c)
	if !slices.Equal(missing, []string{"Login failures"}) || !slices.Equal(changed, []string{"Crash / panic"}) {
		t.Fatalf("missing %v changed %v", missing, changed)
	}
	if n := RestoreDefaultRules(c, nil); n != 2 {
		t.Fatalf("restored %d", n)
	}
	if m, ch := DefaultRuleChanges(c); len(m)+len(ch) != 0 {
		t.Errorf("still differs: %v %v", m, ch)
	}
	j := slices.IndexFunc(c.Rules, func(r Rule) bool { return r.Name == "Crash / panic" })
	if c.Rules[j].ID != id || c.Rules[j].Enabled {
		t.Errorf("a restored rule keeps its ID and enabled state: %+v", c.Rules[j])
	}
	k := slices.IndexFunc(c.Rules, func(r Rule) bool { return r.Name == "Login failures" })
	if c.Rules[k-1].Name != "PostgreSQL client rejected" || c.Rules[len(c.Rules)-1].ID != "mine" {
		t.Errorf("Login failures at %d after %q; last %q", k, c.Rules[k-1].Name, c.Rules[len(c.Rules)-1].Name)
	}
	if err := Normalize(c); err != nil {
		t.Fatal(err)
	}
	if RestoreDefaultRules(c, nil) != 0 {
		t.Error("a second restore changes nothing")
	}
}

// An install from before has "Crash / panic" ahead of "Resource exhaustion",
// which this version moved first (a PostgreSQL disk-full PANIC is a
// resource problem): the order counts as a change, and restoring fixes it.
func TestRestoreDefaultRulesOrder(t *testing.T) {
	c := Defaults()
	Normalize(c)
	i := slices.IndexFunc(c.Rules, func(r Rule) bool { return r.Name == "Resource exhaustion" })
	r := c.Rules[i]
	c.Rules = slices.Delete(c.Rules, i, i+1)
	j := slices.IndexFunc(c.Rules, func(r Rule) bool { return r.Name == "Service down" })
	c.Rules = slices.Insert(c.Rules, j+1, r)
	c.Rules = append(c.Rules, Rule{ID: "mine", Name: "Mine", Enabled: true, Match: MatchAll, Conditions: []Condition{cond("message", OpContains, "x")}})
	if missing, changed := DefaultRuleChanges(c); len(missing) != 0 || !slices.Equal(changed, []string{"Resource exhaustion"}) {
		t.Fatalf("missing %v changed %v", missing, changed)
	}
	if n := RestoreDefaultRules(c, nil); n != 1 {
		t.Fatalf("restored %d", n)
	}
	d := Defaults()
	Normalize(d)
	var got, want []string
	for _, r := range c.Rules {
		got = append(got, r.Name)
	}
	for _, r := range d.Rules {
		want = append(want, r.Name)
	}
	if !slices.Equal(got, append(want, "Mine")) {
		t.Errorf("order %v", got)
	}
	if m, ch := DefaultRuleChanges(c); len(m)+len(ch) != 0 || RestoreDefaultRules(c, nil) != 0 {
		t.Errorf("still differs: %v %v", m, ch)
	}
}
