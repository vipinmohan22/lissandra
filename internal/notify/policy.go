package notify

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// The alert decision layer: which matching events a channel gets right
// away, which wait for its digest, and the limits that keep a channel from
// being flooded. Every limit is a constant, documented in the README.
const (
	DefaultWarmup        = time.Hour          // per source, from its first event
	newAfter             = 7 * 24 * time.Hour // unseen this long: the pattern starts over and is new again
	promoteFloor         = model.SeverityError
	spikeWindow          = 5 * time.Minute
	spikeMin             = 20 // pattern >= baselineAge: c5 >= max(spikeMin, spikeFactor*usual/12)
	spikeFactor          = 10.0
	floodMin             = 50 // pattern < baselineAge: c5 >= floodMin
	baselineAge          = time.Hour
	averageAge           = 24 * time.Hour // younger: usual = total/max(1h, age); older: EWMA
	ewmaAlpha            = 0.1
	spikeEvery           = time.Hour        // one spike decision per pattern per hour
	spikeRealert         = 15 * time.Minute // a (non-flood) spike breaks a window this old
	routineFactor        = 1.5              // a burst at the time of the last one on an earlier day spikes only above this times its peak
	backlogAge           = 5 * time.Minute  // events stamped older don't count toward spikes or bursts
	burstMin             = 20               // auth or security events at warning and up from one source in spikeWindow
	burstEvery           = time.Hour        // one burst per source and category per hour
	quietAfter           = time.Hour        // repeats reported this long after the last one read "the last at …"
	limitWindow          = 10 * time.Minute
	channelLimit         = 10 // non-critical Smart right-away alerts per channel per window
	sourceLimit          = 1  // promotions per channel per (source, category) per window
	criticalLimit        = 20 // critical Smart right-away alerts per digest-capable channel per window
	summaryDelay         = 2 * time.Minute
	criticalSummaryDelay = time.Minute
	maxPatterns          = 20000 // LRU by Last; evict 10% when full
	patternTTL           = 8 * 24 * time.Hour
	maxAlertStates       = 20000
	alertStateTTL        = 24 * time.Hour     // nothing pending
	pendingTTL           = 7 * 24 * time.Hour // repeats pending (no-digest channels)
	idleDigestTTL        = 7 * 24 * time.Hour // a disabled notifier's digest waits this long for it
	maxDigestEntries     = 200                // per buffer; then per-fingerprint counts only
	maxDigestOther       = 5000               // fingerprints counted beyond the entries; then totals only
	maxDigestListed      = 50                 // entries kept in a sent Digest (renderers show fewer)
	maxDigestRoutes      = 10
	sampleBytes          = 300
	maxDecisionEvents    = 5000 // = the default event buffer (LISSANDRA_EVENT_BUFFER)
	digestLogSize        = 50   // digests kept in full for the Deliveries page
	flushTick            = 10 * time.Second
	saveEvery            = time.Minute
	pruneEvery           = 5 * time.Minute
	stateVersion         = 1

	defaultSmartDigest  = 30 // minutes, for a smart route saved before normalization filled it
	defaultDigestDigest = 60
)

// burstCategories are the categories whose warnings count toward a burst:
// failed logins and attacks matter in numbers, one at a time they don't.
var burstCategories = map[string]bool{"auth": true, "security": true}

// Reason codes. Right away: every, severity, new, spike, worse, burst,
// repeat. Digest messages: digest, summary, manual, shutdown, test.
// Decisions only: throttled, source_limit, channel_limit, critical_limit,
// queue_full, no_digest. Digest entries only (HeldBy): undelivered.
const (
	ReasonEvery    = "every"
	ReasonSeverity = "severity"
	ReasonNew      = "new"
	ReasonSpike    = "spike"
	ReasonWorse    = "worse"
	ReasonBurst    = "burst"
	ReasonRepeat   = "repeat"

	ReasonDigest   = "digest"
	ReasonSummary  = "summary"
	ReasonManual   = "manual"
	ReasonShutdown = "shutdown"
	ReasonTest     = "test"

	ReasonThrottled     = "throttled"
	ReasonSourceLimit   = "source_limit"
	ReasonChannelLimit  = "channel_limit"
	ReasonCriticalLimit = "critical_limit"
	ReasonQueueFull     = "queue_full"
	ReasonNoDigest      = "no_digest"
	ReasonUndelivered   = "undelivered" // a right-away alert failed to deliver: its summary sends it again
)

// Decision actions. Preview uses ActionRightAway, ActionDigest and
// ActionSkipped.
const (
	ActionSent      = "sent"
	ActionDigest    = "digest"
	ActionThrottled = "throttled"
	ActionHeld      = "held"
	ActionSkipped   = "skipped"
	ActionDropped   = "dropped"
	ActionRightAway = "right_away"
)

// Actions names Dispatcher.Decided's counters, in order.
var Actions = [...]string{ActionSent, ActionDigest, ActionThrottled, ActionHeld, ActionSkipped, ActionDropped}

// RightAwayReasons names Dispatcher.RightAway's counters, in order.
var RightAwayReasons = [...]string{ReasonEvery, ReasonSeverity, ReasonNew, ReasonSpike, ReasonWorse, ReasonBurst, ReasonRepeat}

func indexOf(names []string, s string) int { return slices.Index(names, s) }

// Reason says why a message was sent, or a decision made.
type Reason struct {
	Code string `json:"code"`
	Text string `json:"text"` // one sentence (sometimes two), shown as "Why"
}

// Decision is what one event did on one channel.
type Decision struct {
	NotifierID   string    `json:"notifier_id"`
	NotifierName string    `json:"notifier_name"`
	NotifierType string    `json:"notifier_type"`
	Routes       []string  `json:"routes"`
	Action       string    `json:"action"` // sent | digest | throttled | held | skipped | dropped (preview: right_away | digest | skipped)
	Reason       Reason    `json:"reason"`
	Due          time.Time `json:"due,omitzero"` // when the digest holding it is due
}

type tier int

const (
	tierSkip tier = iota
	tierDigest
	tierNow
)

// immediateSeverity is the route's "right away at or above" level.
func (r *route) immediateSeverity() model.Severity {
	if r.ImmediateSeverity.Valid() {
		return r.ImmediateSeverity
	}
	return model.SeverityCritical
}

// digestMinutes is the route's digest interval; 0 for immediate routes.
func (r *route) digestMinutes() int {
	switch r.Delivery {
	case config.DeliverySmart:
		if r.DigestMinutes > 0 {
			return r.DigestMinutes
		}
		return defaultSmartDigest
	case config.DeliveryDigest:
		if r.DigestMinutes > 0 {
			return r.DigestMinutes
		}
		return defaultDigestDigest
	}
	return 0
}

func (r *route) immediate() bool {
	return r.Delivery != config.DeliverySmart && r.Delivery != config.DeliveryDigest
}

// want is what the route asks for ev on one channel.
func (r *route) want(ev *model.Event, s *signal, noDigest bool) (tier, string) {
	switch r.Delivery {
	case config.DeliveryDigest:
		if noDigest {
			return tierSkip, ""
		}
		return tierDigest, ReasonDigest
	case config.DeliverySmart:
		if ev.Severity.AtLeast(r.immediateSeverity()) {
			return tierNow, ReasonSeverity
		}
		if !noDigest && ev.Severity.AtLeast(promoteFloor) {
			switch {
			case s.New:
				return tierNow, ReasonNew
			case s.Spike:
				return tierNow, ReasonSpike
			case s.Worse:
				return tierNow, ReasonWorse
			}
		}
		if !noDigest && s.Burst {
			return tierNow, ReasonBurst
		}
		if noDigest {
			return tierSkip, ""
		}
		return tierDigest, ReasonDigest
	default: // "" and immediate
		return tierNow, ReasonEvery
	}
}

// reasonRank orders right-away reasons: the strongest one is told.
func reasonRank(code string) int {
	switch code {
	case ReasonNew:
		return 6
	case ReasonSpike:
		return 5
	case ReasonWorse:
		return 4
	case ReasonBurst:
		return 3
	case ReasonSeverity:
		return 2
	case ReasonEvery:
		return 1
	}
	return 0
}

// plan merges what every matching route wants for one notifier, so a
// channel gets at most one message per event.
type plan struct {
	tier      tier
	reason    string        // strongest right-away reason: new > spike > worse > burst > severity > every
	route     *route        // the route that gave it (texts)
	routes    []string      // every matching route/group name for this notifier, config order, deduplicated
	throttle  time.Duration // min ThrottleSeconds over routes wanting tierNow
	nowRoutes int
	digestMin int    // min DigestMinutes over matching smart/digest routes; 0 = none
	digestRt  *route // the route that set digestMin (texts)
	holdable  bool   // no immediate route wants tierNow (legacy sends are never held or counted)
	smart     bool   // a smart route wants tierNow (spike may break a window)
}

// interval is the digest interval held and undelivered events wait for.
func (p *plan) interval() int {
	if p.digestMin > 0 {
		return p.digestMin
	}
	return defaultSmartDigest
}

// plans returns the plan per notifier for ev, in first-seen order. matching
// indexes d.routes; d.mu is held.
func (d *Dispatcher) plans(ev *model.Event, sig *signal, matching []int) (map[string]*plan, []string) {
	ps := map[string]*plan{}
	var order []string
	for _, i := range matching {
		r := &d.routes[i]
		for _, nid := range r.NotifierIDs {
			ch, ok := d.channels[nid]
			if !ok {
				continue
			}
			p := ps[nid]
			if p == nil {
				p = &plan{holdable: true}
				ps[nid] = p
				order = append(order, nid)
			}
			if !containsString(p.routes, r.Name) {
				p.routes = append(p.routes, r.Name)
			}
			if dm := r.digestMinutes(); dm > 0 && (p.digestMin == 0 || dm < p.digestMin) {
				p.digestMin, p.digestRt = dm, r
			}
			t, code := r.want(ev, sig, ch.noDigest)
			if t == tierNow {
				th := time.Duration(max(r.ThrottleSeconds, 0)) * time.Second
				if p.nowRoutes == 0 || th < p.throttle {
					p.throttle = th
				}
				p.nowRoutes++
				if r.immediate() {
					p.holdable = false
				} else {
					p.smart = true
				}
			}
			switch {
			case t > p.tier, p.route == nil && t == p.tier,
				t == tierNow && p.tier == tierNow && reasonRank(code) > reasonRank(p.reason):
				p.tier, p.reason, p.route = t, code, r
			}
		}
	}
	return ps, order
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// decide applies p to one channel and reports the right-away reason it
// told (sent or held; "" when it batched, counted or skipped the event).
// d.mu (read) and d.amu are held.
func (d *Dispatcher) decide(ev *model.Event, sig *signal, nid string, ch *channel, p *plan, now time.Time) (Decision, string) {
	a := &d.al
	dec := Decision{NotifierID: nid, NotifierName: ch.n.Name, NotifierType: ch.n.Type, Routes: p.routes}
	key := nid + "|" + ev.Fingerprint
	switch p.tier {
	case tierSkip:
		d.decided(ActionSkipped)
		dec.Action, dec.Reason = ActionSkipped, Reason{ReasonNoDigest, skippedText(ch, p)}
		return dec, ""
	case tierDigest:
		add := entryAdd{}
		if st := a.alerts[key]; st != nil && !st.LastSent.IsZero() { // the digest says it was alerted
			add.alertedAt, add.alertedAs = st.LastSent, st.Reason
		}
		buf := a.addToDigest(nid, ev, sig, p.digestMin, p.routes, add, now)
		d.decided(ActionDigest)
		dec.Action, dec.Due = ActionDigest, buf.Due
		dec.Reason = Reason{ReasonDigest, fmt.Sprintf("In the digest due %s. %s", fmtClock(buf.Due, now), routePolicy(p.digestRt))}
		return dec, ""
	}

	st := a.alerts[key]
	code := p.reason
	windowWorse := false
	if st != nil && !st.LastSent.IsZero() && p.throttle > 0 && now.Sub(st.LastSent) < p.throttle {
		switch {
		case ev.Severity.Rank() > st.LastSeverity.Rank():
			code, windowWorse = ReasonWorse, true
		case p.smart && !ch.noDigest && sig.Spike && !sig.Flood && now.Sub(st.LastSent) >= spikeRealert: // PagerDuty gets no promotions
			code = ReasonSpike
		default: // a repeat: reported once, in the digest or with the next alert
			d.Throttled.Add(1)
			d.decided(ActionThrottled)
			dec.Action = ActionThrottled
			if p.digestMin > 0 && !ch.noDigest {
				buf := a.addToDigest(nid, ev, sig, p.digestMin, p.routes, entryAdd{repeat: true, alertedAt: st.LastSent, alertedAs: st.Reason}, now)
				dec.Due = buf.Due
				dec.Reason = Reason{ReasonThrottled, fmt.Sprintf("Repeat of the alert at %s: in the digest due %s.", fmtClock(st.LastSent, now), fmtClock(buf.Due, now))}
			} else {
				st.addPending(1, now, now)
				dec.Reason = Reason{ReasonThrottled, fmt.Sprintf("Repeat of the alert at %s: counted, and reported with the next alert after %s.",
					fmtClock(st.LastSent, now), fmtClock(st.LastSent.Add(p.throttle), now))}
			}
			return dec, ""
		}
	}
	if p.holdable && !ch.noDigest {
		if by := d.limit(nid, ev, code, now); by != "" {
			return d.hold(nid, ev, sig, p, code, by, now), code
		}
	}
	n, last := a.peekRepeats(nid, ev.Fingerprint)
	if st != nil && st.Pending > 0 {
		n, last = n+st.Pending, laterOf(last, st.LastRepeat)
	}
	if st != nil && n > 0 && (code == ReasonEvery || code == ReasonSeverity) {
		code = ReasonRepeat
	}
	msg := &Message{
		ID: model.NewID(), Kind: KindAlert, Event: *ev, Routes: p.routes, RouteName: strings.Join(p.routes, ", "),
		Suppressed: n, Link: d.eventLink(ev.ID),
		at: now, nid: nid, fp: ev.Fingerprint, seq: a.seq + 1,
	}
	if n > 0 {
		msg.LastRepeat = last
	}
	if st != nil {
		msg.Since, msg.prevSent, msg.prevSev, msg.prevWhy, msg.prevSeq, msg.prev = st.LastSent, st.LastSent, st.LastSeverity, st.Reason, st.Seq, a.inflight[st.Seq]
		if n > 0 && st.Digested.After(st.LastSent) {
			msg.SinceDigest = st.Digested
		}
	}
	if p.digestMin > 0 && !ch.noDigest {
		msg.digestMin = p.digestMin
	}
	msg.Reason = Reason{code, d.alertText(code, ev, sig, p, ch, st, windowWorse, n, last, now)}
	d.pending.Add(1)
	if !d.enqueue(nid, job{ch: ch, msg: msg}) {
		d.pending.Add(-1)
		if !ch.noDigest { // no window opens: the next event alerts
			return d.hold(nid, ev, sig, p, code, ReasonQueueFull, now), code
		}
		if n := d.Dropped.Add(1); n == 1 || n%100 == 0 {
			d.log.Warn("notification queue full: alerts dropped", "notifier", ch.n.Name, "dropped_total", n)
		}
		d.record(ch, msg, "dropped", "notification queue full", 0, 0)
		if st == nil {
			st = &alertState{}
			a.alerts[key] = st
		}
		st.addPending(1, now, now)
		d.decided(ActionDropped)
		dec.Action, dec.Reason = ActionDropped, Reason{ReasonQueueFull, "Not sent: the notification queue was full. Counted, and reported with the next alert."}
		return dec, ""
	}
	a.seq++
	a.inflight[a.seq] = msg
	a.foldRepeats(nid, ev.Fingerprint) // reported by this alert's Suppressed
	a.alerts[key] = &alertState{LastSent: now, LastSeverity: ev.Severity, Reason: code, Seq: a.seq, Updated: now}
	if len(a.alerts) > maxAlertStates {
		a.evictAlerts(d)
	}
	if p.holdable && !ch.noDigest {
		if ev.Severity.AtLeast(model.SeverityCritical) {
			a.critical[nid] = append(a.critical[nid], now)
		} else {
			a.recent[nid] = append(a.recent[nid], now)
			if promotes(code) {
				a.promoted[promotedKey(nid, ev)] = now
			}
		}
	}
	if i := indexOf(RightAwayReasons[:], code); i >= 0 {
		d.RightAway[i].Add(1)
	}
	d.decided(ActionSent)
	dec.Action, dec.Reason = ActionSent, msg.Reason
	return dec, code
}

func (d *Dispatcher) decided(action string) {
	if i := indexOf(Actions[:], action); i >= 0 {
		d.Decided[i].Add(1)
	}
}

func promotes(code string) bool {
	return code == ReasonNew || code == ReasonSpike || code == ReasonWorse || code == ReasonBurst
}

func promotedKey(nid string, ev *model.Event) string {
	return nid + "|" + ev.SourceID + "|" + ev.Category
}

// within drops the times older than limitWindow from a time-ordered list
// and returns what is left.
func within(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= limitWindow {
		i++
	}
	return ts[i:]
}

// limit reports which flood limit, if any, holds back a right-away alert.
func (d *Dispatcher) limit(nid string, ev *model.Event, code string, now time.Time) string {
	a := &d.al
	if ev.Severity.AtLeast(model.SeverityCritical) {
		a.critical[nid] = within(a.critical[nid], now)
		if len(a.critical[nid]) >= criticalLimit {
			return ReasonCriticalLimit
		}
		return ""
	}
	if promotes(code) {
		if t, ok := a.promoted[promotedKey(nid, ev)]; ok && now.Sub(t) < limitWindow {
			return ReasonSourceLimit
		}
	}
	a.recent[nid] = within(a.recent[nid], now)
	if len(a.recent[nid]) >= channelLimit {
		return ReasonChannelLimit
	}
	return ""
}

// hold puts a right-away alert a limit held back into the channel's
// buffer, and brings the buffer forward so it waits at most 10 minutes.
func (d *Dispatcher) hold(nid string, ev *model.Event, sig *signal, p *plan, code, by string, now time.Time) Decision {
	buf := d.al.holdIn(nid, ev, sig, p.interval(), p.routes, entryAdd{held: code, heldBy: by}, by == ReasonCriticalLimit, now)
	d.decided(ActionHeld)
	ch := d.channels[nid]
	dec := Decision{NotifierID: nid, NotifierName: ch.n.Name, NotifierType: ch.n.Type, Routes: p.routes, Action: ActionHeld, Due: buf.Due}
	dueAt := fmtClock(buf.Due, now)
	switch by {
	case ReasonSourceLimit:
		dec.Reason = Reason{by, fmt.Sprintf("Held back: %s already had a right-away alert here in the last %s. In the summary due %s.", sourceCategory(ev.SourceName, ev.Category), fmtDur(limitWindow), dueAt)}
	case ReasonChannelLimit:
		dec.Reason = Reason{by, fmt.Sprintf("Held back: this channel had %d right-away alerts below critical in the last %s. In the summary due %s.", channelLimit, fmtDur(limitWindow), dueAt)}
	case ReasonCriticalLimit:
		dec.Reason = Reason{by, fmt.Sprintf("Held back: this channel had %d critical alerts in the last %s. In the summary due %s, within a minute.", criticalLimit, fmtDur(limitWindow), dueAt)}
	default:
		dec.Reason = Reason{by, fmt.Sprintf("Held back: the channel's queue was full. In the summary due %s.", dueAt)}
	}
	return dec
}

// holdIn adds an alert that didn't go out right away (held, or
// undelivered) to nid's buffer, counting its problem once in Held, and
// brings the buffer forward: a summary due in 2 minutes and at least 10
// after the channel's last one, or in 1 minute for a critical.
func (a *alerting) holdIn(nid string, ev *model.Event, sig *signal, interval int, routes []string, add entryAdd, critical bool, now time.Time) *digestBuffer {
	held := false
	if b := a.digests[nid]; b != nil && b.Entries[ev.Fingerprint] != nil {
		held = b.Entries[ev.Fingerprint].Held != ""
	}
	buf := a.addToDigest(nid, ev, sig, interval, routes, add, now)
	if !held {
		buf.Held++
	}
	if critical {
		buf.CriticalHeld++
	}
	a.summarize(buf, nid, critical, now)
	return buf
}

// summarize makes nid's buffer a summary due in 2 minutes, and at least 10
// after the channel's last summary, or in 1 minute for held criticals,
// unless it is due sooner.
func (a *alerting) summarize(buf *digestBuffer, nid string, critical bool, now time.Time) {
	due := now.Add(summaryDelay)
	if critical {
		due = now.Add(criticalSummaryDelay)
	} else if next := a.summaries[nid].Add(limitWindow); next.After(due) {
		due = next
	}
	if due.Before(buf.Due) {
		buf.Due, buf.Summary = due, true
	}
}

func sourceCategory(source, category string) string {
	switch {
	case source == "":
		return category
	case category == "":
		return source
	}
	return source + " · " + category
}

// skippedText explains why a no-digest channel (PagerDuty) gets nothing.
func skippedText(ch *channel, p *plan) string {
	label := ch.n.Type
	if pl, ok := PluginFor(ch.n.Type); ok {
		label = pl.Label
	}
	here := "this route sends nothing right away"
	if p.route != nil && p.route.Delivery == config.DeliverySmart {
		here = string(p.route.immediateSeverity()) + " and up"
	}
	return fmt.Sprintf("%s is %s: it gets only what is sent right away (here: %s), never digests.", ch.n.Name, label, here)
}

// alertText is the Why of a right-away alert; n repeats are reported with
// it, the newest at last.
func (d *Dispatcher) alertText(code string, ev *model.Event, sig *signal, p *plan, ch *channel, st *alertState, windowWorse bool, n int, last, now time.Time) string {
	var b strings.Builder
	switch code {
	case ReasonEvery:
		fmt.Fprintf(&b, "Matches %q, which sends every matching event right away.", routeName(p.route))
		if p.throttle > 0 {
			fmt.Fprintf(&b, " Repeats are held back for %s.", fmtDur(p.throttle))
		}
	case ReasonSeverity:
		imm := p.route.immediateSeverity()
		fmt.Fprintf(&b, "%s: at or above \"right away\" (%s) on %q.", capitalize(string(ev.Severity)), imm, routeName(p.route))
		switch {
		case sig.New:
			fmt.Fprintf(&b, " It is also new: nothing like it %s.", lookedBack(ev, sig, now))
		case sig.Spike:
			fmt.Fprintf(&b, " It is also spiking: %d in the last %d minutes.", sig.C5, int(spikeWindow/time.Minute))
		case sig.Burst:
			fmt.Fprintf(&b, " It is also part of a burst: %s.", burstCount(ev, sig))
		}
		b.WriteString(repeatsSentence(p, ch))
	case ReasonNew:
		if !sig.LastSeen.IsZero() {
			fmt.Fprintf(&b, "Back after %s: last seen %s.", fmtDays(now.Sub(sig.LastSeen)), fmtClock(sig.LastSeen, now))
		} else {
			fmt.Fprintf(&b, "New problem: first seen just now; nothing like it %s.", lookedBack(ev, sig, now))
		}
	case ReasonSpike:
		b.WriteString(spikeText(sig, now))
	case ReasonBurst:
		fmt.Fprintf(&b, "Burst: %s.", burstCount(ev, sig))
	case ReasonWorse:
		if windowWorse && st != nil {
			fmt.Fprintf(&b, "Got worse: now %s; the alert at %s was %s.", ev.Severity, fmtClock(st.LastSent, now), st.LastSeverity)
		} else {
			fmt.Fprintf(&b, "Got worse: now %s; it was at most %s %s.", ev.Severity, sig.PrevMax, lookedBack(ev, sig, now))
		}
	case ReasonRepeat:
		b.WriteString(stillHappening(n, st, last, now))
	}
	return b.String()
}

// lookedBack is how far back "new" and "worse" look: 7 days, or since
// Lissandra first saw the source, when that is more recent (a fresh
// install, a reset alert state).
func lookedBack(ev *model.Event, sig *signal, now time.Time) string {
	if !sig.Watched.IsZero() && now.Sub(sig.Watched) < newAfter {
		return fmt.Sprintf("since Lissandra started watching %s at %s", ev.SourceName, fmtClock(sig.Watched, now))
	}
	return "in the last " + fmtDays(newAfter)
}

// burstCount says how many events made a burst: "20 auth warnings or worse
// from node-syslog in the last 5 minutes".
func burstCount(ev *model.Event, sig *signal) string {
	return fmt.Sprintf("%d %s warnings or worse from %s in the last %d minutes", sig.BurstN, ev.Category, ev.SourceName, int(spikeWindow/time.Minute))
}

// repeatsSentence says where repeats of a severity alert go.
func repeatsSentence(p *plan, ch *channel) string {
	if p.throttle <= 0 {
		return ""
	}
	if p.digestMin > 0 && !ch.noDigest {
		return fmt.Sprintf(" Repeats for the next %s go into the digest.", fmtDur(p.throttle))
	}
	return fmt.Sprintf(" Repeats for the next %s are counted and reported with the next alert.", fmtDur(p.throttle))
}

func spikeText(sig *signal, now time.Time) string {
	if sig.Flood {
		return fmt.Sprintf("Flood: %d in the last %d minutes from a problem first seen at %s.", sig.C5, int(spikeWindow/time.Minute), fmtClock(sig.FirstSeen, now))
	}
	usual := "less than 1 an hour"
	if sig.Usual >= 1 {
		usual = fmt.Sprintf("about %d an hour", int(math.Round(sig.Usual)))
	}
	return fmt.Sprintf("Spiking: %d in the last %d minutes, usually %s.", sig.C5, int(spikeWindow/time.Minute), usual)
}

// stillHappening reports n repeats, the newest at last. Repeats that
// stopped long ago are not "still happening": the problem is back. They
// count from the digest that reported the earlier ones, if any.
func stillHappening(n int, st *alertState, last, now time.Time) string {
	if st == nil || st.LastSent.IsZero() {
		return fmt.Sprintf("Still happening: %d more since the last alert.", n)
	}
	var digested time.Time
	if st.Digested.After(st.LastSent) {
		digested = st.Digested
	}
	if quietSince(last, now) {
		return "Happening again: " + repeatsAfter(n, st.LastSent, digested, last, now) + "."
	}
	return fmt.Sprintf("Still happening: %d more since %s.", n, sinceWhat(st.LastSent, digested, now))
}

// sinceWhat is "the alert at 11:00 UTC", or "the digest at 11:32 UTC" when
// a digest reported the repeats before it.
func sinceWhat(alert, digest, now time.Time) string {
	if !digest.IsZero() {
		return "the digest at " + fmtClock(digest, now)
	}
	return "the alert at " + fmtClock(alert, now)
}

// quietSince reports whether the newest repeat, at last, was long enough
// before now that the repeats read as over.
func quietSince(last, now time.Time) bool { return !last.IsZero() && now.Sub(last) >= quietAfter }

// repeatsAfter is "11 more after the alert at Oct 27 11:18 UTC, the last
// at Oct 27 11:19 UTC" (after the digest at …, when digest isn't zero).
func repeatsAfter(n int, alert, digest, last, now time.Time) string {
	return fmt.Sprintf("%d more after %s, the last at %s", n, sinceWhat(alert, digest, now), fmtClock(last, now))
}

func routeName(r *route) string {
	if r == nil {
		return ""
	}
	return r.Name
}

// routePolicy is policySentence for a route.
func routePolicy(r *route) string {
	if r == nil {
		return ""
	}
	return policySentence(r.DeliveryPolicy, r.MinSeverity, time.Duration(r.ThrottleSeconds)*time.Second)
}

// digestText is the Why of a digest message.
func digestText(dg *Digest, r *route) string {
	switch dg.Reason {
	case ReasonSummary:
		return summaryText(dg)
	case ReasonManual:
		return `Sent on request ("Send now").`
	case ReasonShutdown:
		return "Sent at shutdown: alert state couldn't be saved for after the restart."
	case ReasonTest:
		return "Test notification."
	}
	every := fmtDur(time.Duration(dg.IntervalMinutes) * time.Minute)
	if r == nil { // its route changed since: name the routes that filled it
		if len(dg.Routes) == 0 {
			return fmt.Sprintf("Batched every %s.", every)
		}
		names := make([]string, len(dg.Routes))
		for i, n := range dg.Routes {
			names[i] = strconv.Quote(n)
		}
		return fmt.Sprintf("Batched every %s for %s.", every, joinAnd(names))
	}
	return strings.TrimSpace(fmt.Sprintf("Batched every %s on %q. %s", every, routeName(r), routePolicy(r)))
}

// summaryText names what held a summary's alerts back, one clause per
// limit that did.
func summaryText(dg *Digest) string {
	var pairs []string
	by := map[string]bool{}
	for _, e := range dg.Entries {
		if e.HeldBy == "" {
			continue
		}
		by[e.HeldBy] = true
		if sc := sourceCategory(e.SourceName, e.Category); e.HeldBy == ReasonSourceLimit && !containsString(pairs, sc) {
			pairs = append(pairs, sc)
		}
	}
	var clauses []string
	if by[ReasonSourceLimit] {
		list := pairs
		if len(list) > 3 {
			list = append(list[:3:3], fmt.Sprintf("%d more", len(pairs)-3))
		}
		verb := "already had a right-away alert here"
		if len(pairs) > 1 {
			verb = "each already had a right-away alert here"
		}
		clauses = append(clauses, joinAnd(list)+" "+verb)
	}
	if by[ReasonChannelLimit] {
		clauses = append(clauses, fmt.Sprintf("%d alerts below critical in %s", channelLimit, fmtDur(limitWindow)))
	}
	if by[ReasonCriticalLimit] {
		clauses = append(clauses, fmt.Sprintf("more than %d critical alerts in %s", criticalLimit, fmtDur(limitWindow)))
	}
	if by[ReasonQueueFull] {
		clauses = append(clauses, "the channel's queue was full")
	}
	const retried = "Sent again: right-away alerts that failed to deliver."
	if by[ReasonUndelivered] && len(clauses) == 0 {
		return retried
	}
	head := "Held back so this channel isn't flooded"
	if len(clauses) > 0 {
		head += ": " + strings.Join(clauses, "; ")
	}
	if by[ReasonUndelivered] {
		head += ". " + strings.TrimSuffix(retried, ".")
	}
	return fmt.Sprintf("%s. The first %d critical alerts in any %d minutes are never held.", head, criticalLimit, int(limitWindow/time.Minute))
}

func joinAnd(list []string) string {
	if len(list) <= 1 {
		return strings.Join(list, "")
	}
	return strings.Join(list[:len(list)-1], ", ") + " and " + list[len(list)-1]
}

// policySentence is the one Go source of the policy wording. The UI's
// policyText (web/static/app.js) mirrors it: change both together.
func policySentence(p config.DeliveryPolicy, minSev model.Severity, throttle time.Duration) string {
	if !minSev.Valid() {
		minSev = model.SeverityError
	}
	switch p.Delivery {
	case config.DeliverySmart:
		imm := p.ImmediateSeverity
		if !imm.Valid() {
			imm = model.SeverityCritical
		}
		every := fmtDur(time.Duration(cmp0(p.DigestMinutes, defaultSmartDigest)) * time.Minute)
		floor := minSev
		if floor.Rank() < promoteFloor.Rank() {
			floor = promoteFloor
		}
		// bursts of warnings reach routes that match warnings and don't
		// send them right away anyway
		bursts := minSev.Rank() <= model.SeverityWarning.Rank() && imm.Rank() > model.SeverityWarning.Rank()
		switch {
		case imm.Rank() > floor.Rank():
			s := fmt.Sprintf("Right away: %s and up", imm)
			switch {
			case bursts:
				s += ", errors that are new, spiking or getting worse, and bursts of auth or security warnings"
			case minSev.Rank() <= promoteFloor.Rank():
				s += ", and errors that are new, spiking or getting worse"
			}
			return fmt.Sprintf("%s. Everything else from %s up: one digest every %s.", s, minSev, every)
		case imm.Rank() <= minSev.Rank():
			return fmt.Sprintf("Everything from %s up right away, with flood protection; repeats in a digest every %s.", minSev, every)
		default:
			s := fmt.Sprintf("Right away: %s and up", imm)
			if bursts {
				s += ", and bursts of auth or security warnings"
			}
			return fmt.Sprintf("%s. Everything else from %s up: one digest every %s.", s, minSev, every)
		}
	case config.DeliveryDigest:
		return fmt.Sprintf("Everything this route matches: one digest every %s.", fmtDur(time.Duration(cmp0(p.DigestMinutes, defaultDigestDigest))*time.Minute))
	}
	if throttle > 0 {
		return fmt.Sprintf("Every matching event right away; repeats of a problem are held back for %s.", fmtDur(throttle))
	}
	return "Every matching event right away."
}

// cmp0 returns v, or def when v is 0.
func cmp0(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// fmtClock formats t as "15:04 UTC", or "Oct 17 15:04 UTC" when it isn't
// on now's day (UTC).
func fmtClock(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04 UTC")
	}
	return t.Format("Jan 2 15:04 UTC")
}

// fmtSpan formats a period such as "10:00–10:30 UTC".
func fmtSpan(from, to, now time.Time) string {
	from, to = from.UTC(), to.UTC()
	if from.Truncate(time.Minute).Equal(to.Truncate(time.Minute)) {
		return fmtClock(from, now)
	}
	if from.YearDay() == to.YearDay() && from.Year() == to.Year() {
		return strings.TrimSuffix(fmtClock(from, now), " UTC") + "–" + to.Format("15:04 UTC")
	}
	return strings.TrimSuffix(fmtClock(from, now), " UTC") + "–" + fmtClock(to, now)
}

// fmtDur formats a duration as "30 s", "15 min", "1 h" or "1 h 30 min".
func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	m := int(d.Round(time.Minute).Minutes())
	switch h := m / 60; {
	case h == 0:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%d h", h)
	default:
		return fmt.Sprintf("%d h %d min", h, m%60)
	}
}

// fmtDays formats a long duration in whole days: "7 days", "1 day".
func fmtDays(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}
