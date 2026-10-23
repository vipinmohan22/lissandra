// Package learn turns the Laya engine's answers into classification rules.
// When the engine categorizes a live event that no rule categorized, Lissandra
// adds a rule for the event's message pattern and source, marked for
// review. From then on the rules classify that pattern (and alert on it)
// without asking the engine, and an operator approves, edits or rejects
// the rule.
package learn

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

const (
	// batchWait is how long proposals collect before they are written, so
	// a burst of new patterns costs one config write.
	batchWait = time.Second
	maxBatch  = 100
	queueSize = 1000
	// exampleLen bounds the example message kept on a learned rule.
	exampleLen = 500
)

// Updater applies a change to the configuration (config.Store.Update).
type Updater func(fn func(c *config.Config) error) error

// Learner is safe for concurrent use.
type Learner struct {
	log    *slog.Logger
	update Updater
	in     chan config.Rule

	mu       sync.Mutex
	settings config.EngineSettings
	known    map[string]bool // source and pattern of every learned rule, any status
	pending  map[string]bool // proposed, not written yet
	review   int             // rules awaiting review
	full     bool            // MaxReviewRules reached (logged once)

	Learned, Dropped atomic.Uint64
}

// Stats is reported in /api/stats.
type Stats struct {
	Enabled       bool    `json:"enabled"`
	MinConfidence float64 `json:"min_confidence"` // the effective threshold
	Learned       uint64  `json:"learned"`        // rules learned since Lissandra started
	Review        int     `json:"review"`         // rules awaiting review
	MaxReview     int     `json:"max_review"`
	Dropped       uint64  `json:"dropped"` // answers not learned: too many rules awaiting review, or a backlog
}

// New returns a learner that writes rules with update. Call Configure, and
// Run to write them.
func New(log *slog.Logger, update Updater) *Learner {
	return &Learner{log: log, update: update, in: make(chan config.Rule, queueSize),
		known: map[string]bool{}, pending: map[string]bool{}}
}

func key(sourceID, pattern string) string { return sourceID + "\x00" + pattern }

// patternOf returns the pattern a learned rule matches, or "" for any
// other rule.
func patternOf(r *config.Rule) string {
	if r.Learned == nil || len(r.Conditions) != 1 {
		return ""
	}
	c := r.Conditions[0]
	if c.Field != "pattern" || c.Op != config.OpEquals {
		return ""
	}
	return c.Value
}

// Configure applies cfg: the settings, and the learned rules it holds.
func (l *Learner) Configure(cfg *config.Config) {
	known := map[string]bool{}
	review := 0
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if p := patternOf(r); p != "" {
			known[key(r.Learned.SourceID, p)] = true
		}
		if r.Status == config.RuleReview {
			review++
		}
	}
	l.mu.Lock()
	l.settings, l.known, l.review = cfg.Settings.Engine, known, review
	if review < config.MaxReviewRules {
		l.full = false
	}
	l.mu.Unlock()
}

// threshold is the confidence an answer needs to be learned.
func threshold(s config.EngineSettings) float64 {
	return max(s.LearnMinConfidence, s.MinConfidence)
}

// Observe proposes a rule for ev when the engine's category answer was
// applied to it with enough confidence and no rule covers its pattern yet.
// It never blocks.
func (l *Learner) Observe(ev *model.Event) {
	e := ev.Engine
	if e == nil || !e.CategoryApplied || ev.ImportID != "" {
		return
	}
	pattern := classify.Pattern(ev.Message)
	if pattern == "" {
		return
	}
	k := key(ev.SourceID, pattern)
	l.mu.Lock()
	s := l.settings
	switch {
	case !s.LearnRules || e.Confidence < threshold(s) || l.known[k] || l.pending[k]:
		l.mu.Unlock()
		return
	case l.review+len(l.pending) >= config.MaxReviewRules:
		first := !l.full
		l.full = true
		l.mu.Unlock()
		l.Dropped.Add(1)
		if first {
			l.log.Warn("not learning new rules: too many await review; approve or reject some", "max", config.MaxReviewRules)
		}
		return
	}
	l.pending[k] = true
	l.mu.Unlock()

	select {
	case l.in <- newRule(ev, pattern):
	default:
		l.mu.Lock()
		delete(l.pending, k)
		l.mu.Unlock()
		l.Dropped.Add(1)
	}
}

// newRule is the rule learned from ev, whose engine answer was applied.
func newRule(ev *model.Event, pattern string) config.Rule {
	e := ev.Engine
	r := config.Rule{
		Name:       fmt.Sprintf("Learned: %s — %s", e.Category, model.Truncate(pattern, 60)),
		Enabled:    true,
		Match:      config.MatchAll,
		Conditions: []config.Condition{{Field: "pattern", Op: config.OpEquals, Value: pattern}},
		SourceIDs:  []string{ev.SourceID},
		Category:   e.Category,
		Status:     config.RuleReview,
		Learned: &config.Learned{
			At:         time.Now().UTC(),
			Model:      e.Model,
			Category:   e.Category,
			Confidence: e.Confidence,
			SourceID:   ev.SourceID,
			EventID:    ev.ID,
			Example:    model.Truncate(ev.Message, exampleLen),
		},
	}
	if e.SeverityApplied {
		// At least: a later record of the pattern may state a higher level.
		r.Severity, r.SeverityMode = ev.Severity, config.SeverityAtLeast
		r.Learned.Severity, r.Learned.SeverityConfidence = e.Severity, e.SeverityConfidence
	}
	return r
}

// Run writes proposed rules until ctx ends.
func (l *Learner) Run(ctx context.Context) {
	for {
		var batch []config.Rule
		select {
		case <-ctx.Done():
			return
		case r := <-l.in:
			batch = append(batch, r)
		}
		t := time.NewTimer(batchWait)
	collect:
		for len(batch) < maxBatch {
			select {
			case r := <-l.in:
				batch = append(batch, r)
			case <-t.C:
				break collect
			case <-ctx.Done():
				break collect
			}
		}
		t.Stop()
		l.write(batch)
	}
}

// write appends batch to the rules, skipping patterns a rule learned
// meanwhile (from another batch, or a config import) already covers.
func (l *Learner) write(batch []config.Rule) {
	added := 0
	err := l.update(func(c *config.Config) error {
		added = 0
		have := map[string]bool{}
		for i := range c.Rules {
			if p := patternOf(&c.Rules[i]); p != "" {
				have[key(c.Rules[i].Learned.SourceID, p)] = true
			}
		}
		sources := map[string]bool{}
		for _, src := range c.Sources {
			sources[src.ID] = true
		}
		for _, r := range batch {
			k := key(r.Learned.SourceID, patternOf(&r))
			// A source deleted meanwhile would leave a rule for every source.
			if have[k] || !sources[r.Learned.SourceID] {
				continue
			}
			have[k] = true
			c.Rules = append(c.Rules, r)
			added++
		}
		return nil
	})
	l.mu.Lock()
	for _, r := range batch {
		delete(l.pending, key(r.Learned.SourceID, patternOf(&r)))
	}
	l.mu.Unlock()
	if err != nil {
		l.Dropped.Add(uint64(len(batch)))
		l.log.Error("saving learned rules failed", "rules", len(batch), "err", err)
		return
	}
	if added > 0 {
		l.Learned.Add(uint64(added))
		l.log.Info("learned rules from the Laya engine, awaiting review", "rules", added)
	}
}

// Stats reports what the learner did.
func (l *Learner) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Stats{
		Enabled:       l.settings.LearnRules,
		MinConfidence: threshold(l.settings),
		Learned:       l.Learned.Load(),
		Review:        l.review,
		MaxReview:     config.MaxReviewRules,
		Dropped:       l.Dropped.Load(),
	}
}
