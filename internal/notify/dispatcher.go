package notify

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
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
	defaultQueueSize = 10000
	maxDeliveries    = 500
	maxAttempts      = 3
	attemptTimeout   = 30 * time.Second
)

// Delivery records one notification attempt for the UI.
type Delivery struct {
	ID           string         `json:"id"`
	Time         time.Time      `json:"time"`
	NotifierID   string         `json:"notifier_id"`
	NotifierName string         `json:"notifier_name"`
	NotifierType string         `json:"notifier_type"`
	Kind         string         `json:"kind"` // alert | digest
	RouteName    string         `json:"route_name,omitempty"`
	Routes       []string       `json:"routes,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	ReasonText   string         `json:"reason_text,omitempty"`
	EventID      string         `json:"event_id,omitempty"`
	Fingerprint  string         `json:"fingerprint,omitempty"` // alerts
	Severity     model.Severity `json:"severity,omitempty"`    // digests: the highest
	Title        string         `json:"title"`
	Events       int            `json:"events,omitempty"`   // alert: 1 + Suppressed; digest: Events
	Patterns     int            `json:"patterns,omitempty"` // digests
	Held         int            `json:"held,omitempty"`     // digests
	Status       string         `json:"status"`             // sent, failed, dropped (queued: a digest not attempted yet)
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
	n        config.Notifier
	sender   Sender
	err      error
	noDigest bool // the plugin's NoDigest: right-away alerts only
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

// Options configures a Dispatcher.
type Options struct {
	StatePath string           // "" = alert state in memory only (Drain then sends pending digests)
	Now       func() time.Time // nil = time.Now
	Warmup    time.Duration    // 0 = DefaultWarmup; < 0 = no warm-up (tests, demos)
	QueueSize int              // per notifier lane; 0 = 10000
}

// alerting is the decision layer's state, guarded by Dispatcher.amu.
type alerting struct {
	last      time.Time                // latest decision time: now never goes backwards
	sources   map[string]*sourceState  // source ID
	patterns  map[string]*patternState // fingerprint
	alerts    map[string]*alertState   // notifierID + "|" + fingerprint
	digests   map[string]*digestBuffer // notifier ID
	recent    map[string][]time.Time   // notifier → non-critical Smart right-away sends, last 10 min
	critical  map[string][]time.Time   // notifier → critical Smart right-away sends, last 10 min
	promoted  map[string]time.Time     // notifierID|sourceID|category → last promotion sent
	summaries map[string]time.Time     // notifier → last summary flushed
	decisions decisionRing             // event ID → []Decision (memory only)
	digestLog digestLog                // last 50 sent digests in full, by ID (memory only)
	dirty     bool
	seq       uint64
	stuck     map[string]bool // notifiers whose digest couldn't be queued (logged once per streak)
}

func newAlerting() alerting {
	return alerting{
		sources: map[string]*sourceState{}, patterns: map[string]*patternState{}, alerts: map[string]*alertState{},
		digests: map[string]*digestBuffer{}, recent: map[string][]time.Time{}, critical: map[string][]time.Time{},
		promoted: map[string]time.Time{}, summaries: map[string]time.Time{}, stuck: map[string]bool{},
		decisions: decisionRing{m: map[string][]Decision{}}, digestLog: digestLog{m: map[string]*Digest{}},
	}
}

// alertState is one channel's throttle window for one problem.
type alertState struct {
	LastSent     time.Time      `json:"last_sent,omitzero"`
	LastSeverity model.Severity `json:"last_severity,omitempty"`
	Pending      int            `json:"pending,omitempty"` // repeats since LastSent not reported yet (no-digest channels)
	Seq          uint64         `json:"seq"`
	Updated      time.Time      `json:"updated,omitzero"` // last change, for pruning a state a failure reset
}

// touched is when the state last changed.
func (st *alertState) touched() time.Time {
	if st.Updated.After(st.LastSent) {
		return st.Updated
	}
	return st.LastSent
}

// decisionRing keeps the decisions of the last maxDecisionEvents events.
type decisionRing struct {
	m    map[string][]Decision
	ids  []string // FIFO
	head int
}

func (r *decisionRing) put(id string, decs []Decision) {
	if id == "" {
		return
	}
	if _, ok := r.m[id]; ok {
		r.m[id] = decs
		return
	}
	if len(r.ids) < maxDecisionEvents {
		r.ids = append(r.ids, id)
	} else {
		delete(r.m, r.ids[r.head])
		r.ids[r.head] = id
		r.head = (r.head + 1) % maxDecisionEvents
	}
	r.m[id] = decs
}

// digestLog keeps the last digestLogSize digests queued, by ID.
type digestLog struct {
	m   map[string]*Digest
	ids []string
}

func (l *digestLog) put(dg *Digest) {
	l.m[dg.ID] = dg
	l.ids = append(l.ids, dg.ID)
	if len(l.ids) > digestLogSize {
		delete(l.m, l.ids[0])
		l.ids = slices.Delete(l.ids, 0, 1)
	}
}

// Dispatcher matches events against routes, decides per channel whether
// each goes right away or into the channel's digest, and delivers through
// bounded per-notifier queues with retries.
type Dispatcher struct {
	log       *slog.Logger
	nowFn     func() time.Time
	warmup    time.Duration
	queueSize int
	statePath string

	mu        sync.RWMutex
	routes    []route
	channels  map[string]*channel
	publicURL string

	// Lock order: mu → amu → lmu → dmu. saveMu is taken holding none of
	// them; stmu holding nothing else.
	amu    sync.Mutex
	al     alerting
	closed bool // Drain finished: flushDue sends nothing more

	lmu     sync.Mutex
	lanes   map[string]*lane // by notifier ID
	runCtx  context.Context  // set by Run; lanes made later start workers with it
	workers int              // per lane
	stopped bool             // Run is stopping: no new workers
	wg      sync.WaitGroup
	pending atomic.Int64 // queued or being delivered

	draining  chan struct{} // closed by Drain: no more retry waits
	drainOnce sync.Once

	saveMu  sync.Mutex
	stmu    sync.Mutex
	savedAt time.Time
	saveErr string

	dmu        sync.Mutex
	deliveries []Delivery // ring of maxDeliveries, oldest at dhead once full
	dhead      int

	Sent, Failed, Dropped, Throttled atomic.Uint64 // Sent and Failed include digests; Throttled counts throttled decisions
	// Decided counts decisions by Actions; RightAway counts alerts sent
	// right away by RightAwayReasons.
	Decided, RightAway                                                                      [6]atomic.Uint64
	DigestsSent, DigestsFailed, DigestsDiscarded, DigestEvents, LostEvents, StateSaveErrors atomic.Uint64
}

// NewDispatcherWith returns an idle dispatcher with its alert state loaded
// from o.StatePath; call Configure and Run.
func NewDispatcherWith(log *slog.Logger, o Options) *Dispatcher {
	d := &Dispatcher{
		log:       log,
		nowFn:     o.Now,
		warmup:    o.Warmup,
		queueSize: o.QueueSize,
		statePath: o.StatePath,
		channels:  map[string]*channel{},
		al:        newAlerting(),
		lanes:     map[string]*lane{},
		draining:  make(chan struct{}),
	}
	if d.nowFn == nil {
		d.nowFn = time.Now
	}
	if d.warmup == 0 {
		d.warmup = DefaultWarmup
	}
	if d.queueSize <= 0 {
		d.queueSize = defaultQueueSize
	}
	if d.statePath != "" {
		d.loadState()
	}
	return d
}

// NewDispatcher returns an idle dispatcher that keeps alert state in
// memory only; call Configure and Run.
func NewDispatcher(log *slog.Logger) *Dispatcher { return NewDispatcherWith(log, Options{}) }

// now is the clock, unclamped.
func (d *Dispatcher) now() time.Time { return d.nowFn().UTC() }

// clock is the decision time: read under amu and never earlier than the
// last one, so windows and digests never see time go backwards.
func (d *Dispatcher) clock() time.Time { return d.clampLocked(d.now()) }

func (d *Dispatcher) clampLocked(now time.Time) time.Time {
	now = now.UTC()
	if now.Before(d.al.last) {
		return d.al.last
	}
	d.al.last = now
	return now
}

// enqueue queues j on notifier id's lane, creating the lane (and, once Run
// has started, its workers). It reports false when the lane is full. Sends
// happen under lmu so pruneLanes never strands a job on a stopped lane.
func (d *Dispatcher) enqueue(id string, j job) bool {
	d.lmu.Lock()
	defer d.lmu.Unlock()
	l, ok := d.lanes[id]
	if !ok {
		l = &lane{queue: make(chan job, d.queueSize), quit: make(chan struct{})}
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
					d.work(ctx, j)
				}
			}
		}()
	}
}

// work delivers one queued job and applies the outcome to alert state.
func (d *Dispatcher) work(ctx context.Context, j job) {
	err := d.deliver(ctx, j.ch, j.msg)
	d.completed(j.ch, j.msg, err)
	d.pending.Add(-1)
}

// Configure swaps in new notifiers and routes. It never touches alert
// state: learned-rule batches call it often.
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
		channels[n.ID] = &channel{n: n, sender: s, err: err, noDigest: noDigest(n.Type)}
	}
	groupRoutes, groupChannels := alertGroups(c)
	for id, n := range groupChannels {
		s, err := Build(n)
		if err != nil {
			d.log.Warn("alert group email misconfigured", "notifier", n.Name, "err", err)
		}
		channels[id] = &channel{n: n, sender: s, err: err, noDigest: noDigest(n.Type)}
	}
	routes := compileRoutes(append(slices.Clone(c.Routes), groupRoutes...))
	d.mu.Lock()
	d.channels, d.routes, d.publicURL = channels, routes, c.Settings.PublicURL
	d.mu.Unlock()
}

func noDigest(typ string) bool {
	p, ok := PluginFor(typ)
	return ok && p.NoDigest
}

// alertGroups returns a route for every enabled alert group with sources
// assigned: the group's filters and delivery, limited to those sources, to
// its channels and, when it has members, to an email channel addressed to
// them (an email notifier's settings with the members as recipients, keyed
// by groupEmailID).
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
			DeliveryPolicy: g.DeliveryPolicy,
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

// matching returns the indexes of the routes ev matches; d.mu is held.
func (d *Dispatcher) matching(ev *model.Event) []int {
	var out []int
	for i := range d.routes {
		if d.routes[i].matches(ev) {
			out = append(out, i)
		}
	}
	return out
}

// Handle decides, for every channel a classified event's routes reach,
// whether it goes right away, into the channel's digest or nowhere, and
// queues what goes right away. It never blocks.
func (d *Dispatcher) Handle(ev model.Event) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.amu.Lock()
	defer d.amu.Unlock()
	now := d.clock()
	a := &d.al
	a.sawSource(ev.SourceID, ev.SourceName, now)
	matching := d.matching(&ev)
	// Unrouted info noise is not tracked, so it never evicts error patterns.
	if len(matching) == 0 && !ev.Severity.AtLeast(model.SeverityWarning) {
		return
	}
	a.dirty = true
	sig := a.observe(&ev, now, d.warmup)
	if len(matching) == 0 {
		return
	}
	ps, order := d.plans(&ev, sig, matching)
	decs := make([]Decision, 0, len(order))
	spikeUsed := false
	for _, nid := range order {
		dec, used := d.decide(&ev, sig, nid, d.channels[nid], ps[nid], now)
		decs = append(decs, dec)
		spikeUsed = spikeUsed || used
	}
	if p := a.patterns[ev.Fingerprint]; spikeUsed && p != nil {
		p.SpikeAt = now
	}
	a.decisions.put(ev.ID, decs)
}

func (d *Dispatcher) eventLink(id string) string {
	if d.publicURL == "" || id == "" {
		return ""
	}
	return d.publicURL + "/#/events/" + id
}

// completed applies a delivery's outcome to alert state: a digest that
// failed retryably is merged back once; an alert that did re-arms its
// window and returns its events, to be reported once. It runs on lane
// workers with no lock held.
func (d *Dispatcher) completed(ch *channel, msg *Message, err error) {
	if msg.Test {
		return
	}
	d.amu.Lock()
	defer d.amu.Unlock()
	now := d.clock()
	a := &d.al
	retry := err != nil && retryable(err) && ch.err == nil
	if dg := msg.Digest; dg != nil {
		switch {
		case err == nil:
			d.DigestsSent.Add(1)
			d.DigestEvents.Add(uint64(dg.Events))
		case retry && msg.buf != nil && !msg.buf.Retried:
			a.mergeBack(msg.nid, msg.buf, now)
			a.dirty = true
		default:
			d.DigestsFailed.Add(1)
			d.LostEvents.Add(uint64(dg.Events))
			d.log.Warn("digest not delivered: its events are lost", "notifier", ch.n.Name, "events", dg.Events, "err", err)
		}
		return
	}
	if !retry {
		return // sent, or a failure retrying can't fix: the window stays
	}
	key := msg.nid + "|" + msg.fp
	st := a.alerts[key]
	if st != nil && st.Seq == msg.seq { // re-arm: the next event alerts
		st.LastSent, st.LastSeverity, st.Updated = msg.prevSent, msg.prevSev, now
	}
	// The alert's events go back, to be reported once.
	k := 1 + msg.Suppressed
	if msg.digestMin > 0 {
		var sig *signal
		if p := a.patterns[msg.fp]; p != nil {
			sig = &signal{FirstSeen: p.First}
		}
		a.addToDigest(msg.nid, &msg.Event, sig, msg.digestMin, msg.Routes, entryAdd{undelivered: k}, now)
	} else {
		if st == nil {
			st = &alertState{}
			a.alerts[key] = st
		}
		st.Pending += k
		st.Updated = now
	}
	a.dirty = true
}

// wait waits until every queued notification has been delivered (or has
// failed), or until ctx ends.
func (d *Dispatcher) wait(ctx context.Context) error {
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

// Drain sends the digests already due, waits until every queued
// notification has been delivered (or has failed), or until ctx ends, and
// saves alert state; digests not yet due are kept for after the restart.
// Failed attempts are no longer retried, so a failing notifier doesn't
// hold up the rest. Stop handing it events first.
func (d *Dispatcher) Drain(ctx context.Context) error {
	d.drainOnce.Do(func() { close(d.draining) })
	d.flushDue(d.now())
	err := d.wait(ctx)
	d.amu.Lock()
	d.closed = true
	buffered := len(d.al.digests) > 0
	d.amu.Unlock()
	serr := d.SaveState()
	if buffered && (d.statePath == "" || serr != nil) {
		d.log.Warn("could not save alert state; sending pending digests now", "err", serr)
		d.flushAll(ReasonShutdown)
		if werr := d.wait(ctx); err == nil {
			err = werr
		}
	}
	return err
}

// Run delivers with workers per notifier, sends digests when they are due,
// saves alert state and blocks until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context, workers int) {
	d.lmu.Lock()
	d.runCtx, d.workers = ctx, max(workers, 1)
	for _, l := range d.lanes {
		d.startLocked(l)
	}
	d.lmu.Unlock()
	d.flushDue(d.now()) // digests that came due while Lissandra was down
	flush := time.NewTicker(flushTick)
	defer flush.Stop()
	save := time.NewTicker(saveEvery)
	defer save.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			d.lmu.Lock()
			d.stopped = true // a lane made now must not add to wg while it is waited on
			d.lmu.Unlock()
			d.wg.Wait()
			return
		case <-flush.C:
			d.flushDue(d.now())
		case <-save.C:
			d.amu.Lock()
			dirty := d.al.dirty
			d.amu.Unlock()
			if dirty {
				_ = d.SaveState() // logged and counted
			}
		case <-prune.C:
			d.prune(d.now())
			d.pruneLanes()
		}
	}
}

// prune drops alert state nothing needs any more.
func (d *Dispatcher) prune(now time.Time) {
	d.amu.Lock()
	defer d.amu.Unlock()
	now = d.clampLocked(now)
	a := &d.al
	before := len(a.patterns) + len(a.sources) + len(a.alerts) + len(a.promoted) + len(a.summaries)
	a.prunePatterns(now)
	for k, st := range a.alerts {
		age := now.Sub(st.touched())
		switch {
		case st.Pending == 0 && age > alertStateTTL:
			delete(a.alerts, k)
		case st.Pending > 0 && age > pendingTTL:
			d.LostEvents.Add(uint64(st.Pending))
			delete(a.alerts, k)
		}
	}
	if len(a.alerts) > maxAlertStates {
		a.evictAlerts(d)
	}
	for _, m := range []map[string][]time.Time{a.recent, a.critical} {
		for k, ts := range m {
			if kept := within(ts, now); len(kept) == 0 {
				delete(m, k)
				a.dirty = true
			} else if len(kept) < len(ts) {
				m[k] = kept
				a.dirty = true
			}
		}
	}
	for _, m := range []map[string]time.Time{a.promoted, a.summaries} {
		for k, t := range m {
			if now.Sub(t) >= limitWindow {
				delete(m, k)
			}
		}
	}
	if len(a.patterns)+len(a.sources)+len(a.alerts)+len(a.promoted)+len(a.summaries) != before {
		a.dirty = true
	}
}

// evictAlerts drops the oldest alert states down to 90% of the cap, those
// with nothing pending first; pending repeats evicted are lost.
func (a *alerting) evictAlerts(d *Dispatcher) {
	type kv struct {
		key string
		st  *alertState
	}
	all := make([]kv, 0, len(a.alerts))
	for k, st := range a.alerts {
		all = append(all, kv{k, st})
	}
	slices.SortFunc(all, func(x, y kv) int {
		if (x.st.Pending > 0) != (y.st.Pending > 0) {
			if x.st.Pending == 0 {
				return -1
			}
			return 1
		}
		if c := x.st.touched().Compare(y.st.touched()); c != 0 {
			return c
		}
		return strings.Compare(x.key, y.key)
	})
	for _, e := range all[:len(all)-maxAlertStates*9/10] {
		if e.st.Pending > 0 {
			d.LostEvents.Add(uint64(e.st.Pending))
		}
		delete(a.alerts, e.key)
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
	ch := &channel{n: n, sender: s, err: err, noDigest: noDigest(n.Type)}
	msg := sampleMessage(d.now())
	d.mu.RLock()
	if d.publicURL != "" {
		msg.Link = d.publicURL + "/"
	}
	d.mu.RUnlock()
	return d.deliver(ctx, ch, msg)
}

// TestDigest sends a sample digest through n synchronously. It returns
// ErrNoDigest for channels that never get digests (PagerDuty).
func (d *Dispatcher) TestDigest(ctx context.Context, n config.Notifier) error {
	if noDigest(n.Type) {
		return ErrNoDigest
	}
	s, err := Build(n)
	ch := &channel{n: n, sender: s, err: err}
	now := d.now()
	dg := sampleDigest(now)
	d.mu.RLock()
	if d.publicURL != "" {
		dg.Link = d.publicURL + "/"
		for i := range dg.Entries {
			dg.Entries[i].Link = d.publicURL + "/"
		}
	}
	d.mu.RUnlock()
	msg := &Message{ID: dg.ID, Kind: KindDigest, Digest: dg, Routes: dg.Routes, RouteName: strings.Join(dg.Routes, ", "),
		Reason: Reason{ReasonTest, digestText(dg, nil)}, Link: dg.Link, Test: true, at: now}
	msg.Event = digestEvent(msg)
	return d.deliver(ctx, ch, msg)
}

func (d *Dispatcher) record(ch *channel, msg *Message, status, errMsg string, attempts int, dur time.Duration) {
	del := deliveryFor(ch, msg)
	del.Time, del.Status, del.Error, del.Attempts, del.DurationMs = d.now(), status, errMsg, attempts, dur.Milliseconds()
	d.dmu.Lock()
	if len(d.deliveries) < maxDeliveries {
		d.deliveries = append(d.deliveries, del)
	} else {
		d.deliveries[d.dhead] = del
		d.dhead = (d.dhead + 1) % maxDeliveries
	}
	d.dmu.Unlock()
}

// recentDeliveries calls f with delivery records, newest first, until it
// returns false; dmu is held.
func (d *Dispatcher) recentDeliveries(f func(*Delivery) bool) {
	n := len(d.deliveries)
	for i := 1; i <= n; i++ {
		if !f(&d.deliveries[(d.dhead-i+n)%n]) {
			return
		}
	}
}

// deliveryFor describes msg on ch, without an outcome.
func deliveryFor(ch *channel, msg *Message) Delivery {
	del := Delivery{
		ID:           msg.ID,
		NotifierID:   ch.n.ID,
		NotifierName: ch.n.Name,
		NotifierType: ch.n.Type,
		Kind:         KindAlert,
		RouteName:    msg.RouteName,
		Routes:       msg.Routes,
		Reason:       msg.Reason.Code,
		ReasonText:   msg.Reason.Text,
		Title:        msg.Title(),
		Test:         msg.Test,
	}
	if del.ID == "" {
		del.ID = model.NewID()
	}
	if dg := msg.Digest; dg != nil {
		del.Kind, del.Severity, del.Events, del.Patterns, del.Held = KindDigest, dg.Highest, dg.Events, dg.Patterns, dg.Held
		return del
	}
	del.EventID, del.Severity, del.Fingerprint, del.Events = msg.Event.ID, msg.Event.Severity, msg.Event.Fingerprint, 1+msg.Suppressed
	return del
}

// Deliveries returns recent delivery records, newest first; kind, when
// given and not "", keeps only alerts or digests.
func (d *Dispatcher) Deliveries(limit int, kind ...string) []Delivery {
	k := ""
	if len(kind) > 0 {
		k = kind[0]
	}
	d.dmu.Lock()
	defer d.dmu.Unlock()
	if limit <= 0 || limit > len(d.deliveries) {
		limit = len(d.deliveries)
	}
	out := make([]Delivery, 0, limit)
	d.recentDeliveries(func(del *Delivery) bool {
		if k == "" || del.Kind == k {
			out = append(out, *del)
		}
		return len(out) < limit
	})
	return out
}

// DeliveryDetail returns a delivery record and, for one of the last 50
// digests, the digest in full. A digest still queued has a record with
// status "queued".
func (d *Dispatcher) DeliveryDetail(id string) (Delivery, *Digest, bool) {
	d.amu.Lock()
	dg := d.al.digestLog.m[id]
	d.amu.Unlock()
	d.dmu.Lock()
	defer d.dmu.Unlock()
	var found *Delivery
	d.recentDeliveries(func(del *Delivery) bool {
		if del.ID == id {
			found = del
		}
		return found == nil
	})
	if found != nil {
		return *found, dg, true
	}
	if dg != nil {
		return Delivery{ID: dg.ID, Time: dg.To, Kind: KindDigest, Routes: dg.Routes, RouteName: strings.Join(dg.Routes, ", "),
			Reason: dg.Reason, Severity: dg.Highest, Events: dg.Events, Patterns: dg.Patterns, Held: dg.Held, Status: "queued"}, dg, true
	}
	return Delivery{}, nil, false
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

// Decisions returns what an event did on each channel, while it is among
// the last 5,000 events handled (memory only).
func (d *Dispatcher) Decisions(eventID string) []Decision {
	d.amu.Lock()
	defer d.amu.Unlock()
	return slices.Clone(d.al.decisions.m[eventID])
}

// Preview returns where ev would go, without reading or changing alert
// state: it judges ev as if nothing were new, spiking or worse (for the
// rule tester and the source test).
func (d *Dispatcher) Preview(ev *model.Event) []Decision {
	d.mu.RLock()
	defer d.mu.RUnlock()
	ps, order := d.plans(ev, &signal{}, d.matching(ev))
	out := make([]Decision, 0, len(order))
	for _, nid := range order {
		ch, p := d.channels[nid], ps[nid]
		dec := Decision{NotifierID: nid, NotifierName: ch.n.Name, NotifierType: ch.n.Type, Routes: p.routes}
		switch p.tier {
		case tierNow:
			dec.Action = ActionRightAway
			what := "every matching event"
			if p.reason == ReasonSeverity {
				what = string(ev.Severity)
			}
			dec.Reason = Reason{p.reason, fmt.Sprintf("Right away (%s on %q).", what, routeName(p.route))}
		case tierDigest:
			dec.Action = ActionDigest
			every := fmtDur(time.Duration(p.digestMin) * time.Minute)
			text := fmt.Sprintf("In the next digest (every %s), unless it is new, spiking or getting worse.", every)
			if p.route != nil && p.route.Delivery == config.DeliveryDigest {
				text = fmt.Sprintf("In the next digest (every %s).", every)
			}
			dec.Reason = Reason{ReasonDigest, text}
		default:
			dec.Action, dec.Reason = ActionSkipped, Reason{ReasonNoDigest, skippedText(ch, p)}
		}
		out = append(out, dec)
	}
	return out
}

// WarmingSource is a source still learning what's normal.
type WarmingSource struct {
	SourceID   string    `json:"source_id"`
	SourceName string    `json:"source_name"`
	Until      time.Time `json:"until"`
}

// StateStatus is how alert state is persisted.
type StateStatus struct {
	Saved   bool       `json:"saved"` // kept across restarts
	File    string     `json:"file"`  // "" when not kept
	SavedAt *time.Time `json:"saved_at"`
	Error   string     `json:"error"`
}

// Limits are the decision layer's constants, for the UI.
type Limits struct {
	WarmupMinutes       int     `json:"warmup_minutes"`
	NewAfterDays        int     `json:"new_after_days"`
	SpikeFactor         float64 `json:"spike_factor"`
	SpikeMin            int     `json:"spike_min"`
	FloodMin            int     `json:"flood_min"`
	ChannelAlerts       int     `json:"channel_alerts"`
	SourceAlerts        int     `json:"source_alerts"`
	CriticalAlerts      int     `json:"critical_alerts"`
	WindowMinutes       int     `json:"window_minutes"`
	SummaryDelayMinutes int     `json:"summary_delay_minutes"`
}

// Alerting is GET /api/alerting.
type Alerting struct {
	Pending         []PendingDigest `json:"pending"`
	WarmingUp       []WarmingSource `json:"warming_up"`
	PatternsTracked int             `json:"patterns_tracked"`
	State           StateStatus     `json:"state"`
	Limits          Limits          `json:"limits"`
}

// AlertingStatus reports pending digests, sources warming up, how alert
// state is saved and the limits.
func (d *Dispatcher) AlertingStatus() Alerting {
	out := Alerting{Pending: d.PendingDigests(), WarmingUp: []WarmingSource{}}
	d.amu.Lock()
	now := d.clock()
	if d.warmup > 0 {
		for id, s := range d.al.sources {
			if until := s.warmUntil(d.warmup); now.Before(until) {
				out.WarmingUp = append(out.WarmingUp, WarmingSource{SourceID: id, SourceName: s.Name, Until: until})
			}
		}
	}
	out.PatternsTracked = len(d.al.patterns)
	d.amu.Unlock()
	slices.SortFunc(out.WarmingUp, func(x, y WarmingSource) int {
		if c := x.Until.Compare(y.Until); c != 0 {
			return c
		}
		return strings.Compare(x.SourceID, y.SourceID)
	})
	d.stmu.Lock()
	out.State = StateStatus{Saved: d.statePath != "", Error: d.saveErr}
	if d.statePath != "" {
		out.State.File = filepath.Base(d.statePath)
	}
	if !d.savedAt.IsZero() {
		t := d.savedAt
		out.State.SavedAt = &t
	}
	d.stmu.Unlock()
	out.Limits = Limits{
		WarmupMinutes: int(max(d.warmup, 0) / time.Minute), NewAfterDays: int(newAfter / (24 * time.Hour)),
		SpikeFactor: spikeFactor, SpikeMin: spikeMin, FloodMin: floodMin, ChannelAlerts: channelLimit, SourceAlerts: sourceLimit,
		CriticalAlerts: criticalLimit, WindowMinutes: int(limitWindow / time.Minute), SummaryDelayMinutes: int(summaryDelay / time.Minute),
	}
	return out
}

// Gauges is the alert state's size, for /api/stats and /metrics.
type Gauges struct {
	PendingDigestEvents int `json:"pending_digest_events"`
	PatternsTracked     int `json:"patterns_tracked"`
	WarmingUp           int `json:"warming_up"` // sources
}

// Gauges reports events waiting in digests, patterns tracked and sources
// warming up.
func (d *Dispatcher) Gauges() Gauges {
	d.amu.Lock()
	defer d.amu.Unlock()
	now := d.clock()
	g := Gauges{PatternsTracked: len(d.al.patterns)}
	for _, buf := range d.al.digests {
		g.PendingDigestEvents += buf.Events
	}
	if d.warmup > 0 {
		for _, s := range d.al.sources {
			if now.Before(s.warmUntil(d.warmup)) {
				g.WarmingUp++
			}
		}
	}
	return g
}

// Stats is the "notifications" object of /api/stats.
func (d *Dispatcher) Stats() map[string]any {
	g := d.Gauges()
	right := map[string]uint64{}
	for i, r := range RightAwayReasons {
		right[r] = d.RightAway[i].Load()
	}
	decided := func(action string) uint64 { return d.Decided[indexOf(Actions, action)].Load() }
	return map[string]any{
		"queue_depth": d.QueueDepth(), "sent": d.Sent.Load(), "failed": d.Failed.Load(),
		"dropped": d.Dropped.Load(), "throttled": d.Throttled.Load(),
		"digests": d.DigestsSent.Load(), "digests_failed": d.DigestsFailed.Load(), "digest_events": d.DigestEvents.Load(),
		"batched": decided(ActionDigest), "held": decided(ActionHeld), "skipped": decided(ActionSkipped),
		"lost_events": d.LostEvents.Load(), "right_away": right,
		"pending_digest_events": g.PendingDigestEvents, "patterns_tracked": g.PatternsTracked, "warming_up": g.WarmingUp,
	}
}
