package notify

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// 22. Repeats reported long after the last of them say when that was: the
// problem is back, not still happening. Counts stay exact.
func TestQuietRepeats(t *testing.T) {
	pagerCfg := func() *config.Config {
		return &config.Config{
			Notifiers: []config.Notifier{pager("pd")},
			Routes:    []config.Route{immediateRoute("pd", model.SeverityCritical, 900, "pd")},
		}
	}
	shutdown := func(e *env, at time.Time) model.Event {
		e.clock.Set(at)
		return e.ev("postgres", "database", "pg-fatal", model.SeverityCritical)
	}
	t.Run("two days later on the pager", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerting.json")
		e := newEnv(t, Options{StatePath: path}, pagerCfg())
		wantDecision(t, e.handle(shutdown(e, t0.Add(18*time.Minute))), "pd", ActionSent, ReasonEvery)
		for i := 1; i <= 11; i++ {
			wantDecision(t, e.handle(shutdown(e, t0.Add(18*time.Minute+time.Duration(i)*10*time.Second))), "pd", ActionThrottled, ReasonThrottled)
		}
		e.pump()
		if err := e.d.SaveState(); err != nil {
			t.Fatal(err)
		}
		e = e.restart(t, path) // the newest repeat's time is saved too
		e.handle(shutdown(e, t0.Add(48*time.Hour+30*time.Minute)))
		e.pump()
		as := e.sinks["pd"].alerts()
		if len(as) != 2 {
			t.Fatalf("pages = %d", len(as))
		}
		m := as[1]
		const why = "Happening again: 11 more after the alert at Oct 19 09:18 UTC, the last at Oct 19 09:19 UTC."
		if m.Suppressed != 11 || m.Reason.Code != ReasonRepeat || m.Reason.Text != why || !m.LastRepeat.Equal(t0.Add(19*time.Minute+50*time.Second)) {
			t.Errorf("page: %d %s %q %v", m.Suppressed, m.Reason.Code, m.Reason.Text, m.LastRepeat)
		}
		if m.Headline() != "[CRITICAL] database · postgres · happening again" {
			t.Errorf("headline %q", m.Headline())
		}
		facts := map[string]string{}
		for _, f := range m.Facts() {
			facts[f.Name] = f.Value
		}
		if facts["Suppressed"] != "11 more after the alert at Oct 19 09:18 UTC, the last at Oct 19 09:19 UTC" {
			t.Errorf("suppressed fact %q", facts["Suppressed"])
		}
		if p := StandardPayload(m); p["suppressed"] != 11 || p["last_repeat"] != "2026-10-19T09:19:50Z" {
			t.Errorf("payload: %v %v", p["suppressed"], p["last_repeat"])
		}
	})
	t.Run("recent repeats are still happening", func(t *testing.T) {
		e := newEnv(t, Options{}, pagerCfg())
		e.handle(shutdown(e, t0))
		for i := 1; i <= 11; i++ {
			e.handle(shutdown(e, t0.Add(time.Duration(i)*time.Minute)))
		}
		e.handle(shutdown(e, t0.Add(70*time.Minute))) // the last repeat 59 min ago
		e.pump()
		m := e.sinks["pd"].alerts()[1]
		if m.Reason.Text != "Still happening: 11 more since the alert at 09:00 UTC." || m.Headline() != "[CRITICAL] database · postgres · still happening" {
			t.Errorf("%q %q", m.Reason.Text, m.Headline())
		}
		if p := StandardPayload(e.sinks["pd"].alerts()[0]); p["last_repeat"] != nil {
			t.Errorf("a first alert has no repeats: %v", p["last_repeat"])
		}
	})
	t.Run("from a long digest", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 1440, 3600, "slack")},
		})
		e.handle(shutdown(e, t0))
		for i := 1; i <= 3; i++ {
			e.handle(shutdown(e, t0.Add(time.Duration(i)*time.Minute)))
		}
		if b := e.buffer("slack"); b == nil || !b.Entries["pg-fatal"].LastRepeat.Equal(t0.Add(3*time.Minute)) {
			t.Fatalf("buffer = %+v", b)
		}
		e.handle(shutdown(e, t0.Add(5*time.Hour)))
		e.pump()
		m := e.sinks["slack"].alerts()[1]
		if m.Suppressed != 3 || m.Reason.Text != "Happening again: 3 more after the alert at 09:00 UTC, the last at 09:03 UTC." || e.buffer("slack") != nil {
			t.Errorf("%d %q", m.Suppressed, m.Reason.Text)
		}
	})
}

// 23. A digest entry says when its problem was alerted right away, and
// why, when its events came after that alert below its right-away level.
func TestDigestEntryAlertedBefore(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	wantDecision(t, e.at(t0, e.ev("api", "database", "db", model.SeverityError)), "slack", ActionSent, ReasonNew)
	for i := 1; i <= 4; i++ {
		wantDecision(t, e.at(t0.Add(time.Duration(i)*time.Minute), e.ev("api", "database", "db", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
	}
	wantDecision(t, e.at(t0.Add(5*time.Minute), e.ev("api", "app", "other", model.SeverityError)), "slack", ActionSent, ReasonNew)
	wantDecision(t, e.at(t0.Add(6*time.Minute), e.ev("api", "app", "quiet", model.SeverityError)), "slack", ActionHeld, ReasonSourceLimit)
	e.pump()
	e.flush(t0.Add(31 * time.Minute))
	ds := e.sinks["slack"].digests()
	if len(ds) != 1 {
		t.Fatalf("digests = %d", len(ds))
	}
	var db *DigestEntry
	for i := range ds[0].Digest.Entries {
		if en := &ds[0].Digest.Entries[i]; en.Fingerprint == "db" {
			db = en
		} else if !en.AlertedAt.IsZero() || en.AlertedAs != "" {
			t.Errorf("%s was never alerted: %+v", en.Fingerprint, en)
		}
	}
	if db == nil || db.Count != 4 || db.Repeats != 0 || !db.AlertedAt.Equal(t0) || db.AlertedAs != ReasonNew {
		t.Fatalf("entry = %+v", db)
	}
	if text := ds[0].PlainText(); !strings.Contains(text, "ERROR ×4  api · database  09:01–09:04 UTC  ALERTED at 09:00 as new\n") {
		t.Errorf("plain text:\n%s", text)
	}
}
