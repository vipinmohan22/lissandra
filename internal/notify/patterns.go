package notify

import (
	"math"
	"slices"
	"time"

	"github.com/vipinmohan22/lissandra/internal/model"
)

// sourceState anchors a source's warm-up: until it has been seen for
// Options.Warmup, nothing from it counts as new or worse.
type sourceState struct {
	Name  string    `json:"name"`
	First time.Time `json:"first"` // warm-up anchor
	Last  time.Time `json:"last"`
}

// patternState is what the tracker knows about one fingerprint: how often
// it usually happens, how often it happened lately, and how bad it got.
type patternState struct {
	Source    string         `json:"source"`
	First     time.Time      `json:"first"`
	Last      time.Time      `json:"last"`
	Total     int64          `json:"total"`
	Hour      int64          `json:"hour"` // unix hour HourCount belongs to
	HourCount int            `json:"hour_count"`
	Hours     int            `json:"hours"`    // completed hours folded into Baseline
	Baseline  float64        `json:"baseline"` // events/hour, EWMA of completed hours (first completed hour seeds it)
	Minute    int64          `json:"minute"`   // unix minute of Recent[0]
	Recent    [5]int         `json:"recent"`   // per-minute counts, newest first; backlog events excluded
	MaxSev    model.Severity `json:"max_sev,omitempty"`
	MaxSevAt  time.Time      `json:"max_sev_at,omitzero"`
	SpikeAt   time.Time      `json:"spike_at,omitzero"`
}

// signal is what observe learned about one event.
type signal struct {
	New, Spike, Flood, Worse, Warm bool
	C5                             int            // events in the last 5 minutes, this one included
	Usual                          float64        // events per hour
	PrevMax                        model.Severity // for "worse" texts
	LastSeen                       time.Time      // before this event (zero: unknown); for "back after" texts
	FirstSeen                      time.Time
}

// sawSource records that a source sent an event; its first one starts its
// warm-up.
func (a *alerting) sawSource(id, name string, now time.Time) {
	s := a.sources[id]
	if s == nil {
		a.sources[id] = &sourceState{Name: name, First: now, Last: now}
		a.dirty = true // the warm-up anchor is saved even if nothing routed follows
		return
	}
	s.Name, s.Last = name, now
}

// warmUntil is when a source is past its warm-up.
func (s *sourceState) warmUntil(warmup time.Duration) time.Time { return s.First.Add(warmup) }

// observe counts ev in its pattern and reports what is unusual about it.
func (a *alerting) observe(ev *model.Event, now time.Time, warmup time.Duration) *signal {
	s := &signal{}
	src := a.sources[ev.SourceID]
	s.Warm = warmup < 0 || (src != nil && now.Sub(src.First) >= warmup)
	p := a.patterns[ev.Fingerprint]
	known := p != nil && now.Sub(p.Last) <= newAfter
	if p != nil {
		s.LastSeen = p.Last
	}
	if !known {
		p = &patternState{Source: ev.SourceID, First: now, Hour: now.Unix() / 3600, Minute: now.Unix() / 60}
		a.patterns[ev.Fingerprint] = p
	}
	s.FirstSeen = p.First
	p.roll(now)
	backlog := !ev.Timestamp.IsZero() && now.Sub(ev.Timestamp) > backlogAge
	if !backlog {
		p.Recent[0]++
	}
	p.Total++
	p.HourCount++
	for _, c := range p.Recent {
		s.C5 += c
	}
	s.Usual = p.usual(now)
	if p.MaxSev != "" && now.Sub(p.MaxSevAt) <= newAfter {
		s.PrevMax = p.MaxSev
	}
	s.New = s.Warm && !known
	s.Worse = s.Warm && known && s.PrevMax != "" && ev.Severity.Rank() > s.PrevMax.Rank()
	if !backlog && now.Sub(p.SpikeAt) >= spikeEvery {
		if now.Sub(p.First) >= baselineAge {
			s.Spike = float64(s.C5) >= math.Max(spikeMin, spikeFactor*s.Usual/12)
		} else {
			s.Spike = s.C5 >= floodMin
			s.Flood = s.Spike
		}
	}
	p.Last = now
	if s.PrevMax == "" || ev.Severity.Rank() >= p.MaxSev.Rank() {
		p.MaxSev, p.MaxSevAt = ev.Severity, now
	}
	if len(a.patterns) > maxPatterns {
		a.evictPatterns()
	}
	return s
}

// roll moves the hour and minute counters up to now.
func (p *patternState) roll(now time.Time) {
	if cur := now.Unix() / 3600; cur > p.Hour {
		if p.Hours == 0 {
			p.Baseline = float64(p.HourCount)
		} else {
			p.Baseline = ewmaAlpha*float64(p.HourCount) + (1-ewmaAlpha)*p.Baseline
		}
		if gap := cur - p.Hour; gap > 1 { // empty hours
			p.Baseline *= math.Pow(1-ewmaAlpha, float64(gap-1))
		}
		p.Hours++
		p.Hour, p.HourCount = cur, 0
	}
	// A minute before Minute (the clock went back) counts as Minute.
	if cur := now.Unix() / 60; cur > p.Minute {
		shift := int(min(cur-p.Minute, int64(len(p.Recent))))
		copy(p.Recent[shift:], p.Recent[:len(p.Recent)-shift])
		clear(p.Recent[:shift])
		p.Minute = cur
	}
}

// usual is the pattern's usual rate in events per hour, not counting the
// last five minutes (the burst being judged): the lifetime average while
// the pattern is young, then the hourly EWMA.
func (p *patternState) usual(now time.Time) float64 {
	age := now.Sub(p.First)
	if age < averageAge {
		recent := 0
		for _, c := range p.Recent {
			recent += c
		}
		return float64(max(p.Total-int64(recent), 0)) / math.Max(1, age.Hours())
	}
	return p.Baseline
}

// evictPatterns drops the 10% of patterns seen longest ago.
func (a *alerting) evictPatterns() {
	type kv struct {
		fp   string
		last time.Time
	}
	all := make([]kv, 0, len(a.patterns))
	for fp, p := range a.patterns {
		all = append(all, kv{fp, p.Last})
	}
	slices.SortFunc(all, func(x, y kv) int { return x.last.Compare(y.last) })
	for _, e := range all[:max(len(all)/10, len(all)-maxPatterns)] {
		delete(a.patterns, e.fp)
	}
}

// prunePatterns drops patterns and sources unseen for patternTTL: they
// start over (and a source warms up again) when they come back.
func (a *alerting) prunePatterns(now time.Time) {
	for fp, p := range a.patterns {
		if now.Sub(p.Last) > patternTTL {
			delete(a.patterns, fp)
		}
	}
	for id, s := range a.sources {
		if now.Sub(s.Last) > patternTTL {
			delete(a.sources, id)
		}
	}
}
