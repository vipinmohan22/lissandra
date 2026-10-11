// Package pipeline moves events from ingest through rule classification
// and the Laya engine into the event store and notification dispatcher.
package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/layalogs/internal/classify"
	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/engine"
	"github.com/vipinmohan22/layalogs/internal/events"
	"github.com/vipinmohan22/layalogs/internal/model"
	"github.com/vipinmohan22/layalogs/internal/notify"
)

// ErrQueueFull is returned when the pipeline cannot accept more events.
var ErrQueueFull = errors.New("pipeline queue full")

// engineQueueSize bounds events waiting for the Laya engine; when it is
// full, events go on with the rules' classification, except those whose
// answer is already being asked for: up to maxSharers of them wait for it
// outside the queue (a burst of one new error shares one request).
const (
	engineQueueSize = 256
	maxSharers      = 4096
)

// queued is an event waiting for the Laya engine.
type queued struct {
	ev model.Event
	at time.Time
}

// Pipeline is safe for concurrent use.
type Pipeline struct {
	in          chan model.Event
	engineQueue chan queued
	engine      atomic.Pointer[classify.Engine]

	store      *events.Store
	dispatcher *notify.Dispatcher
	laya       *engine.Classifier
	// MaxEngineWait, when set, overrides how long an event may wait for the
	// Laya engine (queue and request) before it goes on without it; by
	// default it is the engine's configured timeout.
	MaxEngineWait time.Duration

	// engineCtx carries engine requests; Drain cancels it.
	engineCtx    context.Context
	engineCancel context.CancelFunc
	draining     atomic.Bool

	// engineLive counts live events queued for, or being answered by, the
	// engine; file analysis leaves the engine to them.
	engineLive atomic.Int64
	sharers    chan struct{} // slots for events waiting on a request already open

	Received, Processed, Dropped, Rejected, EngineQueued, EngineShed atomic.Uint64
}

// New builds a pipeline with a queue of queueSize events. laya may be nil.
func New(queueSize int, store *events.Store, d *notify.Dispatcher, laya *engine.Classifier) *Pipeline {
	p := &Pipeline{
		in:          make(chan model.Event, queueSize),
		engineQueue: make(chan queued, engineQueueSize),
		sharers:     make(chan struct{}, maxSharers),
		store:       store,
		dispatcher:  d,
		laya:        laya,
	}
	p.engineCtx, p.engineCancel = context.WithCancel(context.Background())
	p.engine.Store(classify.New(nil))
	return p
}

// Configure swaps in the rule set from c.
func (p *Pipeline) Configure(c *config.Config) {
	p.engine.Store(classify.New(c.Rules))
}

// Engine returns the active rule engine.
func (p *Pipeline) Engine() *classify.Engine { return p.engine.Load() }

// Submit enqueues events without blocking. It returns how many were
// accepted and ErrQueueFull if any were rejected.
func (p *Pipeline) Submit(evs []model.Event) (int, error) {
	for i, ev := range evs {
		select {
		case p.in <- ev:
			p.Received.Add(1)
		default:
			p.Rejected.Add(uint64(len(evs) - i))
			return i, ErrQueueFull
		}
	}
	return len(evs), nil
}

// SubmitWait enqueues events, blocking while the queue is full.
func (p *Pipeline) SubmitWait(ctx context.Context, evs []model.Event) error {
	for _, ev := range evs {
		select {
		case p.in <- ev:
			p.Received.Add(1)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// QueueDepth reports events waiting for classification.
func (p *Pipeline) QueueDepth() int { return len(p.in) }

// Busy reports that live ingest is backing up (queue over a quarter full);
// background work such as file analysis should pause.
func (p *Pipeline) Busy() bool { return len(p.in) > cap(p.in)/4 }

// EngineQueueDepth reports events waiting for the Laya engine.
func (p *Pipeline) EngineQueueDepth() int { return len(p.engineQueue) }

// Run processes events until ctx is cancelled, with engineWorkers
// goroutines waiting on the Laya engine.
func (p *Pipeline) Run(ctx context.Context, workers, engineWorkers int) {
	stop := context.AfterFunc(ctx, p.engineCancel)
	defer stop()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ev := <-p.in:
					p.process(ev)
				}
			}
		}()
	}
	for i := 0; i < engineWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case q := <-p.engineQueue:
					p.enrich(q)
				}
			}
		}()
	}
	wg.Wait()
}

// Drain finishes every event already accepted: engine requests are
// cancelled and waiting events go on with the rules' result. Stop ingest
// first. It returns when nothing is left, or when ctx ends.
func (p *Pipeline) Drain(ctx context.Context) error {
	p.draining.Store(true)
	p.engineCancel()
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		if len(p.in) == 0 && len(p.engineQueue) == 0 && p.Received.Load() == p.Processed.Load()+p.Dropped.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// maxEngineWait is how long an event may wait for the engine.
func (p *Pipeline) maxEngineWait() time.Duration {
	if p.MaxEngineWait > 0 {
		return p.MaxEngineWait
	}
	return p.laya.MaxWait()
}

// enrich asks the Laya engine about a queued event, then records it. The
// time from queueing to the answer is bounded, so a slow or hung engine
// delays an event (and its alert) by at most the engine wait.
func (p *Pipeline) enrich(q queued) {
	deadline := q.at.Add(p.maxEngineWait())
	if time.Now().After(deadline) || p.draining.Load() {
		// Out of time: an answer that arrived meanwhile is still used.
		if !p.laya.ApplyCached(&q.ev) {
			p.laya.Skipped.Add(1)
		}
	} else {
		ctx, cancel := context.WithDeadline(p.engineCtx, deadline)
		p.laya.Enrich(ctx, &q.ev)
		cancel()
	}
	p.engineLive.Add(-1)
	p.finish(q.ev)
}

// ClassifyOffline classifies ev for file analysis: rules plus any cached
// Laya engine answer for its pattern, but it never calls the engine, stores
// the event or notifies. It returns false when a rule drops the event.
func (p *Pipeline) ClassifyOffline(rules *classify.Engine, ev *model.Event) bool {
	if !rules.Classify(ev) {
		return false
	}
	if p.laya != nil && p.laya.Eligible(ev) {
		p.laya.ApplyCachedQuiet(ev)
	}
	return true
}

// ClassifyFile classifies ev for file analysis: rules, then the Laya engine
// for what they leave. A cached answer is used when there is one; otherwise
// the engine is asked while calls (shared by the analysis) remain and the
// engine has a slot no live event needs, so file analysis doesn't delay
// live classification. It never stores the event or notifies. It returns
// false when a rule drops the event.
func (p *Pipeline) ClassifyFile(ctx context.Context, rules *classify.Engine, ev *model.Event, calls *atomic.Int64) bool {
	if !rules.Classify(ev) {
		return false
	}
	if p.laya == nil || !p.laya.Eligible(ev) || p.laya.ApplyCachedQuiet(ev) {
		return true
	}
	if p.engineLive.Load() >= int64(p.laya.Capacity()) || !p.laya.Available(ev) || calls.Add(-1) < 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, p.laya.MaxWait())
	defer cancel()
	p.laya.EnrichFile(ctx, ev)
	return true
}

func (p *Pipeline) process(ev model.Event) {
	if !p.engine.Load().Classify(&ev) {
		p.Dropped.Add(1)
		return
	}
	// While draining, cached answers are still used; nothing new is asked.
	if p.laya != nil && p.laya.Eligible(&ev) && !p.laya.ApplyCached(&ev) {
		switch {
		case p.draining.Load() || !p.laya.Available(&ev):
			p.laya.Skipped.Add(1)
		case !p.laya.Pending(&ev) && p.laya.WouldWait(len(p.engineQueue)) > p.maxEngineWait():
			// (An event whose pattern is already being asked about will
			// share that answer, so it is never shed.)
			// It would time out in the queue anyway: don't hold it.
			p.EngineShed.Add(1)
			p.laya.Skipped.Add(1)
		default:
			p.engineLive.Add(1)
			q := queued{ev, time.Now()}
			select {
			case p.engineQueue <- q:
				p.EngineQueued.Add(1)
				return
			default:
			}
			// The queue is full. An event whose answer is already being
			// asked for costs no request: let it wait for that one.
			if p.laya.Pending(&ev) {
				select {
				case p.sharers <- struct{}{}:
					p.EngineQueued.Add(1)
					go func() {
						defer func() { <-p.sharers }()
						p.enrich(q)
					}()
					return
				default:
				}
			}
			p.engineLive.Add(-1) // don't hold the event
			p.laya.Skipped.Add(1)
		}
	}
	p.finish(ev)
}

// finish records ev and hands it to the dispatcher. It is counted as
// processed last, so Drain doesn't return before its alert is queued.
func (p *Pipeline) finish(ev model.Event) {
	p.store.Add(ev)
	p.dispatcher.Handle(ev)
	p.Processed.Add(1)
}
