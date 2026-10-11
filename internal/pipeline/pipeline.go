// Package pipeline moves events from ingest through classification and
// optional AI enrichment into the event store and notification dispatcher.
package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/vipinmohan22/layalogs/internal/ai"
	"github.com/vipinmohan22/layalogs/internal/classify"
	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/events"
	"github.com/vipinmohan22/layalogs/internal/model"
	"github.com/vipinmohan22/layalogs/internal/notify"
)

// ErrQueueFull is returned when the pipeline cannot accept more events.
var ErrQueueFull = errors.New("pipeline queue full")

// Pipeline is safe for concurrent use.
type Pipeline struct {
	in      chan model.Event
	aiQueue chan model.Event
	engine  atomic.Pointer[classify.Engine]

	store      *events.Store
	dispatcher *notify.Dispatcher
	ai         *ai.Classifier

	Received, Processed, Dropped, Rejected, AIQueued atomic.Uint64
}

// New builds a pipeline with a queue of queueSize events.
func New(queueSize int, store *events.Store, d *notify.Dispatcher, a *ai.Classifier) *Pipeline {
	p := &Pipeline{
		in:         make(chan model.Event, queueSize),
		aiQueue:    make(chan model.Event, 256),
		store:      store,
		dispatcher: d,
		ai:         a,
	}
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

// Run processes events until ctx is cancelled.
func (p *Pipeline) Run(ctx context.Context, workers, aiWorkers int) {
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
	for i := 0; i < aiWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ev := <-p.aiQueue:
					p.ai.Enrich(ctx, &ev)
					p.finish(ev)
				}
			}
		}()
	}
	wg.Wait()
}

// ClassifyOffline classifies ev for file analysis: rules plus any cached AI
// result, but it never calls the AI API, stores the event or notifies. It
// returns false when a rule drops the event.
func (p *Pipeline) ClassifyOffline(engine *classify.Engine, ev *model.Event) bool {
	if !engine.Classify(ev) {
		return false
	}
	if p.ai != nil && p.ai.Eligible(ev) {
		p.ai.ApplyCached(ev)
	}
	return true
}

func (p *Pipeline) process(ev model.Event) {
	if !p.engine.Load().Classify(&ev) {
		p.Dropped.Add(1)
		return
	}
	if p.ai != nil && p.ai.Eligible(&ev) && !p.ai.ApplyCached(&ev) && p.ai.HasBudget() {
		select {
		case p.aiQueue <- ev:
			p.AIQueued.Add(1)
			return
		default: // AI is backed up; don't hold the event.
		}
	}
	p.finish(ev)
}

func (p *Pipeline) finish(ev model.Event) {
	p.Processed.Add(1)
	p.store.Add(ev)
	p.dispatcher.Handle(ev)
}
