package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// 32. A clock that was ahead and is corrected moves alert state with it:
// summaries, digests and limits don't wait for the clock to catch up.
func TestClockStepBack(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	e.clock.Set(t0.Add(time.Hour)) // an hour ahead, for one reading
	e.d.Gauges()
	held := 0
	for i := 0; i < 25; i++ {
		if d := decisionFor(t, e.at(t0.Add(time.Duration(i)*time.Second), e.ev(fmt.Sprint("src-", i), "app", fmt.Sprint("crit-", i), model.SeverityCritical)), "slack"); d.Action == ActionHeld {
			held++
			if i == 24 && d.Reason.Text != "Held back: this channel had 20 critical alerts in the last 10 min. In the summary due 09:01 UTC, within a minute." {
				t.Errorf("held: %q", d.Reason.Text)
			}
		}
	}
	e.pump()
	if held != 5 {
		t.Fatalf("held %d", held)
	}
	for at := t0.Add(30 * time.Second); !at.After(t0.Add(2 * time.Minute)); at = at.Add(flushTick) {
		e.flush(at)
	}
	if ds := e.sinks["slack"].digests(); len(ds) != 1 || ds[0].Digest.CriticalHeld != 5 {
		t.Fatalf("the summary didn't go out within a minute: %d digests", len(ds))
	}
	wantDecision(t, e.at(t0.Add(15*time.Minute), e.ev("src-new", "app", "crit-new", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
}

// 33. A spike inside the window doesn't page PagerDuty again: it gets
// criticals and severity rises only.
func TestSpikeDoesntRepage(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{pager("pd"), chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "pd", "slack")},
	})
	for k := 0; k < 15; k++ { // 3 an hour
		e.at(t0.Add(time.Duration(k)*20*time.Minute), e.ev("db", "app", "p", model.SeverityCritical))
	}
	paged := t0.Add(5 * time.Hour)
	if d := decisionFor(t, e.at(paged, e.ev("db", "app", "p", model.SeverityCritical)), "pd"); d.Action != ActionSent {
		t.Fatalf("not paged: %+v", d)
	}
	var pd, slack []Decision
	for i := 0; i < 30; i++ {
		decs := e.at(paged.Add(20*time.Minute+time.Duration(i)*8*time.Second), e.ev("db", "app", "p", model.SeverityCritical))
		pd, slack = append(pd, decisionFor(t, decs, "pd")), append(slack, decisionFor(t, decs, "slack"))
	}
	for i, d := range pd {
		if d.Action == ActionSent {
			t.Errorf("event %d paged again: %q", i, d.Reason.Text)
		}
	}
	if d := slack[19]; d.Action != ActionSent || d.Reason.Code != ReasonSpike {
		t.Errorf("slack: %+v", d)
	}
}

// 34. Many distinct warnings evict patterns that never reached an error
// first: a known error isn't announced as new.
func TestWarningsDontEvictErrorHistory(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	wantDecision(t, e.at(t0, e.ev("api", "app", "known", model.SeverityError)), "slack", ActionSent, ReasonNew)
	for i := 0; i <= maxPatterns; i++ { // no route matches warnings, but they are tracked
		e.at(t0.Add(time.Hour), e.ev("batch", "app", fmt.Sprint("warn-", i), model.SeverityWarning))
	}
	if n := e.d.Gauges().PatternsTracked; n > maxPatterns {
		t.Fatalf("patterns = %d", n)
	}
	wantDecision(t, e.at(t0.Add(8*time.Hour), e.ev("api", "app", "known", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
}

// 35. In a critical storm the summary is the alert for the problems it
// held: their repeats go into the digest instead of a summary (and its
// mention) every minute, and they aren't sent again 10 minutes later.
func TestSummaryIsTheHeldAlert(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	start := t0
	tick := start
	for at := start; at.Before(start.Add(12 * time.Minute)); at = at.Add(10 * time.Second) {
		for ; !tick.After(at); tick = tick.Add(flushTick) {
			e.flush(tick)
		}
		for i := 0; i < 25; i++ {
			e.at(at, e.ev(fmt.Sprint("svc-", i), "app", fmt.Sprint("down-", i), model.SeverityCritical))
		}
		e.pump()
	}
	alerts, digests := e.sinks["slack"].alerts(), e.sinks["slack"].digests()
	if len(alerts) != 20 || len(digests) != 1 {
		t.Fatalf("%d alerts, %d digests", len(alerts), len(digests))
	}
	if m := digests[0]; m.Digest.Reason != ReasonSummary || m.Title() != "[SUMMARY] 5 alerts held back · 5 sources" {
		t.Errorf("summary: %q", m.Title())
	}
}

// 36. A burst at the same time every night (a batch job) is a spike once,
// not every night; one well above it still is.
func TestRoutineBurstStopsSpiking(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	night := func(n, events int) (spikes int) {
		at := time.Date(2026, 10, 19+n, 2, 0, 0, 0, time.UTC)
		for i := 0; i < events; i++ {
			d := decisionFor(t, e.at(at.Add(time.Duration(i)*240*time.Second/time.Duration(events)), e.ev("batch", "app", "job-failed", model.SeverityError)), "slack")
			if d.Reason.Code == ReasonSpike {
				spikes++
			}
		}
		e.pump()
		return spikes
	}
	night(0, 100) // new
	for n := 1; n < 7; n++ {
		if s := night(n, 100); s != 0 {
			t.Errorf("night %d spiked", n+1)
		}
	}
	if s := night(7, 300); s != 1 {
		t.Errorf("a burst 3 times the usual one: %d spikes", s)
	}
}

// 37. After a digest reported repeats, the next alert counts the repeats
// since that digest, and says so.
func TestStillHappeningSinceTheDigest(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	tick := t0
	for at := t0; !at.After(t0.Add(time.Hour)); at = at.Add(2 * time.Minute) {
		for ; !tick.After(at); tick = tick.Add(flushTick) {
			e.flush(tick)
		}
		e.at(at, e.ev("api", "app", "flap", model.SeverityCritical))
		e.pump()
	}
	ds, as := e.sinks["slack"].digests(), e.sinks["slack"].alerts()
	if len(ds) != 1 || len(as) != 2 {
		t.Fatalf("%d digests, %d alerts", len(ds), len(as))
	}
	m := as[1]
	digestAt := fmtClock(ds[0].Digest.To, m.at)
	if m.Suppressed+ds[0].Digest.Events != 29 || m.Reason.Text != fmt.Sprintf("Still happening: %d more since the digest at %s.", m.Suppressed, digestAt) {
		t.Errorf("%d %q", m.Suppressed, m.Reason.Text)
	}
	facts := map[string]string{}
	for _, f := range m.Facts() {
		facts[f.Name] = f.Value
	}
	if facts["Suppressed"] != fmt.Sprintf("%d more since the digest at %s", m.Suppressed, digestAt) {
		t.Errorf("suppressed: %q", facts["Suppressed"])
	}
}

// 38. A digest whose route changed since says which routes filled it.
func TestDigestWhyAfterRouteChange(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("slack: error and above", model.SeverityError, 30, 3600, "slack")},
	})
	e.at(t0, e.ev("api", "app", "a", model.SeverityError))
	e.at(t0.Add(time.Minute), e.ev("api", "app", "a", model.SeverityError)) // a repeat
	e.pump()
	e.cfg.Routes[0].Delivery = config.DeliveryImmediate
	e.d.Configure(e.cfg)
	e.flush(t0.Add(31 * time.Minute))
	ds := e.sinks["slack"].digests()
	if len(ds) != 1 || ds[0].Reason.Text != `Batched every 30 min for "slack: error and above".` {
		t.Fatalf("why: %+v", ds)
	}
	if strings.Contains(ds[0].Reason.Text, `""`) {
		t.Error("an empty route name")
	}
}
