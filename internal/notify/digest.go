package notify

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/model"
)

// DigestEntry is one problem (fingerprint) in a digest.
type DigestEntry struct {
	Fingerprint string         `json:"fingerprint"`
	SourceID    string         `json:"source_id"`
	SourceName  string         `json:"source_name"`
	Category    string         `json:"category"`
	Severity    model.Severity `json:"severity"`              // highest in this batch
	Count       int            `json:"count"`                 // events reported by this entry (repeats and undelivered included)
	Repeats     int            `json:"repeats,omitempty"`     // repeats of the alert at AlertedAt
	Undelivered int            `json:"undelivered,omitempty"` // events of right-away alerts that failed to deliver
	First       time.Time      `json:"first"`
	Last        time.Time      `json:"last"`
	FirstSeen   time.Time      `json:"first_seen"` // the pattern, from the tracker
	Sample      string         `json:"sample"`     // latest message at the highest severity, ≤ 300 bytes
	EventID     string         `json:"event_id"`   // the sample's event
	Link        string         `json:"link,omitempty"`
	New         bool           `json:"new,omitempty"`
	Held        string         `json:"held,omitempty"`       // would-be reason: new|spike|worse|burst|severity|every|repeat
	HeldBy      string         `json:"held_by,omitempty"`    // source_limit|channel_limit|critical_limit|queue_full|undelivered
	AlertedAt   time.Time      `json:"alerted_at,omitzero"`  // the last right-away alert on this channel for this problem
	AlertedAs   string         `json:"alerted_as,omitempty"` // its reason: new|spike|worse|burst|severity|every|repeat
	LastRepeat  time.Time      `json:"last_repeat,omitzero"` // the newest of Repeats
}

// Digest is one channel's batched events, sent as one message.
type Digest struct {
	ID              string         `json:"id"`     // = Message.ID = Delivery.ID
	Reason          string         `json:"reason"` // digest | summary | manual | shutdown | test
	From            time.Time      `json:"from"`
	To              time.Time      `json:"to"`
	IntervalMinutes int            `json:"interval_minutes"`
	Routes          []string       `json:"routes"`
	Events          int            `json:"events"`
	Patterns        int            `json:"patterns"` // distinct fingerprints, listed or not (a lower bound if untracked_events > 0)
	Highest         model.Severity `json:"highest_severity"`
	Held            int            `json:"held,omitempty"`
	CriticalHeld    int            `json:"critical_held,omitempty"`
	Undelivered     int            `json:"undelivered,omitempty"` // of Held: problems whose right-away alert failed to deliver
	Entries         []DigestEntry  `json:"entries"`               // ≤ 50
	OtherPatterns   int            `json:"other_patterns,omitempty"`
	OtherEvents     int            `json:"other_events,omitempty"`
	UntrackedEvents int            `json:"untracked_events,omitempty"`
	Retried         bool           `json:"retried,omitempty"`
	Link            string         `json:"link,omitempty"` // <public>/#/deliveries/<id>
}

type otherCount struct {
	Events     int       `json:"events"`
	Repeats    int       `json:"repeats"`
	LastRepeat time.Time `json:"last_repeat,omitzero"`
}

// digestBuffer is a channel's digest being collected. It is persisted.
type digestBuffer struct {
	Opened          time.Time               `json:"opened"`
	Due             time.Time               `json:"due"`
	Interval        int                     `json:"interval_minutes"`
	Routes          []string                `json:"routes"`          // union, ≤ 10
	Entries         map[string]*DigestEntry `json:"entries"`         // ≤ 200
	Other           map[string]*otherCount  `json:"other,omitempty"` // ≤ 5000
	UntrackedEvents int                     `json:"untracked_events,omitempty"`
	Events          int                     `json:"events"`
	Held            int                     `json:"held,omitempty"`
	CriticalHeld    int                     `json:"critical_held,omitempty"`
	Summary         bool                    `json:"summary,omitempty"`
	Retried         bool                    `json:"retried,omitempty"`
}

// PendingDigest is a channel's digest being collected, for the UI.
type PendingDigest struct {
	NotifierID   string         `json:"notifier_id"`
	NotifierName string         `json:"notifier_name"`
	NotifierType string         `json:"notifier_type"`
	Routes       []string       `json:"routes"`
	Opened       time.Time      `json:"opened"`
	Due          time.Time      `json:"due"`
	Events       int            `json:"events"`
	Patterns     int            `json:"patterns"`
	Held         int            `json:"held"`
	Undelivered  int            `json:"undelivered,omitempty"` // of Held: problems whose right-away alert failed to deliver
	Summary      bool           `json:"summary"`
	Disabled     bool           `json:"disabled,omitempty"` // the notifier is disabled: sent when it is enabled again
	Highest      model.Severity `json:"highest_severity"`
	Top          []DigestEntry  `json:"top"` // ≤ 5, with links
}

// entryAdd says how an event is added to a buffer.
type entryAdd struct {
	repeat      bool      // a repeat of the alert at alertedAt
	alertedAt   time.Time // the last alert for the problem, if any
	alertedAs   string    // its reason
	held        string    // the would-be right-away reason a limit held back (or of an undelivered alert)
	heldBy      string    //
	undelivered int       // > 0: this many events of an alert that failed to deliver
}

// addToDigest adds ev to notifier nid's buffer, opening one due in
// interval minutes.
func (a *alerting) addToDigest(nid string, ev *model.Event, sig *signal, interval int, routes []string, add entryAdd, now time.Time) *digestBuffer {
	if interval <= 0 {
		interval = defaultSmartDigest
	}
	buf := a.digests[nid]
	if buf == nil {
		buf = &digestBuffer{Opened: now, Interval: interval, Due: now.Add(time.Duration(interval) * time.Minute), Entries: map[string]*DigestEntry{}}
		a.digests[nid] = buf
	} else if interval < buf.Interval {
		buf.Interval = interval
		if due := buf.Opened.Add(time.Duration(interval) * time.Minute); due.Before(buf.Due) {
			buf.Due = due
		}
	}
	n := 1
	if add.undelivered > 0 {
		n = add.undelivered
	}
	buf.Events += n
	for _, r := range routes {
		if len(buf.Routes) < maxDigestRoutes && !containsString(buf.Routes, r) {
			buf.Routes = append(buf.Routes, r)
		}
	}
	fp := ev.Fingerprint
	e := buf.Entries[fp]
	// A fingerprint counted in Other stays there: in both, a fold could
	// miss repeats.
	if e == nil && buf.Other[fp] == nil && len(buf.Entries) < maxDigestEntries {
		e = &DigestEntry{Fingerprint: fp, SourceID: ev.SourceID, SourceName: ev.SourceName, Category: ev.Category, First: now}
		buf.Entries[fp] = e
	}
	if e == nil {
		o := buf.Other[fp]
		if o == nil && len(buf.Other) < maxDigestOther {
			if buf.Other == nil {
				buf.Other = map[string]*otherCount{}
			}
			o = &otherCount{}
			buf.Other[fp] = o
		}
		if o == nil {
			buf.UntrackedEvents += n
			return buf
		}
		o.Events += n
		if add.repeat {
			o.Repeats += n
			o.LastRepeat = now
		}
		return buf
	}
	e.Count += n
	if ev.Severity.Rank() >= e.Severity.Rank() {
		e.Severity, e.Sample, e.EventID = ev.Severity, model.Truncate(ev.Message, sampleBytes), ev.ID
	}
	e.Last = now
	if sig != nil {
		e.FirstSeen = sig.FirstSeen
		e.New = e.New || sig.New
	}
	if !add.alertedAt.IsZero() && !add.alertedAt.Before(e.AlertedAt) {
		e.AlertedAt, e.AlertedAs = add.alertedAt, add.alertedAs
	}
	switch {
	case add.repeat:
		e.Repeats += n
		e.LastRepeat = now
	case add.undelivered > 0:
		e.Undelivered += n
	}
	if add.held != "" && e.Held == "" {
		e.Held, e.HeldBy = add.held, add.heldBy
	}
	return buf
}

// peekRepeats counts the repeats of fp in nid's buffer, and when the
// newest was: the next alert for fp reports them.
func (a *alerting) peekRepeats(nid, fp string) (int, time.Time) {
	buf := a.digests[nid]
	if buf == nil {
		return 0, time.Time{}
	}
	n, last := 0, time.Time{}
	if e := buf.Entries[fp]; e != nil && e.Repeats > 0 {
		n, last = n+e.Repeats, laterOf(last, e.LastRepeat)
	}
	if o := buf.Other[fp]; o != nil && o.Repeats > 0 {
		n, last = n+o.Repeats, laterOf(last, o.LastRepeat)
	}
	return n, last
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// foldRepeats takes the repeats of fp out of nid's buffer: the alert being
// sent reports them, so the digest doesn't. Untracked events can't be
// folded (they may be reported twice, past 5,200 problems in one period).
func (a *alerting) foldRepeats(nid, fp string) {
	buf := a.digests[nid]
	if buf == nil {
		return
	}
	if e := buf.Entries[fp]; e != nil && e.Repeats > 0 {
		buf.Events -= e.Repeats
		e.Count -= e.Repeats
		e.Repeats, e.AlertedAt, e.AlertedAs, e.LastRepeat = 0, time.Time{}, "", time.Time{}
		if e.Count <= 0 {
			delete(buf.Entries, fp)
		}
	}
	if o := buf.Other[fp]; o != nil && o.Repeats > 0 {
		buf.Events -= o.Repeats
		o.Events -= o.Repeats
		o.Repeats, o.LastRepeat = 0, time.Time{}
		if o.Events <= 0 {
			delete(buf.Other, fp)
		}
	}
	if buf.Events <= 0 {
		delete(a.digests, nid)
	}
}

// mergeBack returns a digest that failed to deliver to nid's buffer, once:
// its events go out with the next one.
func (a *alerting) mergeBack(nid string, old *digestBuffer, now time.Time) {
	cur := a.digests[nid]
	if cur == nil {
		cur = &digestBuffer{Opened: old.Opened, Interval: old.Interval, Entries: map[string]*DigestEntry{}}
		cur.Due = now.Add(time.Duration(old.Interval) * time.Minute)
		a.digests[nid] = cur
	} else {
		if old.Opened.Before(cur.Opened) {
			cur.Opened = old.Opened
		}
		cur.Interval = min(cur.Interval, old.Interval)
		if due := now.Add(time.Duration(cur.Interval) * time.Minute); due.Before(cur.Due) {
			cur.Due = due
		}
	}
	if old.Held > 0 { // a summary keeps its promise: held criticals within a minute, the rest within minutes
		a.summarize(cur, nid, old.CriticalHeld > 0, now)
	}
	cur.Retried = true
	cur.Events += old.Events
	cur.UntrackedEvents += old.UntrackedEvents
	cur.Held += old.Held
	cur.CriticalHeld += old.CriticalHeld
	cur.Summary = cur.Summary || old.Summary
	for _, r := range old.Routes {
		if len(cur.Routes) < maxDigestRoutes && !containsString(cur.Routes, r) {
			cur.Routes = append(cur.Routes, r)
		}
	}
	addOther := func(fp string, events, repeats int, lastRepeat time.Time) {
		o := cur.Other[fp]
		if o == nil && len(cur.Other) < maxDigestOther {
			if cur.Other == nil {
				cur.Other = map[string]*otherCount{}
			}
			o = &otherCount{}
			cur.Other[fp] = o
		}
		if o == nil {
			cur.UntrackedEvents += events
			return
		}
		o.Events += events
		o.Repeats += repeats
		o.LastRepeat = laterOf(o.LastRepeat, lastRepeat)
	}
	for _, fp := range slices.Sorted(maps.Keys(old.Entries)) {
		oe := old.Entries[fp]
		e := cur.Entries[fp]
		if e == nil && (len(cur.Entries) >= maxDigestEntries || cur.Other[fp] != nil) {
			addOther(fp, oe.Count, oe.Repeats, oe.LastRepeat)
			continue
		}
		if e == nil {
			cp := *oe
			cur.Entries[fp] = &cp
			continue
		}
		e.Count += oe.Count
		e.Repeats += oe.Repeats
		e.Undelivered += oe.Undelivered
		if oe.Severity.Rank() > e.Severity.Rank() {
			e.Severity, e.Sample, e.EventID = oe.Severity, oe.Sample, oe.EventID
		}
		if oe.First.Before(e.First) {
			e.First = oe.First
		}
		if oe.Last.After(e.Last) {
			e.Last = oe.Last
		}
		e.New = e.New || oe.New
		if e.Held == "" {
			e.Held, e.HeldBy = oe.Held, oe.HeldBy
		}
		if e.AlertedAt.IsZero() {
			e.AlertedAt, e.AlertedAs = oe.AlertedAt, oe.AlertedAs
		}
		e.LastRepeat = laterOf(e.LastRepeat, oe.LastRepeat)
	}
	for _, fp := range slices.Sorted(maps.Keys(old.Other)) {
		o := old.Other[fp]
		if e := cur.Entries[fp]; e != nil { // listed now: one place per problem
			e.Count += o.Events
			e.Repeats += o.Repeats
			e.LastRepeat = laterOf(e.LastRepeat, o.LastRepeat)
			continue
		}
		addOther(fp, o.Events, o.Repeats, o.LastRepeat)
	}
}

// digested notes when digest msg reported repeats of the alert still
// open for each problem: the next alert counts its repeats from there.
func (a *alerting) digested(msg *Message) {
	if msg.buf == nil {
		return
	}
	note := func(fp string, repeats int) {
		if st := a.alerts[msg.nid+"|"+fp]; repeats > 0 && st != nil && st.LastSent.Before(msg.at) {
			st.Digested = msg.at
		}
	}
	for fp, e := range msg.buf.Entries {
		note(fp, e.Repeats)
	}
	for fp, o := range msg.buf.Other {
		note(fp, o.Repeats)
	}
}

// sortedEntries orders a buffer's entries: held first, then by severity,
// count and last time, all descending, then by fingerprint.
func sortedEntries(buf *digestBuffer) []*DigestEntry {
	out := slices.Collect(maps.Values(buf.Entries))
	slices.SortFunc(out, func(x, y *DigestEntry) int {
		if (x.Held != "") != (y.Held != "") {
			if x.Held != "" {
				return -1
			}
			return 1
		}
		if c := y.Severity.Rank() - x.Severity.Rank(); c != 0 {
			return c
		}
		if c := y.Count - x.Count; c != 0 {
			return c
		}
		if c := y.Last.Compare(x.Last); c != 0 {
			return c
		}
		return strings.Compare(x.Fingerprint, y.Fingerprint)
	})
	return out
}

// highest is the most severe entry's severity.
func (buf *digestBuffer) highest() model.Severity {
	var h model.Severity
	for _, e := range buf.Entries {
		if e.Severity.Rank() > h.Rank() {
			h = e.Severity
		}
	}
	return h
}

// undelivered counts the problems held because their right-away alert
// failed to deliver: sent again, not held back by flood protection.
func (buf *digestBuffer) undelivered() int {
	n := 0
	for _, e := range buf.Entries {
		if e.Held != "" && e.HeldBy == ReasonUndelivered {
			n++
		}
	}
	return n
}

// snapshot turns a buffer into the Digest that is sent.
func (d *Dispatcher) snapshot(buf *digestBuffer, reason string, now time.Time) *Digest {
	if reason == "" {
		reason = ReasonDigest
		if buf.Summary {
			reason = ReasonSummary
		}
	}
	dg := &Digest{
		ID: model.NewID(), Reason: reason, From: buf.Opened, To: now, IntervalMinutes: buf.Interval,
		Routes: slices.Clone(buf.Routes), Events: buf.Events, Patterns: len(buf.Entries) + len(buf.Other),
		Highest: buf.highest(), Held: buf.Held, CriticalHeld: buf.CriticalHeld, Undelivered: buf.undelivered(),
		UntrackedEvents: buf.UntrackedEvents, Retried: buf.Retried, Entries: []DigestEntry{},
	}
	for i, e := range sortedEntries(buf) {
		if i >= maxDigestListed {
			dg.OtherPatterns++
			dg.OtherEvents += e.Count
			continue
		}
		cp := *e
		cp.Link = d.fingerprintLink(e.Fingerprint)
		dg.Entries = append(dg.Entries, cp)
	}
	dg.OtherPatterns += len(buf.Other)
	for _, o := range buf.Other {
		dg.OtherEvents += o.Events
	}
	if d.publicURL != "" {
		dg.Link = d.publicURL + "/#/deliveries/" + dg.ID
	}
	return dg
}

func (d *Dispatcher) fingerprintLink(fp string) string {
	if d.publicURL == "" || fp == "" {
		return ""
	}
	return d.publicURL + "/#/events?fingerprint=" + fp
}

// digestEvent is a digest as one event, for webhook templates written for
// alerts: .Event.Severity is the highest severity, .Event.Message the text.
func digestEvent(m *Message) model.Event {
	dg := m.Digest
	sources := map[string]bool{}
	name := ""
	for _, e := range dg.Entries {
		if !sources[e.SourceName] {
			sources[e.SourceName] = true
			name = e.SourceName
		}
	}
	if len(sources) != 1 {
		name = plural(max(len(sources), 0), "source")
	}
	return model.Event{
		ID: dg.ID, SourceName: name, Severity: dg.Highest, Category: "digest", Fingerprint: dg.ID,
		Message: m.PlainText(), Timestamp: dg.To, ReceivedAt: dg.To,
	}
}

// sampleDigest is the test digest: one new problem, one held back and one
// repeating an alert.
func sampleDigest(now time.Time) *Digest {
	now = now.UTC()
	return &Digest{
		ID: model.NewID(), Reason: ReasonTest, From: now.Add(-30 * time.Minute), To: now, IntervalMinutes: 30,
		Routes: []string{"Test route"}, Events: 63, Patterns: 3, Highest: model.SeverityError, Held: 1,
		Entries: []DigestEntry{
			{Fingerprint: "0000000000000001", SourceName: "lissandra-test", Category: "database", Severity: model.SeverityError, Count: 1,
				First: now.Add(-12 * time.Minute), Last: now.Add(-12 * time.Minute), FirstSeen: now.Add(-12 * time.Minute),
				Sample: "Test: connection to the orders database refused.", EventID: "test", Held: ReasonNew, HeldBy: ReasonSourceLimit},
			{Fingerprint: "0000000000000002", SourceName: "lissandra-test", Category: "application", Severity: model.SeverityError, Count: 50, Repeats: 50,
				First: now.Add(-29 * time.Minute), Last: now.Add(-time.Minute), FirstSeen: now.Add(-3 * time.Hour), AlertedAt: now.Add(-31 * time.Minute),
				Sample: "Test: payment provider timed out after 30 s.", EventID: "test"},
			{Fingerprint: "0000000000000003", SourceName: "lissandra-test", Category: "network", Severity: model.SeverityWarning, Count: 12,
				First: now.Add(-25 * time.Minute), Last: now.Add(-2 * time.Minute), FirstSeen: now.Add(-25 * time.Minute),
				Sample: "Test: upstream responded slowly (2.4 s).", EventID: "test", New: true},
		},
	}
}

// flushDue sends every digest that is due at now; the ticker in Run calls
// it with d.now(). It sends nothing once Drain has finished.
func (d *Dispatcher) flushDue(now time.Time) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.amu.Lock()
	defer d.amu.Unlock()
	if d.closed {
		return
	}
	now = d.clampLocked(now)
	for _, nid := range slices.Sorted(maps.Keys(d.al.digests)) {
		if buf := d.al.digests[nid]; !now.Before(buf.Due) {
			d.flushLocked(nid, buf, "", now)
		}
	}
}

// flushAll sends every digest now, with reason: Drain does when alert
// state can't be saved for after the restart.
func (d *Dispatcher) flushAll(reason string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.amu.Lock()
	defer d.amu.Unlock()
	now := d.clock()
	for _, nid := range slices.Sorted(maps.Keys(d.al.digests)) {
		buf := d.al.digests[nid]
		if _, err := d.flushLocked(nid, buf, reason, now); errors.Is(err, ErrNotifierDisabled) && d.al.digests[nid] == buf {
			d.discard(nid, buf, "digest discarded at shutdown: its notifier is disabled and alert state can't be saved")
		}
	}
}

// Why "Send now" sent nothing.
var (
	ErrNothingBatched   = errors.New("nothing is batched for this notifier")
	ErrNotifierDisabled = errors.New("this notifier is disabled or was removed: enable it to send its digest")
	ErrQueueFull        = errors.New("this notifier's queue is full: the digest goes out on the next tick")
)

// FlushDigest sends notifier nid's digest now ("Send now"). It reports the
// digest's delivery ID, or why nothing was sent: nothing is batched for
// nid, its notifier is disabled or gone (the digest stays), or its queue
// is full.
func (d *Dispatcher) FlushDigest(nid string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.amu.Lock()
	defer d.amu.Unlock()
	buf := d.al.digests[nid]
	if buf == nil {
		return "", ErrNothingBatched
	}
	if ch := d.channels[nid]; ch == nil || ch.noDigest {
		return "", ErrNotifierDisabled
	}
	return d.flushLocked(nid, buf, ReasonManual, d.clock())
}

// flushLocked queues nid's buffer as a digest. A disabled notifier's buffer
// waits for it; one whose notifier is gone or no-digest is discarded (its
// events are lost); one that can't be queued stays for the next tick. When
// the digest carries held alerts, it is their alert: their repeats in the
// window go into later digests rather than being held again. d.mu (read)
// and d.amu are held.
func (d *Dispatcher) flushLocked(nid string, buf *digestBuffer, reason string, now time.Time) (string, error) {
	a := &d.al
	ch := d.channels[nid]
	if _, idle := d.idle[nid]; ch == nil && idle && buf.Events > 0 {
		return "", ErrNotifierDisabled
	}
	if ch == nil || ch.noDigest || buf.Events <= 0 {
		if buf.Events <= 0 {
			delete(a.digests, nid)
			a.dirty = true
			return "", ErrNothingBatched
		}
		d.discard(nid, buf, "digest discarded: its notifier is gone or takes no digests")
		return "", ErrNotifierDisabled
	}
	dg := d.snapshot(buf, reason, now)
	msg := &Message{ID: dg.ID, Kind: KindDigest, Digest: dg, Routes: dg.Routes, RouteName: strings.Join(dg.Routes, ", "),
		Link: dg.Link, at: now, nid: nid, buf: buf}
	msg.Reason = Reason{dg.Reason, digestText(dg, d.digestRoute(buf))}
	msg.Event = digestEvent(msg)
	d.pending.Add(1)
	if !d.enqueue(nid, job{ch: ch, msg: msg}) {
		d.pending.Add(-1)
		if !a.stuck[nid] {
			a.stuck[nid] = true
			d.log.Warn("notification queue full: the digest waits for the next tick", "notifier", ch.n.Name, "events", buf.Events)
		}
		return "", ErrQueueFull
	}
	delete(a.stuck, nid)
	delete(a.digests, nid)
	if buf.Held > 0 {
		a.summaries[nid] = now
		for _, fp := range slices.Sorted(maps.Keys(buf.Entries)) {
			if e := buf.Entries[fp]; e.Held != "" {
				a.seq++
				key := nid + "|" + fp
				st := a.alerts[key]
				if st == nil {
					st = &alertState{}
					a.alerts[key] = st
				}
				if msg.opened == nil {
					msg.opened = map[string]openedWindow{}
				}
				msg.opened[key] = openedWindow{seq: a.seq, prev: *st}
				st.LastSent, st.LastSeverity, st.Reason, st.Digested, st.Seq, st.Updated = now, e.Severity, e.Held, time.Time{}, a.seq, now
			}
		}
		if len(a.alerts) > maxAlertStates {
			a.evictAlerts(d)
		}
	}
	a.digestLog.put(dg)
	a.dirty = true
	return dg.ID, nil
}

// discard drops nid's buffer, whose events are lost.
func (d *Dispatcher) discard(nid string, buf *digestBuffer, why string) {
	delete(d.al.digests, nid)
	d.al.dirty = true
	d.DigestsDiscarded.Add(1)
	d.LostEvents.Add(uint64(buf.Events))
	d.log.Warn(why, "notifier", nid, "events", buf.Events)
}

// digestRoute is the route a buffer's interval comes from (for its Why).
func (d *Dispatcher) digestRoute(buf *digestBuffer) *route {
	var first *route
	for i := range d.routes {
		r := &d.routes[i]
		if r.digestMinutes() == 0 || !containsString(buf.Routes, r.Name) {
			continue
		}
		if r.digestMinutes() == buf.Interval {
			return r
		}
		if first == nil {
			first = r
		}
	}
	return first
}

// PendingDigests lists the digests being collected, soonest due first.
func (d *Dispatcher) PendingDigests() []PendingDigest {
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.amu.Lock()
	defer d.amu.Unlock()
	out := []PendingDigest{}
	for nid, buf := range d.al.digests {
		pd := PendingDigest{
			NotifierID: nid, Routes: slices.Clone(buf.Routes), Opened: buf.Opened, Due: buf.Due, Events: buf.Events,
			Patterns: len(buf.Entries) + len(buf.Other), Held: buf.Held, Undelivered: buf.undelivered(), Summary: buf.Summary, Highest: buf.highest(), Top: []DigestEntry{},
		}
		if ch := d.channels[nid]; ch != nil {
			pd.NotifierName, pd.NotifierType = ch.n.Name, ch.n.Type
		} else if name, idle := d.idle[nid]; idle {
			pd.NotifierName, pd.Disabled = name, true
		}
		for i, e := range sortedEntries(buf) {
			if i == 5 {
				break
			}
			cp := *e
			cp.Link = d.fingerprintLink(e.Fingerprint)
			pd.Top = append(pd.Top, cp)
		}
		out = append(out, pd)
	}
	slices.SortFunc(out, func(x, y PendingDigest) int {
		if c := x.Due.Compare(y.Due); c != 0 {
			return c
		}
		return strings.Compare(x.NotifierID, y.NotifierID)
	})
	return out
}
