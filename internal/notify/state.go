package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/vipinmohan22/lissandra/internal/atomicfile"
	"github.com/vipinmohan22/lissandra/internal/classify"
)

// savedState is alerting.json: what was sent, pending digests and the
// per-pattern baselines, so a restart neither re-alerts nor forgets. It
// holds log samples, hence mode 0600. It is not part of the config (saving
// it never reconfigures anything) and not exported.
type savedState struct {
	Version        int                      `json:"version"`
	PatternVersion int                      `json:"pattern_version"` // classify.PatternVersion: fingerprints are the keys
	SavedAt        time.Time                `json:"saved_at"`
	Sources        map[string]*sourceState  `json:"sources"`
	Patterns       map[string]*patternState `json:"patterns"`
	Alerts         map[string]*alertState   `json:"alerts"`
	Digests        map[string]*digestBuffer `json:"digests"`
	Recent         map[string][]time.Time   `json:"recent"`
	Critical       map[string][]time.Time   `json:"critical"`
	Promoted       map[string]time.Time     `json:"promoted"`
	Summaries      map[string]time.Time     `json:"summaries"`
}

// loadState reads d.statePath into d.al. A missing file is a fresh start;
// a file that can't be used is set aside as .bad-<unix> and Lissandra
// starts fresh, every source warming up.
func (d *Dispatcher) loadState() {
	data, err := os.ReadFile(d.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var s savedState
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	if err == nil && s.Version != stateVersion {
		err = fmt.Errorf("unknown version %d", s.Version)
	}
	if err != nil {
		bad := fmt.Sprintf("%s.bad-%d", d.statePath, d.now().Unix())
		if rerr := os.Rename(d.statePath, bad); rerr != nil {
			bad = ""
		}
		d.log.Warn("alert state unreadable: starting fresh; sources warm up again", "file", d.statePath, "set_aside", bad, "err", err)
		return
	}
	if s.PatternVersion != classify.PatternVersion {
		s.Sources, s.Patterns, s.Alerts = nil, nil, nil
		d.log.Info("message patterns changed: alert history reset; sources warm up for 1 h", "was", s.PatternVersion, "now", classify.PatternVersion)
	}
	now := d.now()
	clampState(&s, now)
	a := &d.al
	a.sources, a.patterns, a.alerts, a.digests = s.Sources, s.Patterns, s.Alerts, s.Digests
	a.recent, a.critical, a.promoted, a.summaries = s.Recent, s.Critical, s.Promoted, s.Summaries
	for _, st := range a.alerts {
		a.seq = max(a.seq, st.Seq)
	}
}

// clampState makes a loaded state safe to use: no nil maps or entries, and
// no time later than now + 1 min (a clock that was ahead, a hand edit).
func clampState(s *savedState, now time.Time) {
	limit := now.Add(time.Minute)
	clamp := func(t *time.Time) {
		if t.After(limit) {
			*t = now
		}
	}
	s.Sources = orEmpty(s.Sources)
	s.Patterns = orEmpty(s.Patterns)
	s.Alerts = orEmpty(s.Alerts)
	s.Digests = orEmpty(s.Digests)
	s.Recent = orEmpty(s.Recent)
	s.Critical = orEmpty(s.Critical)
	s.Promoted = orEmpty(s.Promoted)
	s.Summaries = orEmpty(s.Summaries)
	for k, v := range s.Sources {
		if v == nil {
			delete(s.Sources, k)
			continue
		}
		clamp(&v.First)
		clamp(&v.Last)
	}
	hour, minute := now.Unix()/3600, now.Unix()/60
	for k, v := range s.Patterns {
		if v == nil {
			delete(s.Patterns, k)
			continue
		}
		for _, t := range []*time.Time{&v.First, &v.Last, &v.MaxSevAt, &v.SpikeAt} {
			clamp(t)
		}
		v.Hour, v.Minute = min(v.Hour, hour), min(v.Minute, minute)
	}
	for k, v := range s.Alerts {
		if v == nil {
			delete(s.Alerts, k)
			continue
		}
		clamp(&v.LastSent)
		clamp(&v.Updated)
	}
	for k, b := range s.Digests {
		if b == nil {
			delete(s.Digests, k)
			continue
		}
		clamp(&b.Opened)
		if b.Interval <= 0 {
			b.Interval = defaultSmartDigest
		}
		// Due is in the future by design, but never by more than an interval.
		if longest := now.Add(time.Duration(b.Interval) * time.Minute); b.Due.After(longest.Add(time.Minute)) {
			b.Due = longest
		}
		b.Entries = orEmpty(b.Entries)
		for fp, e := range b.Entries {
			if e == nil {
				delete(b.Entries, fp)
				continue
			}
			for _, t := range []*time.Time{&e.First, &e.Last, &e.FirstSeen, &e.AlertedAt} {
				clamp(t)
			}
		}
		for fp, o := range b.Other {
			if o == nil {
				delete(b.Other, fp)
			}
		}
	}
	for _, m := range []map[string][]time.Time{s.Recent, s.Critical} {
		for k, ts := range m {
			for i := range ts {
				clamp(&ts[i])
			}
			slices.SortFunc(ts, time.Time.Compare)
			m[k] = ts
		}
	}
	for _, m := range []map[string]time.Time{s.Promoted, s.Summaries} {
		for k, t := range m {
			clamp(&t)
			m[k] = t
		}
	}
}

func orEmpty[M ~map[K]V, K comparable, V any](m M) M {
	if m == nil {
		return M{}
	}
	return m
}

// SaveState writes alert state to Options.StatePath (it is a no-op
// without one). Run saves every minute when something changed, and Drain
// at shutdown.
func (d *Dispatcher) SaveState() error {
	if d.statePath == "" {
		return nil
	}
	d.saveMu.Lock()
	defer d.saveMu.Unlock()
	d.amu.Lock()
	s := d.al.snapshotState(d.clock())
	d.al.dirty = false
	d.amu.Unlock()
	data, err := json.Marshal(s)
	if err == nil {
		err = atomicfile.Write(d.statePath, data, 0o600)
	}
	d.stmu.Lock()
	failing := d.saveErr != ""
	if err != nil {
		d.saveErr = err.Error()
	} else {
		d.saveErr, d.savedAt = "", s.SavedAt
	}
	d.stmu.Unlock()
	if err != nil {
		d.StateSaveErrors.Add(1)
		d.amu.Lock()
		d.al.dirty = true
		d.amu.Unlock()
		if !failing {
			d.log.Warn("could not save alert state", "file", d.statePath, "err", err)
		}
		return err
	}
	if failing {
		d.log.Info("alert state saved again", "file", d.statePath)
	}
	return nil
}

// snapshotState deep-copies what is persisted; amu is held. Marshalling
// happens after amu is released.
func (a *alerting) snapshotState(now time.Time) *savedState {
	s := &savedState{
		Version: stateVersion, PatternVersion: classify.PatternVersion, SavedAt: now,
		Sources: make(map[string]*sourceState, len(a.sources)), Patterns: make(map[string]*patternState, len(a.patterns)),
		Alerts: make(map[string]*alertState, len(a.alerts)), Digests: make(map[string]*digestBuffer, len(a.digests)),
		Recent: map[string][]time.Time{}, Critical: map[string][]time.Time{},
		Promoted: maps.Clone(a.promoted), Summaries: maps.Clone(a.summaries),
	}
	for k, v := range a.sources {
		cp := *v
		s.Sources[k] = &cp
	}
	for k, v := range a.patterns {
		cp := *v
		s.Patterns[k] = &cp
	}
	for k, v := range a.alerts {
		cp := *v
		s.Alerts[k] = &cp
	}
	for k, b := range a.digests {
		s.Digests[k] = b.clone()
	}
	for k, ts := range a.recent {
		s.Recent[k] = slices.Clone(ts)
	}
	for k, ts := range a.critical {
		s.Critical[k] = slices.Clone(ts)
	}
	return s
}

func (b *digestBuffer) clone() *digestBuffer {
	cp := *b
	cp.Routes = slices.Clone(b.Routes)
	cp.Entries = make(map[string]*DigestEntry, len(b.Entries))
	for fp, e := range b.Entries {
		ec := *e
		cp.Entries[fp] = &ec
	}
	if b.Other != nil {
		cp.Other = make(map[string]*otherCount, len(b.Other))
		for fp, o := range b.Other {
			oc := *o
			cp.Other[fp] = &oc
		}
	}
	return &cp
}
