package notify

import (
	"context"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

const (
	queueSize      = 10000
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

// lane is one notifier's queue with its own workers, so a slow, hung or
// failing notifier only delays its own notifications.
type lane struct {
	queue chan job
	quit  chan struct{} // closed when the notifier is gone and the queue is empty
}

type throttleState struct {
	last       time.Time
	suppressed int
}

// Dispatcher matches events against routes and delivers them to notifiers
// through bounded per-notifier queues with retries.
type Dispatcher struct {
	log *slog.Logger

	mu        sync.RWMutex
	routes    []route
	channels  map[string]*channel
	publicURL string

	tmu      sync.Mutex
	throttle map[string]*throttleState

	lmu     sync.Mutex
	lanes   map[string]*lane // by notifier ID
	runCtx  context.Context  // set by Run; lanes made later start workers with it
	workers int              // per lane
	stopped bool             // Run is stopping: no new workers
	wg      sync.WaitGroup
	pending atomic.Int64 // queued or being delivered

	draining  chan struct{} // closed by Drain: no more retry waits
	drainOnce sync.Once

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
		lanes:    map[string]*lane{},
		draining: make(chan struct{}),
	}
}

// enqueue queues j on notifier id's lane, creating the lane (and, once Run
// has started, its workers). It reports false when the lane is full. Sends
// happen under lmu so pruneLanes never strands a job on a stopped lane.
func (d *Dispatcher) enqueue(id string, j job) bool {
	d.lmu.Lock()
	defer d.lmu.Unlock()
	l, ok := d.lanes[id]
	if !ok {
		l = &lane{queue: make(chan job, queueSize), quit: make(chan struct{})}
		d.lanes[id] = l
		if d.runCtx != nil {
			d.startLocked(l)
		}
	}
	select {
	case l.queue <- j:
		return true
	default:
		return false
	}
}

// pruneLanes stops the idle lanes of notifiers that no longer exist.
func (d *Dispatcher) pruneLanes() {
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.lmu.Lock()
	defer d.lmu.Unlock()
	for id, l := range d.lanes {
		if _, ok := d.channels[id]; !ok && len(l.queue) == 0 {
			delete(d.lanes, id)
			close(l.quit)
		}
	}
}

func (d *Dispatcher) startLocked(l *lane) {
	if d.stopped {
		return
	}
	ctx := d.runCtx
	for i := 0; i < d.workers; i++ {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-l.quit:
					return
				case j := <-l.queue:
					d.deliver(ctx, j.ch, j.msg)
					d.pending.Add(-1)
				}
			}
		}()
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
	groupRoutes, groupChannels := alertGroups(c)
	for id, n := range groupChannels {
		s, err := Build(n)
		if err != nil {
			d.log.Warn("alert group email misconfigured", "notifier", n.Name, "err", err)
		}
		channels[id] = &channel{n: n, sender: s, err: err}
	}
	routes := compileRoutes(append(slices.Clone(c.Routes), groupRoutes...))
	d.mu.Lock()
	d.channels, d.routes, d.publicURL = channels, routes, c.Settings.PublicURL
	d.mu.Unlock()
}

// alertGroups returns a route for every enabled alert group with sources
// assigned: the group's filters, limited to those sources, to its channels
// and, when it has members, to an email channel addressed to them (an
// email notifier's settings with the members as recipients, keyed by
// groupEmailID).
func alertGroups(c *config.Config) ([]config.Route, map[string]config.Notifier) {
	sources := map[string][]string{}
	for _, src := range c.Sources {
		for _, gid := range src.AlertGroupIDs {
			sources[gid] = append(sources[gid], src.ID)
		}
	}
	var routes []config.Route
	channels := map[string]config.Notifier{}
	for _, g := range c.AlertGroups {
		// A group without sources must not match every source.
		if !g.Enabled || len(sources[g.ID]) == 0 {
			continue
		}
		rt := config.Route{
			ID: "group:" + g.ID, Name: "Alert group: " + g.Name, Enabled: true,
			MinSeverity: g.MinSeverity, Categories: g.Categories, SourceIDs: sources[g.ID],
			NotifierIDs: slices.Clone(g.NotifierIDs), ThrottleSeconds: g.ThrottleSeconds,
		}
		if i := slices.IndexFunc(c.Notifiers, func(n config.Notifier) bool { return n.ID == g.EmailNotifierID }); i >= 0 && len(g.Emails) > 0 && c.Notifiers[i].Enabled {
			n := c.Notifiers[i]
			n.ID, n.Name = groupEmailID(g.ID), n.Name+" → "+g.Name
			n.Settings = maps.Clone(n.Settings)
			n.Settings["to"] = strings.Join(g.Emails, ", ")
			channels[n.ID] = n
			rt.NotifierIDs = append(rt.NotifierIDs, n.ID)
		}
		routes = append(routes, rt)
	}
	return routes, channels
}

// groupEmailID is the channel that emails an alert group's members.
func groupEmailID(groupID string) string { return "group:" + groupID + ":email" }

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

// RouteMatcher returns a function reporting which enabled routes and alert
// groups in c an event would be sent through. It builds no notifiers and
// sends nothing, so offline tools can use it safely.
func RouteMatcher(c *config.Config) func(ev *model.Event) []string {
	groupRoutes, _ := alertGroups(c)
	routes := compileRoutes(append(slices.Clone(c.Routes), groupRoutes...))
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
			d.pending.Add(1)
			if !d.enqueue(nid, job{ch: ch, msg: msg}) {
				d.pending.Add(-1)
				if n := d.Dropped.Add(1); n == 1 || n%100 == 0 {
					d.log.Warn("notification queue full: alerts dropped", "notifier", ch.n.Name, "dropped_total", n)
				}
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

// Drain waits until every queued notification has been delivered (or has
// failed), or until ctx ends. Failed attempts are no longer retried, so a
// failing notifier doesn't hold up the rest. Stop handing it events first.
func (d *Dispatcher) Drain(ctx context.Context) error {
	d.drainOnce.Do(func() { close(d.draining) })
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for d.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// Run delivers with workers per notifier and blocks until ctx is
// cancelled.
func (d *Dispatcher) Run(ctx context.Context, workers int) {
	d.lmu.Lock()
	d.runCtx, d.workers = ctx, max(workers, 1)
	for _, l := range d.lanes {
		d.startLocked(l)
	}
	d.lmu.Unlock()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.lmu.Lock()
			d.stopped = true // a lane made now must not add to wg while it is waited on
			d.lmu.Unlock()
			d.wg.Wait()
			return
		case now := <-t.C:
			d.pruneThrottle(now)
			d.pruneLanes()
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
		// Back off 2s, then 8s, between attempts (not while draining).
		if err == nil || !retryable(err) || attempt == maxAttempts || !d.sleep(ctx, time.Duration(attempt*attempt)*2*time.Second) {
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

// sleep waits for dur unless ctx ends or a drain starts.
func (d *Dispatcher) sleep(ctx context.Context, dur time.Duration) bool {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-d.draining:
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

// QueueDepth reports queued notifications.
func (d *Dispatcher) QueueDepth() int {
	d.lmu.Lock()
	defer d.lmu.Unlock()
	n := 0
	for _, l := range d.lanes {
		n += len(l.queue)
	}
	return n
}
