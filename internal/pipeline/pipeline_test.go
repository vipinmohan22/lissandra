package pipeline

import (
	"context"
	"fmt"
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
	p, store, laya := run(t, fake.URL, 100*time.Millisecond)
	// One request is with the engine (max_in_flight 1); the rest wait for
	// it, then go on without it, all within the engine wait.
	var evs []model.Event
	for i := 0; i < 8; i++ {
		evs = append(evs, model.Event{ID: string(rune('a' + i)), SourceID: "s", Message: "ERROR distinct failure " + string(rune('a'+i))})
	}
	start := time.Now()
	p.Submit(evs)
	waitFor(t, store, 8)
	if time.Since(start) > 2*time.Second || laya.Skipped.Load() == 0 {
		t.Errorf("held back %v, skipped %d", time.Since(start), laya.Skipped.Load())
	}
}

func TestDrainFinishesEverything(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	block := make(chan struct{})
	defer close(block)
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer { <-block; return enginetest.Answer{} }
	p, store, _ := run(t, fake.URL, time.Minute)
	var evs []model.Event
	for i := 0; i < 6; i++ {
		evs = append(evs, model.Event{ID: fmt.Sprint(i), SourceID: "s", Message: fmt.Sprint("ERROR distinct failure ", i)})
	}
	p.Submit(evs)
	time.Sleep(100 * time.Millisecond) // one is with the engine, the rest queued
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := p.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if time.Since(start) > 2*time.Second || len(store.Find(events.Query{Limit: 100})) != 6 {
		t.Errorf("drain took %v and stored %d of 6", time.Since(start), len(store.Find(events.Query{Limit: 100})))
	}
}

func TestShedsWhenTheEngineCannotKeepUp(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	release := make(chan struct{})
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		select {
		case <-release:
		case <-time.After(300 * time.Millisecond):
		}
		return enginetest.Answer{Choice: q.Labels[0], Probs: map[string]float64{q.Labels[0]: 1}}
	}
	p, store, _ := run(t, fake.URL, time.Second)
	// One answer measures the engine at ~300ms per event.
	p.Submit([]model.Event{{ID: "warm", SourceID: "s", Message: "ERROR warm up"}})
	waitFor(t, store, 1)
	var evs []model.Event
	for i := 0; i < 20; i++ {
		evs = append(evs, model.Event{ID: fmt.Sprint(i), SourceID: "s", Message: fmt.Sprint("ERROR burst failure ", i)})
	}
	p.Submit(evs)
	waitFor(t, store, 21)
	close(release)
	if p.EngineShed.Load() == 0 {
		t.Error("events that would time out in the queue should not be queued")
	}
}
