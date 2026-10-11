package notify

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

const (
	queueSize      = 1000
	maxDeliveries  = 500
	maxAttempts    = 3
	attemptTimeout = 30 * time.Second
)

// Delivery records one notification attempt for the UI.
type Delivery struct {
	ID           string         `json:"id"`
	Time         time.Time      `json:"time"`
	NotifierID   string         `json:"notifier_id"`
	NotifierName string         `json:"notifier_name"`
	NotifierType string         `json:"notifier_type"`
	RouteName    string         `json:"route_name,omitempty"`
	EventID      string         `json:"event_id,omitempty"`
	Severity     model.Severity `json:"severity,omitempty"`
	Title        string         `json:"title"`
	Status       string         `json:"status"` // sent, failed, dropped
	Error        string         `json:"error,omitempty"`
	Attempts     int            `json:"attempts"`
	DurationMs   int64          `json:"duration_ms"`
	Test         bool           `json:"test,omitempty"`
}

type route struct {
	config.Route
	re *regexp.Regexp
}

type channel struct {
	n      config.Notifier
	sender Sender
	err    error
}

type job struct {
	ch  *channel
	msg *Message
}

type throttleState struct {
	last       time.Time
	suppressed int
}

// Dispatcher matches events against routes and delivers them to notifiers
// through a bounded queue with retries.
type Dispatcher struct {
	log *slog.Logger

	mu        sync.RWMutex
	routes    []route
	channels  map[string]*channel
	publicURL string

	tmu      sync.Mutex
	throttle map[string]*throttleState

	queue chan job

	dmu        sync.Mutex
	deliveries []Delivery

	Sent, Failed, Dropped, Throttled atomic.Uint64
}

// NewDispatcher returns an idle dispatcher; call Configure and Run.
func NewDispatcher(log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		log:      log,
		channels: map[string]*channel{},
		throttle: map[string]*throttleState{},
		queue:    make(chan job, queueSize),
	}
}

// Configure swaps in new notifiers and routes.
func (d *Dispatcher) Configure(c *config.Config) {
	channels := make(map[string]*channel, len(c.Notifiers))
	for _, n := range c.Notifiers {
		if !n.Enabled {
			continue
		}
		s, err := Build(n)
		if err != nil {
			d.log.Warn("notifier misconfigured", "notifier", n.Name, "err", err)
		}
		channels[n.ID] = &channel{n: n, sender: s, err: err}
	}
	routes := compileRoutes(c.Routes)
	d.mu.Lock()
	d.channels, d.routes, d.publicURL = channels, routes, c.Settings.PublicURL
	d.mu.Unlock()
}

// compileRoutes prepares the enabled routes for matching.
func compileRoutes(in []config.Route) []route {
	routes := make([]route, 0, len(in))
	for _, r := range in {
		if !r.Enabled {
			continue
		}
		rt := route{Route: r}
		if r.MessageRegex != "" {
			re, err := regexp.Compile(r.MessageRegex)
			if err != nil {
				continue
			}
			rt.re = re
		}
		routes = append(routes, rt)
	}
	return routes
}

func (r *route) matches(ev *model.Event) bool {
	if !ev.Severity.AtLeast(r.MinSeverity) {
		return false
	}
	if len(r.Categories) > 0 && !slices.Contains(r.Categories, ev.Category) {
		return false
	}
	if len(r.SourceIDs) > 0 && !slices.Contains(r.SourceIDs, ev.SourceID) {
		return false
	}
	if len(r.Tags) > 0 && !slices.ContainsFunc(r.Tags, ev.HasTag) {
		return false
	}
	if r.re != nil && !r.re.MatchString(ev.Message) {
		return false
	}
	return true
}

// RouteMatcher returns a function reporting which enabled routes in c an
// event would be sent through. It builds no notifiers and sends nothing,
// so offline tools can use it safely.
func RouteMatcher(c *config.Config) func(ev *model.Event) []string {
	routes := compileRoutes(c.Routes)
	return func(ev *model.Event) []string {
		var out []string
		for i := range routes {
			if routes[i].matches(ev) {
				out = append(out, routes[i].Name)
			}
		}
		return out
	}
}

// Matches returns the names of routes ev would be sent through (for the
// rule tester in the UI).
func (d *Dispatcher) Matches(ev *model.Event) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	for i := range d.routes {
		if d.routes[i].matches(ev) {
			out = append(out, d.routes[i].Name)
		}
	}
	return out
}

// Handle routes a classified event. It never blocks.
func (d *Dispatcher) Handle(ev model.Event) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	now := time.Now()
	for i := range d.routes {
		r := &d.routes[i]
		if !r.matches(&ev) {
			continue
		}
		for _, nid := range r.NotifierIDs {
			ch, ok := d.channels[nid]
			if !ok {
				continue
			}
			suppressed, send := d.admit(r, nid, ev.Fingerprint, now)
			if !send {
				d.Throttled.Add(1)
				continue
			}
			msg := &Message{Event: ev, RouteName: r.Name, Suppressed: suppressed, Link: d.eventLink(ev.ID)}
			select {
			case d.queue <- job{ch: ch, msg: msg}:
			default:
				d.Dropped.Add(1)
				d.record(ch, msg, "dropped", "notification queue full", 0, 0)
			}
		}
	}
}

// admit applies the route's per-fingerprint throttle window.
func (d *Dispatcher) admit(r *route, notifierID, fp string, now time.Time) (int, bool) {
	if r.ThrottleSeconds <= 0 {
		return 0, true
	}
	key := r.ID + "|" + notifierID + "|" + fp
	d.tmu.Lock()
	defer d.tmu.Unlock()
	st, ok := d.throttle[key]
	if !ok {
		d.throttle[key] = &throttleState{last: now}
		return 0, true
	}
	if now.Sub(st.last) < time.Duration(r.ThrottleSeconds)*time.Second {
		st.suppressed++
		return 0, false
	}
	n := st.suppressed
	st.last, st.suppressed = now, 0
	return n, true
}

func (d *Dispatcher) eventLink(id string) string {
	if d.publicURL == "" || id == "" {
		return ""
	}
	return d.publicURL + "/#/events/" + id
}

// Run starts delivery workers and blocks until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context, workers int) {
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-d.queue:
					d.deliver(ctx, j.ch, j.msg)
				}
			}
		}()
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case now := <-t.C:
			d.pruneThrottle(now)
		}
	}
}

func (d *Dispatcher) pruneThrottle(now time.Time) {
	d.tmu.Lock()
	defer d.tmu.Unlock()
	for k, st := range d.throttle {
		if now.Sub(st.last) > 24*time.Hour {
			delete(d.throttle, k)
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, ch *channel, msg *Message) error {
	start := time.Now()
	if ch.err != nil {
		d.Failed.Add(1)
		d.record(ch, msg, "failed", "misconfigured: "+ch.err.Error(), 0, 0)
		return ch.err
	}
	var err error
	attempt := 1
	for ; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err = ch.sender.Send(actx, msg)
		cancel()
		// Back off 2s, then 8s, between attempts.
		if err == nil || !retryable(err) || attempt == maxAttempts || !sleep(ctx, time.Duration(attempt*attempt)*2*time.Second) {
			break
		}
	}
	dur := time.Since(start)
	if err != nil {
		d.Failed.Add(1)
		d.log.Warn("notification failed", "notifier", ch.n.Name, "attempts", attempt, "err", err)
		d.record(ch, msg, "failed", err.Error(), attempt, dur)
		return err
	}
	d.Sent.Add(1)
	d.record(ch, msg, "sent", "", attempt, dur)
	return nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Test sends a sample notification through n synchronously.
func (d *Dispatcher) Test(ctx context.Context, n config.Notifier) error {
	s, err := Build(n)
	ch := &channel{n: n, sender: s, err: err}
	msg := sampleMessage()
	d.mu.RLock()
	if d.publicURL != "" {
		msg.Link = d.publicURL + "/"
	}
	d.mu.RUnlock()
	return d.deliver(ctx, ch, msg)
}

func (d *Dispatcher) record(ch *channel, msg *Message, status, errMsg string, attempts int, dur time.Duration) {
	del := Delivery{
		ID:           model.NewID(),
		Time:         time.Now().UTC(),
		NotifierID:   ch.n.ID,
		NotifierName: ch.n.Name,
		NotifierType: ch.n.Type,
		RouteName:    msg.RouteName,
		EventID:      msg.Event.ID,
		Severity:     msg.Event.Severity,
		Title:        msg.Title(),
		Status:       status,
		Error:        errMsg,
		Attempts:     attempts,
		DurationMs:   dur.Milliseconds(),
		Test:         msg.Test,
	}
	d.dmu.Lock()
	d.deliveries = append(d.deliveries, del)
	if len(d.deliveries) > maxDeliveries {
		d.deliveries = append([]Delivery(nil), d.deliveries[len(d.deliveries)-maxDeliveries:]...)
	}
	d.dmu.Unlock()
}

// Deliveries returns recent delivery records, newest first.
func (d *Dispatcher) Deliveries(limit int) []Delivery {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	if limit <= 0 || limit > len(d.deliveries) {
		limit = len(d.deliveries)
	}
	out := make([]Delivery, 0, limit)
	for i := len(d.deliveries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, d.deliveries[i])
	}
	return out
}

// QueueDepth reports pending notifications.
func (d *Dispatcher) QueueDepth() int { return len(d.queue) }
