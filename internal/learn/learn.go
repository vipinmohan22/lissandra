// Package learn turns the engine's answers into classification rules.
// When the engine categorizes a live event that no rule categorized, Lissandra
// adds a rule for the event's message pattern and source, marked for
// review. From then on the rules classify that pattern (and alert on it)
// without asking the engine, and an operator approves, edits or rejects
// the rule.
package learn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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
			known[key(r.Learned.SourceID, classify.Renormalize(p))] = true
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
		Name:       ruleName(e.Category, pattern),
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

// MigratePatterns brings learned rules saved under an older
// classify.Pattern up to date, in c: a rule that still holds the pattern
// of its example gets the example's pattern now; any other gets its
// pattern renormalized. Learned rules that then share a source and a
// pattern, and do the same to the events they match (category, severity,
// tags, drop, stop, enabled…), are merged into one: an approved rule wins
// over a rejected one, which wins over one awaiting review, the earliest
// on a tie; it takes the place of the first of them. Rules that do
// something else are kept, as they all applied before. It returns how
// many rules were rewritten and how many removed. Running it again
// changes nothing.
func MigratePatterns(c *config.Config) (rewritten, removed int) {
	for i := range c.Rules {
		r := &c.Rules[i]
		old := patternOf(r)
		if old == "" {
			continue
		}
		next := classify.Renormalize(old)
		if ex := r.Learned.Example; ex != "" && classify.LegacyPattern(ex) == old {
			next = classify.Pattern(ex)
		}
		if next == old || next == "" {
			continue
		}
		if r.Name == ruleName(r.Learned.Category, old) {
			r.Name = ruleName(r.Learned.Category, next)
		}
		r.Conditions[0].Value = next
		rewritten++
	}
	rank := func(r *config.Rule) int {
		switch r.Status {
		case config.RuleActive:
			return 0
		case config.RuleRejected:
			return 1
		}
		return 2
	}
	type group struct{ first, keep, n int }
	groups := map[string]*group{}
	keys := make([]string, len(c.Rules)) // "" for rules that aren't learned
	var order []string
	for i := range c.Rules {
		r := &c.Rules[i]
		p := patternOf(r)
		if p == "" {
			continue
		}
		// Only rules that do the same merge: the others all applied before.
		k := strings.Join(r.SourceIDs, ",") + "\x00" + key(r.Learned.SourceID, p) + "\x00" + action(*r)
		keys[i] = k
		g := groups[k]
		if g == nil {
			groups[k] = &group{first: i, keep: i, n: 1}
			order = append(order, k)
			continue
		}
		g.n++
		if rank(r) < rank(&c.Rules[g.keep]) {
			g.keep = i
		}
	}
	drop := map[int]bool{}
	for _, k := range order {
		g := groups[k]
		if g.n == 1 {
			continue
		}
		keep := c.Rules[g.keep]
		if keep.Description == "" {
			keep.Description = fmt.Sprintf("Merged from %d learned rules when message patterns changed.", g.n)
		}
		c.Rules[g.first] = keep
		for i := range c.Rules {
			if i != g.first && keys[i] == k {
				drop[i] = true
			}
		}
	}
	if len(drop) > 0 {
		out := c.Rules[:0]
		for i, r := range c.Rules {
			if !drop[i] {
				out = append(out, r)
			}
		}
		c.Rules = out
		removed = len(drop)
	}
	return rewritten, removed
}

// action is what rule r does to the events it matches, what makes it a
// rule (name, review status, learned details) aside: rules with the same
// action and pattern can be merged.
func action(r config.Rule) string {
	r.ID, r.Name, r.Description, r.Status, r.Learned = "", "", "", "", nil
	b, _ := json.Marshal(r)
	return string(b)
}

// ruleName is the name a rule learned for pattern gets.
func ruleName(category, pattern string) string {
	return fmt.Sprintf("Learned: %s — %s", category, model.Truncate(pattern, 60))
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
				have[key(c.Rules[i].Learned.SourceID, classify.Renormalize(p))] = true
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
		l.log.Info("learned rules from the engine, awaiting review", "rules", added)
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
