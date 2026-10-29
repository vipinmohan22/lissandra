package classify

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// TestDefaultRulesCorpus checks the severity and category the default rules
// give lines that alerting depends on (testdata/rules/defaults.jsonl).
func TestDefaultRulesCorpus(t *testing.T) {
	c := config.Defaults()
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	e := New(c.Rules)
	fh, err := os.Open("testdata/rules/defaults.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for ln := 1; sc.Scan(); ln++ {
		var s struct {
			Line, Level, Severity, Category, Why string
			Fields                               map[string]any
		}
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("line %d: %v", ln, err)
		}
		ev := &model.Event{SourceID: "s", Level: s.Level, Message: s.Line, Fields: s.Fields}
		e.Classify(ev)
		want := s.Category
		if want == "" {
			want = model.Uncategorized
		}
		if string(ev.Severity) != s.Severity || ev.Category != want {
			t.Errorf("defaults.jsonl:%d (%s): got %s/%s, want %s/%s (rules %v): %.100q", ln, s.Why, ev.Severity, ev.Category, s.Severity, want, ev.MatchedRules, s.Line)
		}
	}
}
