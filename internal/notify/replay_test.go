package notify

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// reported counts what channel nid was told about: alerts (each with its
// Suppressed) and digests delivered, plus what is still pending in alert
// state.
func reported(d *Dispatcher, s *sink, nid string) (delivered, pending int) {
	for _, m := range s.all() {
		if m.Digest != nil {
			delivered += m.Digest.Events
		} else {
			delivered += 1 + m.Suppressed
		}
	}
	d.amu.Lock()
	defer d.amu.Unlock()
	if b := d.al.digests[nid]; b != nil {
		pending += b.Events
	}
	for k, st := range d.al.alerts {
		if strings.HasPrefix(k, nid+"|") {
			pending += st.Pending
		}
	}
	return delivered, pending
}

// checkWindows checks that, per channel and problem, an alert that isn't
// "worse" or "spike" never follows the previous one within the window.
func checkWindows(t *testing.T, nid string, msgs []*Message, window time.Duration) {
	t.Helper()
	last := map[string]time.Time{}
	sorted := slices.Clone(msgs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].at.Before(sorted[j].at) })
	for _, m := range sorted {
		if m.Digest != nil {
			continue
		}
		if prev, ok := last[m.fp]; ok && m.Reason.Code != ReasonWorse && m.Reason.Code != ReasonSpike && m.at.Sub(prev) < window {
			t.Errorf("%s: %s alerted again after %v (%s)", nid, m.fp, m.at.Sub(prev), m.Reason.Code)
		}
		last[m.fp] = m.at
	}
}

// 18. Every event a channel matched is reported to it exactly once, with
// handlers, flushes, reconfiguration, saves, a restart and failed
// deliveries all running at once.
func TestCountConservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting.json")
	cfg := &config.Config{
		Notifiers: []config.Notifier{chat("slack"), chat("email"), pager("pd")},
		Sources:   []config.Source{{ID: "src-0", AlertGroupIDs: []string{"g"}}, {ID: "src-1", AlertGroupIDs: []string{"g"}}},
		Routes: []config.Route{
			smartRoute("smart", model.SeverityWarning, 30, 1800, "slack", "email", "pd"),
			immediateRoute("legacy", model.SeverityError, 1800, "slack", "pd"),
		},
		AlertGroups: []config.AlertGroup{{ID: "g", Name: "team", Enabled: true, MinSeverity: model.SeverityError, NotifierIDs: []string{"email", "slack"},
			ThrottleSeconds: 1800, DeliveryPolicy: config.DeliveryPolicy{Delivery: config.DeliverySmart, ImmediateSeverity: model.SeverityCritical, DigestMinutes: 60}}},
	}
	e := newEnv(t, Options{StatePath: path, Warmup: 10 * time.Minute}, cfg)
	// Some sends fail, retryably; a digest's retry never does, so nothing
	// is lost.
	for _, s := range e.sinks {
		s.fail = func(m *Message) error {
			h := fnv.New32a()
			h.Write([]byte(m.ID))
			if h.Sum32()%5 == 0 && (m.Digest == nil || !m.Digest.Retried) {
				return &HTTPError{Status: 503}
			}
			return nil
		}
	}
	severities := []model.Severity{model.SeverityWarning, model.SeverityWarning, model.SeverityError, model.SeverityError, model.SeverityError, model.SeverityCritical}
	var mu sync.Mutex
	matched := map[string]int{}
	perEvent := map[string]int{}

	phase := func(d *Dispatcher, half int) {
		ctx, cancel := context.WithCancel(context.Background())
		d.drainOnce.Do(func() { close(d.draining) }) // no retry waits
		runDone := make(chan struct{})
		go func() { d.Run(ctx, 2); close(runDone) }()
		stop := make(chan struct{})
		var bg sync.WaitGroup
		for _, f := range []func(){
			func() { d.flushDue(e.clock.Now()) },
			func() { d.Configure(cfg) },
			func() { _ = d.SaveState() },
		} {
			bg.Add(1)
			go func() {
				defer bg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						f()
						time.Sleep(3 * time.Millisecond)
					}
				}
			}()
		}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(1000*half + g)))
				for i := 0; i < 1000; i++ {
					fp := rng.Intn(300)
					ev := model.Event{ID: fmt.Sprintf("h%d-g%d-%d", half, g, i), SourceID: fmt.Sprint("src-", fp%4), SourceName: fmt.Sprint("src-", fp%4),
						Category: fmt.Sprint("cat-", fp%3), Fingerprint: fmt.Sprintf("fp-%03d", fp), Severity: severities[rng.Intn(len(severities))], Message: "m"}
					e.clock.Add(time.Second)
					d.Handle(ev)
					decs := d.Decisions(ev.ID)
					mu.Lock()
					for _, dec := range decs {
						perEvent[dec.NotifierID+" "+ev.ID]++
						if dec.Action != ActionSkipped {
							matched[dec.NotifierID]++
						}
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		close(stop)
		bg.Wait()
		dctx, dcancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := d.Drain(dctx); err != nil {
			t.Fatal(err)
		}
		dcancel()
		cancel()
		<-runDone
	}
	phase(e.d, 0)
	lost := e.d.LostEvents.Load()
	d2 := NewDispatcherWith(quiet, Options{StatePath: path, Now: e.clock.Now, Warmup: 10 * time.Minute})
	d2.Configure(cfg)
	phase(d2, 1)
	lost += d2.LostEvents.Load()
	if lost != 0 {
		t.Errorf("lost %d events", lost)
	}
	for _, nid := range []string{"slack", "email", "pd"} {
		delivered, pending := reported(d2, e.sinks[nid], nid)
		if matched[nid] == 0 || matched[nid] != delivered+pending {
			t.Errorf("%s: matched %d, reported %d + pending %d", nid, matched[nid], delivered, pending)
		}
		checkWindows(t, nid, e.sinks[nid].all(), 1800*time.Second)
		seen := map[string]int{}
		for _, m := range e.sinks[nid].alerts() {
			if seen[m.Event.ID]++; seen[m.Event.ID] > 1 {
				t.Errorf("%s: event %s sent twice", nid, m.Event.ID)
			}
		}
	}
	for k, n := range perEvent {
		if n > 1 {
			t.Errorf("%s: %d decisions", k, n)
		}
	}
	if e.d.Decided[indexOf(Actions, ActionHeld)].Load()+d2.Decided[indexOf(Actions, ActionHeld)].Load() == 0 {
		t.Error("nothing was held: flood protection wasn't exercised")
	}
}

// incidentEvent is one line of a replayed incident.
type incidentEvent struct {
	at time.Duration
	ev model.Event
}

// incident reproduces the baseline incident's shapes: an outage logging
// one error 200 times a minute, another logging 100 errors that differ by a
// request id, nginx 5xx, criticals on six sources, SSH noise, a late
// security error and background warnings.
func incident(e *env) []incidentEvent {
	var out []incidentEvent
	add := func(at time.Duration, src, cat, fp string, sev model.Severity) {
		out = append(out, incidentEvent{at, e.ev(src, cat, fp, sev)})
	}
	for i := 0; i < 200; i++ {
		add(time.Duration(i)*300*time.Millisecond, "checkout-api", "database", "db-json", model.SeverityError)
	}
	for i := 0; i < 100; i++ {
		add(5*time.Second+time.Duration(i)*200*time.Millisecond, "orders-java", "database", fmt.Sprintf("rid-%03d", i), model.SeverityError)
	}
	for k := 0; k < 4; k++ {
		for j := 0; j < 10; j++ {
			add(30*time.Second+time.Duration(k*10+j)*3*time.Second, "nginx", "network", fmt.Sprint("nginx-5xx-", k), model.SeverityError)
		}
	}
	critSources := []string{"payments", "kube", "db-primary", "worker", "cache", "gateway"}
	for i := 0; i < 10; i++ {
		add(10*time.Second+time.Duration(i)*20*time.Second, critSources[i%6], "crash", fmt.Sprint("crit-", i), model.SeverityCritical)
	}
	for j := 1; j < 12; j++ {
		add(10*time.Second+time.Duration(j)*15*time.Second, critSources[0], "crash", "crit-0", model.SeverityCritical)
	}
	add(60*time.Second, "billing", "application", "billing-err", model.SeverityError)
	add(90*time.Second, "auth", "security", "auth-err", model.SeverityError)
	for k := 0; k < 6; k++ {
		for j := 0; j < 7; j++ {
			add(60*time.Second+time.Duration(k*7+j)*5*time.Second, "bastion", "security", fmt.Sprint("ssh-", k), model.SeverityError)
		}
	}
	add(464*time.Second, "nginx", "security", "waf", model.SeverityError)
	for i := 0; i < 30; i++ {
		add(time.Duration(i)*50*time.Second, critSources[i%6], "app", fmt.Sprint("warn-", i%5), model.SeverityWarning)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out
}

// replay handles the incident from start, with a flush tick every 10 s,
// delivering everything at once, until start+until. It returns each
// event's decisions and decision time.
func (e *env) replay(start time.Time, events []incidentEvent, until time.Duration) (map[string][]Decision, map[string]time.Time) {
	decs, when := map[string][]Decision{}, map[string]time.Time{}
	tick := start
	for _, ie := range events {
		at := start.Add(ie.at)
		for ; !tick.After(at); tick = tick.Add(flushTick) {
			e.flush(tick)
		}
		decs[ie.ev.ID] = e.at(at, ie.ev)
		when[ie.ev.ID] = at
		e.pump()
	}
	for ; !tick.After(start.Add(until)); tick = tick.Add(flushTick) {
		e.flush(tick)
	}
	return decs, when
}

func wizardRoutes() *config.Config {
	return &config.Config{
		Notifiers: []config.Notifier{chat("slack"), chat("email"), pager("pd")},
		Routes: []config.Route{
			immediateRoute("pd: critical and above", model.SeverityCritical, 900, "pd"),
			smartRoute("slack: error and above", model.SeverityError, 30, 3600, "slack"),
			smartRoute("email: warning and above", model.SeverityWarning, 60, 3600, "email"),
		},
	}
}

// 19. The baseline incident, replayed on a fresh install, on a warm
// system, and again 2 h later.
func TestIncidentReplay(t *testing.T) {
	t.Run("fresh install", func(t *testing.T) {
		e := newEnv(t, Options{}, wizardRoutes())
		events := incident(e)
		decs, _ := e.replay(t0, events, 35*time.Minute)
		alerts, digests := e.sinks["slack"].alerts(), e.sinks["slack"].digests()
		var crit, flood int
		var floodAt time.Time
		for _, m := range alerts {
			switch {
			case m.Event.Severity == model.SeverityCritical:
				crit++
			case m.Reason.Code == ReasonSpike && strings.HasPrefix(m.Reason.Text, "Flood: 50 in the last 5 minutes"):
				flood++
				floodAt = m.at
			}
		}
		if crit != 10 || flood != 1 || len(alerts) != 11 || len(digests) != 1 || !digests[0].at.Equal(t0.Add(30*time.Minute)) {
			t.Fatalf("slack: %d critical, %d flood, %d alerts, %d digests", crit, flood, len(alerts), len(digests))
		}
		if want := t0.Add(49 * 300 * time.Millisecond); !floodAt.Equal(want) {
			t.Errorf("flood at %v, want the 50th DB event at %v", floodAt, want)
		}
		for _, m := range alerts {
			if m.at.Before(floodAt) && m.Event.Severity != model.SeverityCritical {
				t.Errorf("a lesser alert before the flood: %s", m.Title())
			}
		}
		if n := len(e.sinks["pd"].alerts()); n != 10 {
			t.Errorf("pages = %d", n)
		}
		conserve(t, e, events, decs)
	})
	t.Run("warm, then 2 h later", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, wizardRoutes())
		events := incident(e)
		decs, when := e.replay(t0, events, 20*time.Minute)
		for _, ie := range events {
			for _, d := range decs[ie.ev.ID] {
				if ie.ev.Severity == model.SeverityCritical && d.Action != ActionSent && d.Action != ActionThrottled {
					t.Errorf("%s on %s: %s", ie.ev.Fingerprint, d.NotifierID, d.Action)
				}
				if d.Action == ActionHeld {
					held := when[ie.ev.ID]
					if d.Due.Sub(held) > 10*time.Minute {
						t.Errorf("%s held on %s until %v", ie.ev.Fingerprint, d.NotifierID, d.Due)
					}
					if !slices.ContainsFunc(e.sinks[d.NotifierID].digests(), func(m *Message) bool {
						return !m.at.Before(held) && !m.at.After(d.Due.Add(flushTick))
					}) {
						t.Errorf("%s held on %s at %v: no summary by %v", ie.ev.Fingerprint, d.NotifierID, held, d.Due)
					}
				}
			}
		}
		for _, nid := range []string{"slack", "email"} {
			var lesser []time.Time
			for _, m := range e.sinks[nid].alerts() {
				if m.Event.Severity != model.SeverityCritical {
					lesser = append(lesser, m.at)
				}
			}
			for i := range lesser {
				n := 0
				for _, at := range lesser[i:] {
					if at.Sub(lesser[i]) < 10*time.Minute {
						n++
					}
				}
				if n > channelLimit {
					t.Errorf("%s: %d lesser alerts within 10 min of %v", nid, n, lesser[i])
				}
			}
			rid := 0
			for _, ie := range events {
				if strings.HasPrefix(ie.ev.Fingerprint, "rid-") && decisionFor(t, decs[ie.ev.ID], nid).Action == ActionSent {
					rid++
				}
			}
			if rid != 1 {
				t.Errorf("%s: %d right-away alerts for the request-id outage", nid, rid)
			}
		}
		conserve(t, e, events, decs)

		later := incident(e)
		decs2, _ := e.replay(t0.Add(2*time.Hour), later, 20*time.Minute)
		var db []Decision
		for _, ie := range later {
			if ie.ev.Fingerprint != "db-json" {
				continue
			}
			if d := decisionFor(t, decs2[ie.ev.ID], "slack"); d.Action == ActionSent {
				db = append(db, d)
			}
		}
		if len(db) != 1 || db[0].Reason.Code != ReasonSpike || !strings.HasPrefix(db[0].Reason.Text, "Spiking: ") {
			t.Errorf("the DB outage 2 h later: %+v", db)
		}
	})
}

// conserve checks exact count conservation per channel for a replay.
func conserve(t *testing.T, e *env, events []incidentEvent, decs map[string][]Decision) {
	t.Helper()
	matched := map[string]int{}
	for _, ie := range events {
		for _, d := range decs[ie.ev.ID] {
			if d.Action != ActionSkipped {
				matched[d.NotifierID]++
			}
		}
	}
	for _, nid := range []string{"slack", "email", "pd"} {
		delivered, pending := reported(e.d, e.sinks[nid], nid)
		if matched[nid] != delivered+pending || e.d.LostEvents.Load() != 0 {
			t.Errorf("%s: matched %d, reported %d + pending %d", nid, matched[nid], delivered, pending)
		}
	}
}

// 20. Every structure is bounded.
func TestMemoryBounds(t *testing.T) {
	e := newEnv(t, Options{}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	for i := 0; i < 30000; i++ {
		e.d.Handle(model.Event{ID: fmt.Sprint("w", i), SourceID: "s", Fingerprint: fmt.Sprint("w", i), Severity: model.SeverityWarning})
	}
	if n := e.d.Gauges().PatternsTracked; n > maxPatterns || n < maxPatterns*8/10 {
		t.Errorf("patterns = %d", n)
	}
	e.d.Handle(model.Event{ID: "info", SourceID: "s", Fingerprint: "info", Severity: model.SeverityInfo})
	if e.d.Decisions("info") != nil {
		t.Error("unrouted info was decided on")
	}

	now := t0.Add(30 * 24 * time.Hour)
	e.d.amu.Lock()
	a := &e.d.al
	a.alerts = map[string]*alertState{
		"slack|old":           {LastSent: now.Add(-25 * time.Hour)},
		"slack|old-pending":   {LastSent: now.Add(-8 * 24 * time.Hour), Pending: 3},
		"slack|fresh-pending": {LastSent: now.Add(-2 * 24 * time.Hour), Pending: 2},
		"slack|recent":        {LastSent: now.Add(-time.Hour)},
	}
	a.recent["slack"] = []time.Time{now.Add(-11 * time.Minute), now.Add(-time.Minute)}
	a.promoted["slack|s|x"] = now.Add(-11 * time.Minute)
	e.d.amu.Unlock()
	e.d.prune(now)
	e.d.amu.Lock()
	if keys := slices.Sorted(mapsKeys(a.alerts)); strings.Join(keys, ",") != "slack|fresh-pending,slack|recent" ||
		len(a.recent["slack"]) != 1 || len(a.promoted) != 0 || len(a.patterns) != 0 || len(a.sources) != 0 {
		t.Errorf("after prune: %v, recent %d, promoted %d, patterns %d", keys, len(a.recent["slack"]), len(a.promoted), len(a.patterns))
	}
	for i := 0; i <= maxAlertStates; i++ {
		a.alerts[fmt.Sprint("slack|e", i)] = &alertState{LastSent: now.Add(-time.Duration(i) * time.Second)}
	}
	a.evictAlerts(e.d)
	if len(a.alerts) > maxAlertStates || a.alerts["slack|fresh-pending"] == nil || a.alerts["slack|e0"] == nil || a.alerts[fmt.Sprint("slack|e", maxAlertStates)] != nil {
		t.Errorf("after eviction: %d states", len(a.alerts))
	}
	e.d.amu.Unlock()
	if lost := e.d.LostEvents.Load(); lost != 3 {
		t.Errorf("lost = %d", lost)
	}

	e = newEnv(t, Options{}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	for i := 0; i <= maxDecisionEvents; i++ {
		e.d.Handle(model.Event{ID: fmt.Sprint("e", i), SourceID: "s", Fingerprint: "fp", Severity: model.SeverityError})
	}
	if e.d.Decisions("e0") != nil || e.d.Decisions("e1") == nil || len(e.d.al.decisions.m) != maxDecisionEvents {
		t.Errorf("decision ring: %d", len(e.d.al.decisions.m))
	}
	var ids []string
	for i := 0; i <= digestLogSize; i++ {
		e.d.Handle(model.Event{ID: fmt.Sprint("d", i), SourceID: "s", Fingerprint: "fp", Severity: model.SeverityError})
		id, ok := e.d.FlushDigest("slack")
		if !ok {
			t.Fatal("nothing to flush")
		}
		ids = append(ids, id)
		e.pump()
	}
	if _, dg, ok := e.d.DeliveryDetail(ids[0]); !ok || dg != nil || len(e.d.al.digestLog.m) != digestLogSize {
		t.Errorf("digest log: %d kept, oldest %v", len(e.d.al.digestLog.m), dg)
	}
	if _, dg, _ := e.d.DeliveryDetail(ids[digestLogSize]); dg == nil {
		t.Error("the latest digest isn't kept")
	}
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
