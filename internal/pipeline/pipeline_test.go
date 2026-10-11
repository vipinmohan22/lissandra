package pipeline

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/engine"
	"github.com/vipinmohan22/layalogs/internal/engine/enginetest"
	"github.com/vipinmohan22/layalogs/internal/events"
	"github.com/vipinmohan22/layalogs/internal/model"
	"github.com/vipinmohan22/layalogs/internal/notify"
)

func run(t *testing.T, url string, wait time.Duration) (*Pipeline, *events.Store, *engine.Classifier) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := config.Defaults()
	c.Settings.Engine.URL = url
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	store := events.NewStore(100)
	d := notify.NewDispatcher(log)
	laya := engine.New(log)
	laya.Configure(c)
	p := New(100, store, d, laya)
	p.Configure(c)
	d.Configure(c)
	if wait > 0 {
		p.MaxEngineWait = wait
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx, 1)
	go p.Run(ctx, 2, 2)
	return p, store, laya
}

func waitFor(t *testing.T, store *events.Store, n int) []model.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		evs := store.Find(events.Query{Limit: 100})
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d events stored", len(evs), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func byMessage(evs []model.Event) map[string]model.Event {
	m := map[string]model.Event{}
	for _, e := range evs {
		m[e.Message] = e
	}
	return m
}

func TestRulesFirstThenEngine(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	p, store, _ := run(t, fake.URL, 0)
	p.Submit([]model.Event{
		{ID: "1", SourceID: "s", Message: "ERROR java.lang.OutOfMemoryError: Java heap space"}, // a default rule: resource
		{ID: "2", SourceID: "s", Message: "ERROR payments upstream said 418 to the database client"},
		{ID: "3", SourceID: "s", Message: "INFO all good"}, // below min severity
	})
	got := byMessage(waitFor(t, store, 3))
	if e := got["ERROR java.lang.OutOfMemoryError: Java heap space"]; e.Category != "resource" || e.Engine != nil {
		t.Errorf("rule-categorized event went to the engine: %+v", e)
	}
	if e := got["ERROR payments upstream said 418 to the database client"]; e.Category != "database" || e.Classifier != engine.ByEngine {
		t.Errorf("engine did not classify what the rules left: %+v", e)
	}
	if e := got["INFO all good"]; e.Engine != nil {
		t.Errorf("info event sent to the engine: %+v", e)
	}
	if n := fake.Calls.Load(); n != 1 {
		t.Errorf("engine calls = %d, want 1", n)
	}
}

func TestEngineDownFallsBackToRules(t *testing.T) {
	fake := enginetest.New()
	url := fake.URL
	fake.Close() // nothing listens
	p, store, laya := run(t, url, 0)
	p.Submit([]model.Event{{ID: "1", SourceID: "s", Message: "ERROR weird failure one"}})
	e := waitFor(t, store, 1)[0]
	if e.Category != model.Uncategorized || e.Severity != model.SeverityError || e.Engine != nil {
		t.Errorf("event should keep the rules' result: %+v", e)
	}
	// Paused now: the next eligible events skip the engine at once.
	p.Submit([]model.Event{{ID: "2", SourceID: "s", Message: "ERROR weird failure two"}})
	waitFor(t, store, 2)
	if laya.Skipped.Load() == 0 || laya.Status().PausedUntil.IsZero() {
		t.Errorf("status %+v", laya.Status())
	}
}

func TestSlowEngineDoesNotHoldEventsBack(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	block := make(chan struct{})
	defer close(block)
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		<-block
		return enginetest.Answer{Choice: q.Labels[0], Probs: map[string]float64{q.Labels[0]: 1}}
	}
	p, store, _ := run(t, fake.URL, 100*time.Millisecond)
	// MaxInFlight is 4: the first four block in the engine; the rest wait
	// in the queue, then go on without it.
	var evs []model.Event
	for i := 0; i < 8; i++ {
		evs = append(evs, model.Event{ID: string(rune('a' + i)), SourceID: "s", Message: "ERROR distinct failure " + string(rune('a'+i))})
	}
	p.Submit(evs)
	waitFor(t, store, 4)
	if p.EngineStale.Load() == 0 {
		t.Error("queued events should time out of the engine queue")
	}
}
