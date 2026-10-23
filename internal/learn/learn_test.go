package learn

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

func setup(t *testing.T) (*Learner, *config.Store, string) {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	l := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store.Update)
	store.Subscribe(l.Configure)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)
	return l, store, store.Get().Sources[0].ID
}

func answered(src, msg string, conf float64) *model.Event {
	return &model.Event{ID: model.NewID(), SourceID: src, Message: msg, Category: "audit", Severity: model.SeverityError,
		Engine: &model.EngineResult{Model: "english", Category: "audit", Confidence: conf, Applied: true, CategoryApplied: true}}
}

func learned(store *config.Store) []config.Rule {
	var out []config.Rule
	for _, r := range store.Get().Rules {
		if r.Learned != nil {
			out = append(out, r)
		}
	}
	return out
}

func waitRules(t *testing.T, store *config.Store, n int) []config.Rule {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(learned(store)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d learned rules, want %d", len(learned(store)), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(batchWait + 200*time.Millisecond) // nothing more arrives
	return learned(store)
}

func TestLearnsConfidentLiveAnswersOncePerPattern(t *testing.T) {
	l, store, src := setup(t)
	l.Observe(answered(src, "shipment 1 stuck", 0.95))
	l.Observe(answered(src, "shipment 2 stuck", 0.95)) // the same pattern
	l.Observe(answered(src, "invoice 3 late", 0.7))    // below the learning threshold
	file := answered(src, "label 4 missing", 0.95)
	file.ImportID = "imp" // file analysis never learns
	l.Observe(file)
	l.Observe(answered("unknown-source", "parcel 5 lost", 0.95))
	rules := waitRules(t, store, 1)
	if len(rules) != 1 || rules[0].Conditions[0].Value != "shipment <n> stuck" || rules[0].Status != config.RuleReview {
		t.Fatalf("rules: %+v", rules)
	}
	// Known from the config now: not proposed again.
	l.Observe(answered(src, "shipment 3 stuck", 0.99))
	if rules := waitRules(t, store, 1); len(rules) != 1 {
		t.Errorf("relearned: %+v", rules)
	}
	if st := l.Stats(); st.Learned != 1 || st.Review != 1 || st.MinConfidence != config.DefaultLearnMinConfidence {
		t.Errorf("stats: %+v", st)
	}
}

func TestLearningStopsAtTheReviewLimit(t *testing.T) {
	l, store, src := setup(t)
	for i := range config.MaxReviewRules + 5 {
		l.Observe(answered(src, fmt.Sprintf("problem %c%c", 'a'+i/26%26, 'a'+i%26), 0.95))
	}
	waitRules(t, store, config.MaxReviewRules)
	if n := len(learned(store)); n != config.MaxReviewRules {
		t.Errorf("%d rules awaiting review", n)
	}
	if l.Stats().Dropped != 5 {
		t.Errorf("dropped: %d", l.Stats().Dropped)
	}
}
