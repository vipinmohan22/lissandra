// Package events keeps recent classified events in memory and aggregates
// statistics for the dashboard.
package events

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/model"
)

const (
	timelineMinutes = 60
	maxPatterns     = 5000
)

// Store is a fixed-size ring buffer of the most recent events plus rolling
// per-minute counters.
type Store struct {
	mu      sync.RWMutex
	buf     []model.Event
	next    int
	size    int
	total   uint64
	minutes [timelineMinutes]bucket

	patterns map[string]*Pattern
}

type bucket struct {
	minute     int64
	total      int
	bySeverity map[model.Severity]int
	byCategory map[string]int
	bySource   map[string]int
}

// Pattern aggregates events sharing a fingerprint.
type Pattern struct {
	Fingerprint string         `json:"fingerprint"`
	Pattern     string         `json:"pattern"`
	Sample      string         `json:"sample"`
	SourceID    string         `json:"source_id"`
	SourceName  string         `json:"source_name"`
	Category    string         `json:"category"`
	Severity    model.Severity `json:"severity"`
	Count       uint64         `json:"count"`
	FirstSeen   time.Time      `json:"first_seen"`
	LastSeen    time.Time      `json:"last_seen"`
	LastEventID string         `json:"last_event_id"`
}

// NewStore returns a store retaining up to capacity events.
func NewStore(capacity int) *Store {
	if capacity < 100 {
		capacity = 100
	}
	return &Store{buf: make([]model.Event, capacity), patterns: map[string]*Pattern{}}
}

// Add records a classified event.
func (s *Store) Add(ev model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.next] = ev
	s.next = (s.next + 1) % len(s.buf)
	if s.size < len(s.buf) {
		s.size++
	}
	s.total++

	minute := ev.ReceivedAt.Unix() / 60
	b := &s.minutes[minute%timelineMinutes]
	if b.minute != minute {
		*b = bucket{minute: minute, bySeverity: map[model.Severity]int{}, byCategory: map[string]int{}, bySource: map[string]int{}}
	}
	b.total++
	b.bySeverity[ev.Severity]++
	b.byCategory[ev.Category]++
	b.bySource[ev.SourceName]++

	p, ok := s.patterns[ev.Fingerprint]
	if !ok {
		if len(s.patterns) >= maxPatterns {
			s.evictPatterns()
		}
		p = &Pattern{Fingerprint: ev.Fingerprint, Pattern: classify.Pattern(ev.Message), SourceID: ev.SourceID, SourceName: ev.SourceName, FirstSeen: ev.ReceivedAt}
		s.patterns[ev.Fingerprint] = p
	}
	p.Count++
	p.LastSeen = ev.ReceivedAt
	p.LastEventID = ev.ID
	p.Sample = model.Truncate(ev.Message, 300)
	p.Category = ev.Category
	if ev.Severity.Rank() > p.Severity.Rank() {
		p.Severity = ev.Severity
	}
}

// evictPatterns drops the least recently seen half of the pattern table.
func (s *Store) evictPatterns() {
	list := make([]*Pattern, 0, len(s.patterns))
	for _, p := range s.patterns {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].LastSeen.Before(list[j].LastSeen) })
	for _, p := range list[:len(list)/2] {
		delete(s.patterns, p.Fingerprint)
	}
}

// Query filters stored events.
type Query struct {
	MinSeverity model.Severity
	Category    string
	SourceID    string
	Tag         string
	Fingerprint string
	Text        string
	Before      string // event ID cursor: return events older than this one
	Limit       int
}

// Find returns matching events, newest first.
func (s *Store) Find(q Query) []model.Event {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	text := strings.ToLower(q.Text)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Event, 0, min(q.Limit, s.size))
	skipping := q.Before != ""
	for i := 0; i < s.size && len(out) < q.Limit; i++ {
		ev := &s.buf[(s.next-1-i+len(s.buf))%len(s.buf)]
		if skipping {
			if ev.ID == q.Before {
				skipping = false
			}
			continue
		}
		if q.MinSeverity != "" && !ev.Severity.AtLeast(q.MinSeverity) {
			continue
		}
		if q.Category != "" && ev.Category != q.Category {
			continue
		}
		if q.SourceID != "" && ev.SourceID != q.SourceID {
			continue
		}
		if q.Tag != "" && !ev.HasTag(q.Tag) {
			continue
		}
		if q.Fingerprint != "" && ev.Fingerprint != q.Fingerprint {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(ev.Message), text) {
			continue
		}
		out = append(out, *ev)
	}
	return out
}

// All returns every retained event, newest first.
func (s *Store) All() []model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Event, 0, s.size)
	for i := 0; i < s.size; i++ {
		out = append(out, s.buf[(s.next-1-i+len(s.buf))%len(s.buf)])
	}
	return out
}

// Get returns the stored event with id.
func (s *Store) Get(id string) (model.Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := 0; i < s.size; i++ {
		ev := s.buf[(s.next-1-i+len(s.buf))%len(s.buf)]
		if ev.ID == id {
			return ev, true
		}
	}
	return model.Event{}, false
}

// Stats summarises the last hour of activity.
type Stats struct {
	TotalSinceStart uint64                 `json:"total_since_start"`
	Retained        int                    `json:"retained"`
	LastHour        int                    `json:"last_hour"`
	BySeverity      map[model.Severity]int `json:"by_severity"`
	ByCategory      map[string]int         `json:"by_category"`
	BySource        map[string]int         `json:"by_source"`
	Timeline        []TimelinePoint        `json:"timeline"`
	TopPatterns     []Pattern              `json:"top_patterns"`
}

// TimelinePoint is one minute of event counts by severity.
type TimelinePoint struct {
	Time       time.Time              `json:"time"`
	Total      int                    `json:"total"`
	BySeverity map[model.Severity]int `json:"by_severity"`
}

// Stats computes dashboard statistics as of now.
func (s *Store) Stats(now time.Time) Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{
		TotalSinceStart: s.total,
		Retained:        s.size,
		BySeverity:      map[model.Severity]int{},
		ByCategory:      map[string]int{},
		BySource:        map[string]int{},
		Timeline:        make([]TimelinePoint, 0, timelineMinutes),
	}
	cur := now.Unix() / 60
	for m := cur - timelineMinutes + 1; m <= cur; m++ {
		pt := TimelinePoint{Time: time.Unix(m*60, 0).UTC(), BySeverity: map[model.Severity]int{}}
		b := &s.minutes[m%timelineMinutes]
		if b.minute == m {
			pt.Total = b.total
			st.LastHour += b.total
			for k, v := range b.bySeverity {
				pt.BySeverity[k] = v
				st.BySeverity[k] += v
			}
			for k, v := range b.byCategory {
				st.ByCategory[k] += v
			}
			for k, v := range b.bySource {
				st.BySource[k] += v
			}
		}
		st.Timeline = append(st.Timeline, pt)
	}
	cutoff := now.Add(-time.Hour)
	for _, p := range s.patterns {
		if p.LastSeen.After(cutoff) {
			st.TopPatterns = append(st.TopPatterns, *p)
		}
	}
	// Errors and worse first, then by volume.
	sort.Slice(st.TopPatterns, func(i, j int) bool {
		a, b := st.TopPatterns[i], st.TopPatterns[j]
		ah, bh := a.Severity.AtLeast(model.SeverityError), b.Severity.AtLeast(model.SeverityError)
		if ah != bh {
			return ah
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.LastSeen.After(b.LastSeen)
	})
	if len(st.TopPatterns) > 15 {
		st.TopPatterns = st.TopPatterns[:15]
	}
	return st
}
