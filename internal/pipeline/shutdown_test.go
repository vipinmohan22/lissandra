package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine"
	"github.com/vipinmohan22/lissandra/internal/engine/enginetest"
	"github.com/vipinmohan22/lissandra/internal/events"
	"github.com/vipinmohan22/lissandra/internal/model"
	"github.com/vipinmohan22/lissandra/internal/notify"
)

// hook counts webhook deliveries; hookConfig routes every event to it.
func hook(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func hookConfig(t *testing.T, url string) *config.Config {
	t.Helper()
	c := config.Defaults()
	c.Notifiers = []config.Notifier{{ID: "n1", Name: "hook", Type: "webhook", Enabled: true, Settings: map[string]string{"url": url, "method": "POST", "content_type": "application/json"}}}
	c.Routes = []config.Route{{ID: "r1", Name: "all", Enabled: true, MinSeverity: model.SeverityDebug, NotifierIDs: []string{"n1"}}}
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// shutdown follows main.go: drain the pipeline, then the dispatcher, then
// stop both.
func shutdown(t *testing.T, p *Pipeline, d *notify.Dispatcher, stopRun context.CancelFunc, done chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	stopRun()
	<-done
	<-done
}

// Drain must not return until an accepted event's alert is queued, even
// while the store is slow to take it (a text search holds its lock).
func TestShutdownAlertsEveryAcceptedEvent(t *testing.T) {
	srv, hits := hook(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := hookConfig(t, srv.URL)

	store := events.NewStore(1000)
	big := strings.Repeat("x", 16*1024)
	for i := 0; i < 1000; i++ {
		store.Add(model.Event{ID: fmt.Sprint("old", i), Message: big, Fingerprint: fmt.Sprint(i), ReceivedAt: time.Now()})
	}
	stopReader := make(chan struct{})
	var rwg sync.WaitGroup
	rwg.Add(1)
	go func() {
		defer rwg.Done()
		for {
			select {
			case <-stopReader:
				return
			default:
				store.Find(events.Query{Text: "no such text", Limit: 1})
			}
		}
	}()
	defer func() { close(stopReader); rwg.Wait() }()

	for trial := 0; trial < 10; trial++ {
		hits.Store(0)
		d := notify.NewDispatcher(log)
		d.Configure(c)
		p := New(100, store, d, nil)
		p.Configure(c)
		runCtx, stopRun := context.WithCancel(context.Background())
		done := make(chan struct{}, 2)
		go func() { d.Run(runCtx, 4); done <- struct{}{} }()
		go func() { p.Run(runCtx, 4, 1); done <- struct{}{} }()

		n, _ := p.Submit([]model.Event{{ID: fmt.Sprint("e", trial), SourceID: "s", Message: "ERROR shutdown race", ReceivedAt: time.Now()}})
		shutdown(t, p, d, stopRun, done)
		if int(hits.Load()) != n {
			t.Fatalf("trial %d: accepted %d events, delivered %d alerts", trial, n, hits.Load())
		}
	}
}

// Events still queued for the engine at shutdown get an answer the engine
// already gave for their pattern, and so reach their category's route.
func TestDrainUsesCachedEngineAnswers(t *testing.T) {
	srv, hits := hook(t)
	fake := enginetest.New()
	defer fake.Close()
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		return enginetest.Answer{Choice: "database", Probs: map[string]float64{"database": 0.95}}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := hookConfig(t, srv.URL)
	c.Settings.Engine.URL = fake.URL
	c.Routes = []config.Route{{ID: "db", Name: "db team", Enabled: true, MinSeverity: model.SeverityWarning, Categories: []string{"database"}, NotifierIDs: []string{"n1"}}}
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	store := events.NewStore(1000)
	d := notify.NewDispatcher(log)
	d.Configure(c)
	laya := engine.New(log)
	t.Cleanup(laya.Close)
	laya.Configure(c)
	laya.CheckHealth()
	p := New(1000, store, d, laya)
	p.Configure(c)
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go d.Run(runCtx, 2)
	go p.Run(runCtx, 2, 2)

	p.Submit([]model.Event{{ID: "warm", SourceID: "s", Message: "ERROR orders replica 1 is lagging"}})
	waitFor(t, store, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()

	var evs []model.Event
	for i := 0; i < 50; i++ {
		evs = append(evs, model.Event{ID: fmt.Sprint("q", i), SourceID: "s", Message: fmt.Sprintf("ERROR orders replica %d is lagging", i+2)})
	}
	p.Submit(evs) // and SIGTERM at once, with the backlog still queued
	if err := p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load() - before; got != 50 {
		t.Errorf("%d of 50 events of an answered pattern reached the 'database' route", got)
	}
}

// File analysis leaves the engine to a live event waiting for it.
func TestFileAnalysisLeavesEngineToWaitingLiveEvents(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	release := make(chan struct{})
	inner := fake.Decide
	var mu sync.Mutex
	var order []string
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		mu.Lock()
		switch {
		case strings.Contains(state, "filefail"):
			order = append(order, "file")
		case strings.Contains(state, "first"):
			order = append(order, "L1")
		default:
			order = append(order, "L2")
		}
		mu.Unlock()
		<-release
		return inner(state, q)
	}
	p, store, laya := run(t, fake.URL, 0) // max_in_flight 1
	laya.CheckHealth()
	p.Submit([]model.Event{{ID: "l1", SourceID: "s", Message: "ERROR first livefail happened"}})
	time.Sleep(100 * time.Millisecond) // L1 is with the engine
	p.Submit([]model.Event{{ID: "l2", SourceID: "s", Message: "ERROR second livefail happened"}})
	time.Sleep(100 * time.Millisecond) // L2 waits for the slot

	var calls atomic.Int64
	calls.Store(100)
	done := make(chan struct{})
	go func() {
		ev := model.Event{ID: "f", SourceID: "s", Severity: model.SeverityError, Message: "ERROR filefail kqzx happened"}
		p.ClassifyFile(context.Background(), p.Engine(), &ev, &calls)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	<-done
	waitFor(t, store, 2)
	mu.Lock()
	defer mu.Unlock()
	if used := 100 - calls.Load(); used > 0 {
		t.Errorf("file analysis asked the engine while a live event waited (engine order %v)", order)
	}
}
