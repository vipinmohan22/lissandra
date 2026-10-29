package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// 24. A summary or an alert that fails to deliver goes out again within a
// minute for criticals, not with the next regular digest.
func TestFailedDeliveryComesBackSoon(t *testing.T) {
	t.Run("summary of held criticals", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("email")},
			Routes:    []config.Route{smartRoute("r", model.SeverityWarning, 60, 3600, "email")},
		})
		held := 0
		for i := 0; i < 25; i++ {
			if d := decisionFor(t, e.at(t0.Add(time.Duration(i)*time.Second), e.ev(fmt.Sprint("src-", i), "app", fmt.Sprint("crit-", i), model.SeverityCritical)), "email"); d.Action == ActionHeld {
				held++
			}
		}
		e.pump()
		b := e.buffer("email")
		if held != 5 || b == nil || !b.Summary {
			t.Fatalf("held %d, buffer %+v", held, b)
		}
		failedAt := b.Due.Add(10 * time.Second) // 3 attempts
		e.clock.Set(b.Due)
		e.d.flushDue(b.Due)
		e.clock.Set(failedAt)
		e.pumpErr(&HTTPError{Status: 503})
		b = e.buffer("email")
		if b == nil || !b.Summary || b.CriticalHeld != 5 || b.Due.After(failedAt.Add(time.Minute)) {
			t.Fatalf("merged back: %+v", b)
		}
		e.flush(b.Due)
		if ds := e.sinks["email"].digests(); len(ds) != 1 || ds[0].Digest.Reason != ReasonSummary || ds[0].Digest.CriticalHeld != 5 {
			t.Fatalf("the held criticals weren't sent within a minute of the failure: %+v", ds)
		}
	})
	t.Run("one-off critical", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
		})
		wantDecision(t, e.at(t0, e.ev("payments", "app", "payment-down", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
		failedAt := t0.Add(10 * time.Second)
		e.clock.Set(failedAt)
		e.pumpErr(&HTTPError{Status: 503})
		b := e.buffer("slack")
		if b == nil || !b.Summary || b.CriticalHeld != 1 || b.Held != 1 || !b.Due.Equal(failedAt.Add(time.Minute)) {
			t.Fatalf("re-filed: %+v", b)
		}
		if en := b.Entries["payment-down"]; en == nil || en.Undelivered != 1 || en.HeldBy != ReasonUndelivered || en.Held != ReasonSeverity {
			t.Fatalf("entry: %+v", en)
		}
		e.flush(b.Due)
		ds := e.sinks["slack"].digests()
		if len(ds) != 1 {
			t.Fatalf("digests = %d", len(ds))
		}
		m := ds[0]
		if m.Reason.Text != "Sent again: right-away alerts that failed to deliver." || m.Title() != "[SUMMARY] 1 alert held back · payments" {
			t.Errorf("summary: %q %q", m.Title(), m.Reason.Text)
		}
		blocks := (&slack{s: map[string]string{"mention": "<!here>"}}).digestBlocks(m)
		if head := blocks[0]["text"].(map[string]any)["text"].(string); !strings.HasPrefix(head, "<!here> ") {
			t.Errorf("a critical sent again without the mention: %q", head)
		}
		// The summary was its alert: a repeat now goes into the digest.
		wantDecision(t, e.at(b.Due.Add(time.Minute), e.ev("payments", "app", "payment-down", model.SeverityCritical)), "slack", ActionThrottled, ReasonThrottled)
	})
	t.Run("new error", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
		})
		wantDecision(t, e.at(t0, e.ev("api", "app", "a", model.SeverityError)), "slack", ActionSent, ReasonNew)
		e.pumpErr(&HTTPError{Status: 429})
		if b := e.buffer("slack"); b == nil || !b.Summary || b.CriticalHeld != 0 || !b.Due.Equal(t0.Add(summaryDelay)) {
			t.Fatalf("re-filed: %+v", b)
		}
		e.flush(t0.Add(summaryDelay))
		if ds := e.sinks["slack"].digests(); len(ds) != 1 || ds[0].Digest.Events != 1 {
			t.Fatalf("digests: %+v", ds)
		}
	})
}

// 25. Two alerts for one problem that both fail leave the window of the
// last alert that was delivered, whichever fails first.
func TestDoubleFailureRestoresDeliveredWindow(t *testing.T) {
	for _, order := range []string{"earlier first", "later first"} {
		t.Run(order, func(t *testing.T) {
			e := newEnv(t, Options{}, &config.Config{
				Notifiers: []config.Notifier{chat("ops")},
				Routes:    []config.Route{immediateRoute("ops", model.SeverityWarning, 3600, "ops")},
			})
			wantDecision(t, e.at(t0, e.ev("api", "app", "p", model.SeverityWarning)), "ops", ActionSent, ReasonEvery)
			e.pump()
			wantDecision(t, e.at(t0.Add(10*time.Minute), e.ev("api", "app", "p", model.SeverityError)), "ops", ActionSent, ReasonWorse)
			wantDecision(t, e.at(t0.Add(11*time.Minute), e.ev("api", "app", "p", model.SeverityCritical)), "ops", ActionSent, ReasonWorse)
			first, _ := e.next()
			second, _ := e.next()
			jobs := []job{first, second}
			if order == "later first" {
				jobs = []job{second, first}
			}
			for _, j := range jobs {
				e.d.completed(j.ch, j.msg, &HTTPError{Status: 503})
				e.d.pending.Add(-1)
			}
			if st := e.state("ops", "p"); !st.LastSent.Equal(t0) || st.LastSeverity != model.SeverityWarning {
				t.Fatalf("window: %+v", st)
			}
			wantDecision(t, e.at(t0.Add(12*time.Minute), e.ev("api", "app", "p", model.SeverityWarning)), "ops", ActionThrottled, ReasonThrottled)
			wantDecision(t, e.at(t0.Add(13*time.Minute), e.ev("api", "app", "p", model.SeverityError)), "ops", ActionSent, ReasonWorse)
		})
	}
}

// 26. Alert state saved while alerts wait to be delivered (a hung notifier
// at shutdown, a crash after a save) doesn't count them as sent.
func TestUndeliveredAlertsAreNotSavedAsSent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting.json")
	e := newEnv(t, Options{StatePath: path, Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack"), pager("pd")},
		Routes: []config.Route{
			smartRoute("slack", model.SeverityError, 30, 3600, "slack"),
			immediateRoute("pd", model.SeverityCritical, 900, "pd"),
		},
	})
	wantDecision(t, e.at(t0, e.ev("payments", "app", "down", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
	if err := e.d.SaveState(); err != nil { // both alerts are still queued
		t.Fatal(err)
	}
	if st := e.state("slack", "down"); st == nil || !st.LastSent.Equal(t0) {
		t.Fatalf("the window in memory: %+v", st)
	}
	e2 := e.restart(t, path)
	decs := e2.at(t0.Add(5*time.Minute), e2.ev("payments", "app", "down", model.SeverityCritical))
	wantDecision(t, decs, "slack", ActionSent, ReasonSeverity)
	wantDecision(t, decs, "pd", ActionSent, ReasonRepeat) // and it reports the page that never went out
	if b := e2.buffer("slack"); b == nil || b.Entries["down"] == nil || b.Entries["down"].Undelivered != 1 {
		t.Errorf("the undelivered event isn't reported: %+v", b)
	}
	// Delivered, the next save keeps the window.
	e.pump()
	if err := e.d.SaveState(); err != nil {
		t.Fatal(err)
	}
	e3 := e.restart(t, path)
	wantDecision(t, e3.at(t0.Add(6*time.Minute), e3.ev("payments", "app", "down", model.SeverityCritical)), "slack", ActionThrottled, ReasonThrottled)
}

// An alert delivered after a save that caught it in flight (as
// undelivered) is saved as sent by the next minute's save: a crash then
// neither sends it again nor pages again.
func TestDeliveredAfterSaveIsSavedAsSent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting.json")
	e := newEnv(t, Options{StatePath: path, Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack"), pager("pd")},
		Routes: []config.Route{
			smartRoute("slack", model.SeverityError, 30, 3600, "slack"),
			immediateRoute("pd", model.SeverityCritical, 4*3600, "pd"),
		},
	})
	wantDecision(t, e.at(t0, e.ev("payments", "app", "down", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
	if err := e.d.SaveState(); err != nil { // the minute's save, while both alerts are being delivered
		t.Fatal(err)
	}
	e.clock.Set(t0.Add(10 * time.Second)) // delivered after a retry
	if n := e.pump(); n != 2 {
		t.Fatalf("delivered %d", n)
	}
	e.clock.Set(t0.Add(time.Minute))
	e.d.saveChanged()        // the next minute's save
	e2 := e.restart(t, path) // a crash now
	e2.flush(t0.Add(3 * time.Minute))
	if all := e.sinks["slack"].all(); len(all) != 1 {
		t.Fatalf("the delivered alert went out again: %d messages, the last %q", len(all), all[len(all)-1].Reason.Text)
	}
	decs := e2.at(t0.Add(5*time.Minute), e2.ev("payments", "app", "down", model.SeverityCritical))
	wantDecision(t, decs, "slack", ActionThrottled, ReasonThrottled)
	wantDecision(t, decs, "pd", ActionThrottled, ReasonThrottled)
}

// A summary of held alerts that isn't delivered was not their alert: the
// windows it opened close again, so a problem the channel never heard of
// isn't throttled as a repeat of it.
func TestUndeliveredSummaryOpensNoWindows(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("email")},
		Routes:    []config.Route{smartRoute("r", model.SeverityWarning, 60, 3600, "email")},
	})
	for i := 0; i < 25; i++ {
		e.at(t0.Add(time.Duration(i)*time.Second), e.ev(fmt.Sprint("src-", i), "app", fmt.Sprint("crit-", i), model.SeverityCritical))
	}
	e.pump()
	b := e.buffer("email")
	if b == nil || b.CriticalHeld != 5 {
		t.Fatalf("held: %+v", b)
	}
	failedAt := b.Due.Add(10 * time.Second)
	e.clock.Set(b.Due)
	e.d.flushDue(b.Due)
	e.clock.Set(failedAt)
	e.pumpErr(&HTTPError{Status: 429}) // merged back, due again within a minute
	if st := e.state("email", "crit-24"); st != nil && !st.LastSent.IsZero() {
		t.Fatalf("the summary that failed left crit-24's window open: %+v", st)
	}
	// Meanwhile crit-24 is held again, not a repeat of the summary.
	wantDecision(t, e.at(failedAt.Add(5*time.Second), e.ev("src-24", "app", "crit-24", model.SeverityCritical)), "email", ActionHeld, ReasonCriticalLimit)
	b = e.buffer("email")
	e.clock.Set(b.Due)
	e.d.flushDue(b.Due)
	e.clock.Set(b.Due.Add(10 * time.Second))
	e.pumpErr(&HTTPError{Status: 429}) // failed again: lost
	if lost := e.d.LostEvents.Load(); lost != 6 || len(e.sinks["email"].digests()) != 0 {
		t.Fatalf("lost %d", lost)
	}
	// Past the critical limit's window, crit-24 alerts: the channel never got it.
	wantDecision(t, e.at(t0.Add(15*time.Minute), e.ev("src-24", "app", "crit-24", model.SeverityCritical)), "email", ActionSent, ReasonSeverity)
}

// A right-away alert that failed to deliver is counted in the summary as
// undelivered, apart from what flood protection held back, so the UI
// doesn't call it held.
func TestUndeliveredCountedApartFromHeld(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	wantDecision(t, e.at(t0, e.ev("payments", "app", "payment-down", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
	e.pumpErr(&HTTPError{Status: 503})
	counts := func(v any) string {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m struct{ Held, Undelivered int }
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("held %d, undelivered %d", m.Held, m.Undelivered)
	}
	pending := e.d.PendingDigests()
	if len(pending) != 1 || counts(pending[0]) != "held 1, undelivered 1" {
		t.Fatalf("pending: %+v", pending)
	}
	e.flush(t0.Add(2 * time.Minute))
	ds := e.sinks["slack"].digests()
	if len(ds) != 1 || counts(ds[0].Digest) != "held 1, undelivered 1" {
		t.Fatalf("digests: %+v", ds)
	}
}

// A notifier that hangs through Drain's deadline: its alerts are saved as
// not delivered.
func TestDrainTimeoutSavesUndeliveredAlerts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting.json")
	e := newEnv(t, Options{StatePath: path, Warmup: -1}, stateConfig())
	release := make(chan struct{})
	e.sinks["slack"].fail = func(*Message) error { <-release; return context.Canceled }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.d.Run(ctx, 1); close(done) }()
	for i := 0; i < 3; i++ { // one being delivered, two queued
		wantDecision(t, e.at(t0, e.ev("payments", "app", fmt.Sprint("down-", i), model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err := e.d.Drain(dctx)
	dcancel()
	close(release)
	cancel()
	<-done
	if err == nil {
		t.Fatal("Drain didn't time out")
	}
	e2 := e.restart(t, path)
	for i := 0; i < 3; i++ {
		wantDecision(t, e2.at(t0.Add(5*time.Minute), e2.ev("payments", "app", fmt.Sprint("down-", i), model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
	}
}

// 27. A digest delivered is saved at the next tick, so a crash right after
// it doesn't send it again from the last minute's save.
func TestDeliveredDigestSavedSoon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting.json")
	e := newEnv(t, Options{StatePath: path}, stateConfig())
	for i := 0; i < 4; i++ {
		e.at(t0.Add(time.Duration(i)*time.Second), e.ev("api", "app", "a", model.SeverityError))
	}
	if err := e.d.SaveState(); err != nil { // the minute's save
		t.Fatal(err)
	}
	due := t0.Add(30 * time.Minute)
	e.clock.Set(due)
	e.d.tick(due)
	e.pump() // delivered
	e.d.tick(due.Add(flushTick))
	if s := readState(t, path); len(s.Digests) != 0 {
		t.Fatalf("the delivered digest is still saved: %+v", s.Digests)
	}
	e2 := e.restart(t, path) // a crash now
	e2.flush(due.Add(time.Hour))
	if ds := e.sinks["slack"].digests(); len(ds) != 1 {
		t.Errorf("digests = %d", len(ds))
	}
}

// 28. Repeats counted for the next alert, dropped when message patterns
// change, are counted as lost.
func TestPatternVersionCountsLostRepeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting.json")
	e := newEnv(t, Options{StatePath: path}, &config.Config{
		Notifiers: []config.Notifier{pager("pd")},
		Routes:    []config.Route{immediateRoute("pd", model.SeverityCritical, 900, "pd")},
	})
	for i := 0; i < 8; i++ {
		e.at(t0.Add(time.Duration(i)*time.Second), e.ev("db", "app", "x", model.SeverityCritical))
	}
	e.pump()
	if err := e.d.SaveState(); err != nil {
		t.Fatal(err)
	}
	s := readState(t, path)
	s.PatternVersion--
	writeState(t, path, s)
	e2 := e.restart(t, path)
	if n := e2.d.LostEvents.Load(); n != 7 {
		t.Errorf("lost = %d, want 7", n)
	}
}

// 29. A problem merged back from a failed digest is listed once, not also
// counted among the others.
func TestMergeBackOnePlacePerProblem(t *testing.T) {
	e := newEnv(t, Options{}, stateConfig())
	for i := 0; i <= maxDigestEntries; i++ {
		e.at(t0, e.ev("api", "app", fmt.Sprintf("fp-%03d", i), model.SeverityError))
	}
	last := fmt.Sprintf("fp-%03d", maxDigestEntries)
	if b := e.buffer("slack"); b.Other[last] == nil {
		t.Fatal("the 201st problem isn't among the others")
	}
	due := t0.Add(30 * time.Minute)
	e.clock.Set(due)
	e.d.flushDue(due)
	e.at(due.Add(time.Second), e.ev("api", "app", last, model.SeverityError))
	e.pumpErr(&HTTPError{Status: 503})
	b := e.buffer("slack")
	if b.Entries[last] == nil || b.Other[last] != nil || b.Entries[last].Count != 2 || b.Events != maxDigestEntries+2 {
		t.Fatalf("entry %+v, other %+v, events %d", b.Entries[last], b.Other[last], b.Events)
	}
	e.flush(b.Due)
	if dg := e.sinks["slack"].digests()[0].Digest; dg.Patterns != maxDigestEntries+1 {
		t.Errorf("patterns = %d", dg.Patterns)
	}
}

// 30. A digest's outcome stays known after its delivery record left the
// ring of 500.
func TestDigestOutcomeOutlivesItsRecord(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack"), chat("ops")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack"), immediateRoute("ops", model.SeverityError, 0, "ops")},
	})
	e.at(t0, e.ev("api", "app", "a", model.SeverityWarning)) // matches no route
	e.at(t0, e.ev("api", "app", "a", model.SeverityError))
	e.at(t0, e.ev("api", "app", "a", model.SeverityError))
	id, err := e.d.FlushDigest("slack")
	if err != nil {
		t.Fatal(err)
	}
	e.pump()
	for i := 0; i < maxDeliveries+20; i++ {
		e.at(t0.Add(time.Minute), e.ev("api", "app", "b", model.SeverityError))
	}
	e.pump()
	del, dg, ok := e.d.DeliveryDetail(id)
	if !ok || dg == nil || del.Status != "sent" || del.NotifierID != "slack" || del.NotifierName != "slack" || del.Attempts != 1 {
		t.Errorf("detail: %+v (%v)", del, ok)
	}
}

// 31. A disabled notifier's digest (and its held criticals) waits for it
// to be enabled again, a week at most; "Send now" says why it can't send
// and keeps it; a removed notifier's digest is discarded when due.
func TestDisabledNotifierKeepsDigest(t *testing.T) {
	setEnabled := func(e *env, id string, on bool) {
		for i := range e.cfg.Notifiers {
			if e.cfg.Notifiers[i].ID == id {
				e.cfg.Notifiers[i].Enabled = on
			}
		}
		e.d.Configure(e.cfg)
	}
	t.Run("held criticals", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
		})
		for i := 0; i < 22; i++ {
			e.at(t0.Add(time.Duration(i)*time.Second), e.ev(fmt.Sprint("src-", i), "app", fmt.Sprint("crit-", i), model.SeverityCritical))
		}
		e.pump()
		setEnabled(e, "slack", false) // rotating its webhook
		pd := e.d.PendingDigests()
		if len(pd) != 1 || !pd[0].Disabled || pd[0].NotifierName != "slack" || pd[0].Held != 2 {
			t.Fatalf("pending: %+v", pd)
		}
		if _, err := e.d.FlushDigest("slack"); !errors.Is(err, ErrNotifierDisabled) {
			t.Errorf("send now: %v", err)
		}
		e.flush(t0.Add(2 * time.Minute))
		if e.buffer("slack") == nil || e.d.LostEvents.Load() != 0 || e.d.DigestsDiscarded.Load() != 0 {
			t.Fatal("the digest of a disabled notifier was discarded")
		}
		setEnabled(e, "slack", true)
		e.flush(t0.Add(2*time.Minute + flushTick))
		if ds := e.sinks["slack"].digests(); len(ds) != 1 || ds[0].Digest.CriticalHeld != 2 {
			t.Errorf("after enabling it again: %+v", ds)
		}
	})
	t.Run("a week", func(t *testing.T) {
		e := newEnv(t, Options{}, stateConfig())
		e.at(t0, e.ev("api", "app", "a", model.SeverityError))
		setEnabled(e, "slack", false)
		e.d.prune(t0.Add(6 * 24 * time.Hour))
		if e.buffer("slack") == nil {
			t.Fatal("discarded within the week")
		}
		e.d.prune(t0.Add(7*24*time.Hour + time.Minute))
		if e.buffer("slack") != nil || e.d.LostEvents.Load() != 1 {
			t.Error("kept after a week")
		}
	})
	t.Run("removed", func(t *testing.T) {
		e := newEnv(t, Options{}, stateConfig())
		e.at(t0, e.ev("api", "app", "a", model.SeverityError))
		e.d.Configure(&config.Config{Routes: e.cfg.Routes})
		if _, err := e.d.FlushDigest("slack"); !errors.Is(err, ErrNotifierDisabled) || e.buffer("slack") == nil {
			t.Errorf("send now: %v", err)
		}
		e.flush(t0.Add(30 * time.Minute))
		if e.buffer("slack") != nil || e.d.LostEvents.Load() != 1 {
			t.Error("a removed notifier's digest wasn't discarded when due")
		}
	})
	t.Run("queue full", func(t *testing.T) {
		e := newEnv(t, Options{QueueSize: 1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
		})
		e.at(t0, e.ev("api", "app", "c", model.SeverityCritical)) // queued
		e.at(t0, e.ev("api", "app", "a", model.SeverityError))
		if _, err := e.d.FlushDigest("slack"); !errors.Is(err, ErrQueueFull) || e.buffer("slack") == nil {
			t.Errorf("send now: %v", err)
		}
	})
}
