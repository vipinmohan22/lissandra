package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine"
	"github.com/vipinmohan22/lissandra/internal/engine/enginetest"
	"github.com/vipinmohan22/lissandra/internal/events"
	"github.com/vipinmohan22/lissandra/internal/model"
	"github.com/vipinmohan22/lissandra/internal/notify"
)

// A storm of one new pattern: one request answers them all, yet WouldWait
// counts every queued copy as a separate request and sheds most of them.
func TestSamePatternBurstIsNotShed(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	dflt := fake.Decide
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		time.Sleep(500 * time.Millisecond) // ~ english on 4 CPU cores
		return dflt(state, q)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := config.Defaults()
	c.Settings.Engine.URL = fake.URL // timeout_seconds 30, max_in_flight 1
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	store := events.NewStore(5000)
	d := notify.NewDispatcher(log)
	laya := engine.New(log)
	defer laya.Close()
	laya.Configure(c)
	time.Sleep(200 * time.Millisecond) // health
	p := New(10000, store, d, laya)
	p.Configure(c)
	d.Configure(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, 1)
	go p.Run(ctx, 4, 16) // main.go defaults
	p.Submit([]model.Event{{ID: "warm", SourceID: "s", Message: "ERROR warm up database"}})
	for p.Processed.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	var evs []model.Event
	for i := 0; i < 300; i++ {
		evs = append(evs, model.Event{ID: fmt.Sprint(i), SourceID: "s", Message: "ERROR payments upstream said 418 to the database client"})
	}
	start := time.Now()
	p.Submit(evs)
	for p.Processed.Load() < 301 {
		time.Sleep(10 * time.Millisecond)
	}
	answered := 0
	for _, e := range store.Find(events.Query{Limit: 1000}) {
		if e.Engine != nil && e.ID != "warm" {
			answered++
		}
	}
	st := laya.Status()
	t.Logf("300 identical events in %v: engine requests=%d answered=%d shed=%d skipped=%d", time.Since(start).Round(time.Millisecond), fake.Calls.Load()-1, answered, p.EngineShed.Load(), st.Skipped)
	if p.EngineShed.Load() > 0 {
		t.Errorf("%d events shed although one request (%d made) answered the pattern within a second", p.EngineShed.Load(), fake.Calls.Load()-1)
	}
	// More copies than the engine queue holds still share the request.
	if answered != 300 || st.Skipped != 0 {
		t.Errorf("%d of 300 answered, %d skipped", answered, st.Skipped)
	}
}
