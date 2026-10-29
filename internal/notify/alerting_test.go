package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// fakeClock is a settable clock for Options.Now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// t0 is when every test's clock starts: a Monday, 09:00 UTC.
var t0 = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

func newFakeClock() *fakeClock { return &fakeClock{t: t0} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *fakeClock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// sink captures what one test's notifier is sent. The "sink-test" type
// takes digests; "sink-pager-test" is no-digest, like PagerDuty. Both
// deliver to the sink their "sink" setting names.
type sink struct {
	mu   sync.Mutex
	msgs []*Message
	fail func(*Message) error // nil: every send succeeds
}

var sinks sync.Map // name → *sink

func init() {
	newSink := func(s map[string]string) (Sender, error) {
		v, ok := sinks.Load(s["sink"])
		if !ok {
			return nil, fmt.Errorf("no sink %q", s["sink"])
		}
		return v.(*sink), nil
	}
	fields := []Field{{Key: "sink", Label: "Sink", Type: "text"}}
	Register(&Plugin{Type: "sink-test", Label: "Sink", Audience: "tests", Fields: fields, New: newSink})
	Register(&Plugin{Type: "sink-pager-test", Label: "Pager (test)", Audience: "tests", Fields: fields, NoDigest: true, New: newSink})
}

func (s *sink) Send(ctx context.Context, m *Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		if err := s.fail(m); err != nil {
			return err
		}
	}
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *sink) all() []*Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

func (s *sink) alerts() []*Message {
	return slices.DeleteFunc(s.all(), func(m *Message) bool { return m.Digest != nil })
}

func (s *sink) digests() []*Message {
	return slices.DeleteFunc(s.all(), func(m *Message) bool { return m.Digest == nil })
}

// env is a dispatcher on a fake clock whose notifiers are sinks. Most tests
// pump the lanes themselves instead of calling Run.
type env struct {
	t     *testing.T
	d     *Dispatcher
	clock *fakeClock
	cfg   *config.Config
	sinks map[string]*sink // by notifier ID
	n     int
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// newEnv builds the dispatcher for cfg. Notifiers without a type are
// "sink-test"; every notifier is enabled and gets its own sink.
func newEnv(t *testing.T, o Options, cfg *config.Config) *env {
	t.Helper()
	e := &env{t: t, clock: newFakeClock(), cfg: cfg, sinks: map[string]*sink{}}
	if o.Now == nil {
		o.Now = e.clock.Now
	}
	for i := range cfg.Notifiers {
		n := &cfg.Notifiers[i]
		if n.Type == "" {
			n.Type = "sink-test"
		}
		if n.Name == "" {
			n.Name = n.ID
		}
		n.Enabled = true
		name := t.Name() + "/" + n.ID
		n.Settings = map[string]string{"sink": name}
		s := &sink{}
		sinks.Store(name, s)
		t.Cleanup(func() { sinks.Delete(name) })
		e.sinks[n.ID] = s
	}
	if cfg.Settings.PublicURL == "" {
		cfg.Settings.PublicURL = "https://lissandra.test"
	}
	e.d = NewDispatcherWith(quiet, o)
	e.d.Configure(cfg)
	return e
}

// chat is a digest-capable notifier; pager a no-digest one.
func chat(id string) config.Notifier { return config.Notifier{ID: id, Name: id} }
func pager(id string) config.Notifier {
	return config.Notifier{ID: id, Name: id, Type: "sink-pager-test"}
}

func smartRoute(name string, min model.Severity, digest, throttle int, notifiers ...string) config.Route {
	return config.Route{ID: name, Name: name, Enabled: true, MinSeverity: min, NotifierIDs: notifiers, ThrottleSeconds: throttle,
		DeliveryPolicy: config.DeliveryPolicy{Delivery: config.DeliverySmart, ImmediateSeverity: model.SeverityCritical, DigestMinutes: digest}}
}

func immediateRoute(name string, min model.Severity, throttle int, notifiers ...string) config.Route {
	return config.Route{ID: name, Name: name, Enabled: true, MinSeverity: min, NotifierIDs: notifiers, ThrottleSeconds: throttle}
}

// ev is an event of fingerprint fp from a source (by name, which is also
// its ID) in a category; the message names the fingerprint.
func (e *env) ev(src, cat, fp string, sev model.Severity) model.Event {
	e.n++
	return model.Event{ID: fmt.Sprintf("%s-%d", fp, e.n), SourceID: src, SourceName: src, Category: cat, Fingerprint: fp,
		Severity: sev, Message: "boom in " + fp}
}

// handle hands ev to the dispatcher and returns its decisions.
func (e *env) handle(ev model.Event) []Decision {
	e.d.Handle(ev)
	return e.d.Decisions(ev.ID)
}

// at handles ev at time t.
func (e *env) at(t time.Time, ev model.Event) []Decision {
	e.clock.Set(t)
	return e.handle(ev)
}

// next takes one queued job off any lane.
func (e *env) next() (job, bool) {
	d := e.d
	d.lmu.Lock()
	defer d.lmu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(d.lanes)) {
		select {
		case j := <-d.lanes[id].queue:
			return j, true
		default:
		}
	}
	return job{}, false
}

// pump delivers everything queued, as the lane workers would.
func (e *env) pump() int {
	n := 0
	for j, ok := e.next(); ok; j, ok = e.next() {
		e.d.work(context.Background(), j)
		n++
	}
	return n
}

// pumpErr completes everything queued with err, without sending.
func (e *env) pumpErr(err error) []*Message {
	var out []*Message
	for j, ok := e.next(); ok; j, ok = e.next() {
		e.d.completed(j.ch, j.msg, err)
		e.d.pending.Add(-1)
		out = append(out, j.msg)
	}
	return out
}

// flush runs a flush tick at t and delivers what it queued.
func (e *env) flush(t time.Time) {
	e.clock.Set(t)
	e.d.flushDue(t)
	e.pump()
}

func (e *env) buffer(nid string) *digestBuffer {
	e.d.amu.Lock()
	defer e.d.amu.Unlock()
	if b := e.d.al.digests[nid]; b != nil {
		return b.clone()
	}
	return nil
}

func (e *env) state(nid, fp string) *alertState {
	e.d.amu.Lock()
	defer e.d.amu.Unlock()
	if st := e.d.al.alerts[nid+"|"+fp]; st != nil {
		cp := *st
		return &cp
	}
	return nil
}

func decisionFor(t *testing.T, decs []Decision, nid string) Decision {
	t.Helper()
	for _, d := range decs {
		if d.NotifierID == nid {
			return d
		}
	}
	t.Fatalf("no decision for %s in %+v", nid, decs)
	return Decision{}
}

func wantDecision(t *testing.T, decs []Decision, nid, action, code string) Decision {
	t.Helper()
	d := decisionFor(t, decs, nid)
	if d.Action != action || d.Reason.Code != code {
		t.Fatalf("%s: %s/%s (%q), want %s/%s", nid, d.Action, d.Reason.Code, d.Reason.Text, action, code)
	}
	return d
}

func (e *env) rightAway(code string) uint64 {
	return e.d.RightAway[indexOf(RightAwayReasons, code)].Load()
}
func (e *env) decided(action string) uint64 { return e.d.Decided[indexOf(Actions, action)].Load() }

// 1. A route saved before delivery modes existed sends as it always did.
func TestImmediateZeroValueKeepsBehaviour(t *testing.T) {
	e := newEnv(t, Options{}, &config.Config{
		Notifiers: []config.Notifier{chat("n1")},
		Routes:    []config.Route{immediateRoute("dev-errors", model.SeverityError, 3600, "n1")},
	})
	a := e.ev("api", "database", "a", model.SeverityError)
	dec := wantDecision(t, e.handle(a), "n1", ActionSent, ReasonEvery)
	if dec.Reason.Text != `Matches "dev-errors", which sends every matching event right away. Repeats are held back for 1 h.` {
		t.Errorf("every: %q", dec.Reason.Text)
	}
	wantDecision(t, e.handle(e.ev("api", "database", "b", model.SeverityError)), "n1", ActionSent, ReasonEvery)
	e.clock.Add(time.Minute)
	dec = wantDecision(t, e.handle(e.ev("api", "database", "a", model.SeverityError)), "n1", ActionThrottled, ReasonThrottled)
	if dec.Reason.Text != "Repeat of the alert at 09:00 UTC: counted, and reported with the next alert after 10:00 UTC." {
		t.Errorf("throttled: %q", dec.Reason.Text)
	}
	e.pump()
	e.clock.Add(2 * time.Hour)
	wantDecision(t, e.handle(e.ev("api", "database", "a", model.SeverityError)), "n1", ActionSent, ReasonRepeat)
	e.pump()
	msgs := e.sinks["n1"].alerts()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d", len(msgs))
	}
	last := msgs[2]
	if last.Suppressed != 1 || last.Reason.Text != "Still happening: 1 more since the alert at 09:00 UTC." || !last.Since.Equal(t0) {
		t.Errorf("after the window: %d %q %v", last.Suppressed, last.Reason.Text, last.Since)
	}
	if e.buffer("n1") != nil || e.decided(ActionDigest) != 0 || len(e.sinks["n1"].digests()) != 0 {
		t.Error("an immediate route batched something")
	}
}

// 2. A smart route batches lesser events into one digest per channel.
func TestSmartBatchesLesserEvents(t *testing.T) {
	e := newEnv(t, Options{}, &config.Config{ // sources warm up: nothing is new yet
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("dev-slack", model.SeverityWarning, 30, 3600, "slack")},
	})
	a1 := e.ev("checkout-api", "database", "a", model.SeverityError)
	a1.Message = "first A"
	a2 := e.ev("checkout-api", "database", "a", model.SeverityError)
	a2.Message = "second A"
	a3 := e.ev("checkout-api", "database", "a", model.SeverityWarning)
	a3.Message = "a warning A"
	for i, ev := range []model.Event{a1, e.ev("orders", "network", "b", model.SeverityError), a2, e.ev("orders", "network", "b", model.SeverityError), a3} {
		dec := wantDecision(t, e.at(t0.Add(time.Duration(i)*time.Minute), ev), "slack", ActionDigest, ReasonDigest)
		if !dec.Due.Equal(t0.Add(30 * time.Minute)) {
			t.Errorf("due %v", dec.Due)
		}
	}
	e.flush(t0.Add(29 * time.Minute))
	if n := len(e.sinks["slack"].all()); n != 0 {
		t.Fatalf("%d messages before the digest is due", n)
	}
	e.flush(t0.Add(30 * time.Minute))
	ds := e.sinks["slack"].digests()
	if len(ds) != 1 {
		t.Fatalf("digests = %d", len(ds))
	}
	m := ds[0]
	dg := m.Digest
	if dg.Events != 5 || dg.Patterns != 2 || dg.Highest != model.SeverityError || dg.Reason != ReasonDigest || len(dg.Entries) != 2 ||
		!dg.From.Equal(t0) || !dg.To.Equal(t0.Add(30*time.Minute)) || dg.Link != "https://lissandra.test/#/deliveries/"+dg.ID || m.ID != dg.ID {
		t.Fatalf("digest = %+v", dg)
	}
	ea, eb := dg.Entries[0], dg.Entries[1]
	if ea.Fingerprint != "a" || ea.Count != 3 || !ea.First.Equal(t0) || !ea.Last.Equal(t0.Add(4*time.Minute)) || ea.Severity != model.SeverityError ||
		ea.Sample != "second A" || ea.EventID != a2.ID || ea.Link != "https://lissandra.test/#/events?fingerprint=a" || ea.SourceName != "checkout-api" {
		t.Errorf("entry a = %+v", ea)
	}
	if eb.Fingerprint != "b" || eb.Count != 2 {
		t.Errorf("entry b = %+v", eb)
	}
	if m.Reason.Text != `Batched every 30 min on "dev-slack". Right away: critical and up, and errors that are new, spiking or getting worse. Everything else from warning up: one digest every 30 min.` {
		t.Errorf("why: %q", m.Reason.Text)
	}
	if e.buffer("slack") != nil {
		t.Error("the buffer outlived its digest")
	}
	wantDecision(t, e.handle(e.ev("checkout-api", "database", "c", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
	e.pump()
	if len(e.sinks["slack"].alerts()) != 1 {
		t.Error("a critical waited for the digest")
	}
}

// 4. Every repeat is reported once: in the digest, or folded into the
// next alert, or with the next alert on a channel without digests.
func TestRepeatsReportedExactlyOnce(t *testing.T) {
	t.Run("in the digest", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
		})
		wantDecision(t, e.at(t0, e.ev("api", "app", "c", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
		for i := 1; i <= 49; i++ {
			wantDecision(t, e.at(t0.Add(time.Duration(i)*time.Second), e.ev("api", "app", "c", model.SeverityCritical)), "slack", ActionThrottled, ReasonThrottled)
		}
		e.pump()
		e.flush(t0.Add(31 * time.Minute))
		ds := e.sinks["slack"].digests()
		if len(ds) != 1 || len(ds[0].Digest.Entries) != 1 {
			t.Fatalf("digests: %d", len(ds))
		}
		if en := ds[0].Digest.Entries[0]; en.Repeats != 49 || en.Count != 49 || !en.AlertedAt.Equal(t0) {
			t.Errorf("entry = %+v", en)
		}
		wantDecision(t, e.at(t0.Add(61*time.Minute), e.ev("api", "app", "c", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
		e.pump()
		if as := e.sinks["slack"].alerts(); len(as) != 2 || as[1].Suppressed != 0 {
			t.Errorf("the next alert reported repeats the digest had: %+v", as[len(as)-1])
		}
	})
	t.Run("folded into the next alert", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 60, 600, "slack")},
		})
		e.at(t0, e.ev("api", "app", "c", model.SeverityCritical))
		for i := 1; i <= 49; i++ {
			e.at(t0.Add(time.Duration(i)*time.Second), e.ev("api", "app", "c", model.SeverityCritical))
		}
		if b := e.buffer("slack"); b == nil || b.Events != 49 {
			t.Fatalf("buffer = %+v", b)
		}
		wantDecision(t, e.at(t0.Add(20*time.Minute), e.ev("api", "app", "c", model.SeverityCritical)), "slack", ActionSent, ReasonRepeat)
		e.pump()
		if as := e.sinks["slack"].alerts(); len(as) != 2 || as[1].Suppressed != 49 {
			t.Fatalf("alerts = %d, suppressed %d", len(as), as[len(as)-1].Suppressed)
		}
		if e.buffer("slack") != nil {
			t.Error("folded repeats stayed in the buffer")
		}
		e.flush(t0.Add(61 * time.Minute))
		if len(e.sinks["slack"].digests()) != 0 {
			t.Error("a digest repeated what the alert reported")
		}
	})
	t.Run("no-digest channel", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{pager("pd")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 900, "pd")},
		})
		e.at(t0, e.ev("api", "app", "c", model.SeverityCritical))
		for i := 1; i <= 5; i++ {
			wantDecision(t, e.at(t0.Add(time.Duration(i)*time.Minute), e.ev("api", "app", "c", model.SeverityCritical)), "pd", ActionThrottled, ReasonThrottled)
		}
		if st := e.state("pd", "c"); st.Pending != 5 {
			t.Errorf("pending = %d", st.Pending)
		}
		wantDecision(t, e.at(t0.Add(16*time.Minute), e.ev("api", "app", "c", model.SeverityCritical)), "pd", ActionSent, ReasonRepeat)
		e.pump()
		if as := e.sinks["pd"].alerts(); len(as) != 2 || as[1].Suppressed != 5 || e.state("pd", "c").Pending != 0 {
			t.Errorf("alerts = %+v", as)
		}
	})
}

// 5. A rise in severity goes through the throttle, in every mode.
func TestSeverityRiseBypassesThrottle(t *testing.T) {
	for _, mode := range []string{config.DeliveryImmediate, config.DeliverySmart} {
		t.Run(mode, func(t *testing.T) {
			r := smartRoute("r", model.SeverityError, 30, 3600, "n1")
			first := ReasonNew
			if mode == config.DeliveryImmediate {
				r, first = immediateRoute("r", model.SeverityError, 3600, "n1"), ReasonEvery
			}
			e := newEnv(t, Options{Warmup: -1}, &config.Config{Notifiers: []config.Notifier{chat("n1")}, Routes: []config.Route{r}})
			wantDecision(t, e.at(t0, e.ev("api", "app", "a", model.SeverityError)), "n1", ActionSent, first)
			dec := wantDecision(t, e.at(t0.Add(time.Minute), e.ev("api", "app", "a", model.SeverityCritical)), "n1", ActionSent, ReasonWorse)
			if dec.Reason.Text != "Got worse: now critical; the alert at 09:00 UTC was error." {
				t.Errorf("worse: %q", dec.Reason.Text)
			}
			wantDecision(t, e.at(t0.Add(2*time.Minute), e.ev("api", "app", "a", model.SeverityCritical)), "n1", ActionThrottled, ReasonThrottled)
		})
	}
	t.Run("warning to error on a digest channel", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("n1")},
			Routes: []config.Route{
				immediateRoute("all", model.SeverityWarning, 3600, "n1"),
				smartRoute("smart", model.SeverityWarning, 30, 3600, "n1"),
			},
		})
		wantDecision(t, e.at(t0, e.ev("api", "app", "w", model.SeverityWarning)), "n1", ActionSent, ReasonEvery)
		wantDecision(t, e.at(t0.Add(time.Minute), e.ev("api", "app", "w", model.SeverityError)), "n1", ActionSent, ReasonWorse)
		dec := wantDecision(t, e.at(t0.Add(2*time.Minute), e.ev("api", "app", "w", model.SeverityWarning)), "n1", ActionThrottled, ReasonThrottled)
		if dec.Reason.Text != "Repeat of the alert at 09:01 UTC: in the digest due 09:32 UTC." {
			t.Errorf("repeat: %q", dec.Reason.Text)
		}
	})
}

// 6. A source's first hour is batched; after it, a new problem goes right
// away, once, and again only after 7 quiet days.
func TestNoveltyAndWarmup(t *testing.T) {
	cfg := func() *config.Config {
		return &config.Config{Notifiers: []config.Notifier{chat("slack")}, Routes: []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")}}
	}
	e := newEnv(t, Options{}, cfg())
	wantDecision(t, e.at(t0, e.ev("api", "app", "e1", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
	wantDecision(t, e.at(t0.Add(30*time.Minute), e.ev("api", "app", "e2", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
	if g := e.d.Gauges(); g.WarmingUp != 1 {
		t.Errorf("warming up = %d", g.WarmingUp)
	}
	st := e.d.AlertingStatus()
	if len(st.WarmingUp) != 1 || st.WarmingUp[0].SourceID != "api" || !st.WarmingUp[0].Until.Equal(t0.Add(time.Hour)) || st.Limits.WarmupMinutes != 60 {
		t.Errorf("status = %+v", st)
	}
	dec := wantDecision(t, e.at(t0.Add(61*time.Minute), e.ev("api", "app", "e3", model.SeverityError)), "slack", ActionSent, ReasonNew)
	if dec.Reason.Text != "New problem: first seen just now; nothing like it in the last 7 days." {
		t.Errorf("new: %q", dec.Reason.Text)
	}
	wantDecision(t, e.at(t0.Add(62*time.Minute), e.ev("api", "app", "e3", model.SeverityError)), "slack", ActionDigest, ReasonDigest) // a repeat is batched
	wantDecision(t, e.at(t0.Add(63*time.Minute), e.ev("api", "app", "e1", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
	back := t0.Add(62*time.Minute + 8*24*time.Hour)
	dec = wantDecision(t, e.at(back, e.ev("api", "app", "e3", model.SeverityError)), "slack", ActionSent, ReasonNew)
	if dec.Reason.Text != "Back after 8 days: last seen Oct 19 10:02 UTC." {
		t.Errorf("back: %q", dec.Reason.Text)
	}
	// A source added to a warm system warms up on its own.
	wantDecision(t, e.at(back.Add(time.Minute), e.ev("billing", "app", "b1", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
	wantDecision(t, e.at(back.Add(2*time.Minute), e.ev("api", "network", "e9", model.SeverityError)), "slack", ActionSent, ReasonNew)

	e = newEnv(t, Options{Warmup: -1}, cfg())
	wantDecision(t, e.at(t0, e.ev("api", "app", "e1", model.SeverityError)), "slack", ActionSent, ReasonNew)
	if e.d.AlertingStatus().Limits.WarmupMinutes != 0 || e.d.Gauges().WarmingUp != 0 {
		t.Error("no warm-up still reports one")
	}
}

// 7. Spikes are judged against the pattern's usual rate; floods need no
// history; one spike per pattern per hour.
func TestSpikeAgainstBaseline(t *testing.T) {
	cfg := func() *config.Config {
		return &config.Config{Notifiers: []config.Notifier{chat("slack")}, Routes: []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")}}
	}
	spikes := func(e *env, decs [][]Decision) (sent []Decision) {
		for _, ds := range decs {
			if d := decisionFor(e.t, ds, "slack"); d.Action == ActionSent && d.Reason.Code == ReasonSpike {
				sent = append(sent, d)
			}
		}
		return sent
	}
	t.Run("3 an hour, then 30 in 4 minutes", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, cfg())
		for k := 0; k < 15; k++ {
			e.at(t0.Add(time.Duration(k)*20*time.Minute), e.ev("api", "app", "p", model.SeverityError))
		}
		var decs [][]Decision
		start := t0.Add(5*time.Hour + 5*time.Minute)
		for i := 0; i < 30; i++ {
			decs = append(decs, e.at(start.Add(time.Duration(i)*8*time.Second), e.ev("api", "app", "p", model.SeverityError)))
		}
		s := spikes(e, decs)
		if len(s) != 1 || s[0].Reason.Text != "Spiking: 20 in the last 5 minutes, usually about 3 an hour." {
			t.Fatalf("spikes = %+v", s)
		}
		decs = nil
		for i := 0; i < 30; i++ {
			decs = append(decs, e.at(start.Add(30*time.Minute+time.Duration(i)*8*time.Second), e.ev("api", "app", "p", model.SeverityError)))
		}
		if s := spikes(e, decs); len(s) != 0 {
			t.Errorf("a second burst within the hour spiked: %+v", s)
		}
	})
	t.Run("steady 600 an hour", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, cfg())
		start := t0.Add(55 * time.Minute)
		for i := 0; i < 1800; i++ {
			e.at(start.Add(time.Duration(i)*6*time.Second), e.ev("api", "app", "p", model.SeverityError))
		}
		if e.rightAway(ReasonNew) != 1 || e.rightAway(ReasonSpike) != 0 || e.decided(ActionSent) != 1 {
			t.Errorf("new %d, spike %d, sent %d", e.rightAway(ReasonNew), e.rightAway(ReasonSpike), e.decided(ActionSent))
		}
	})
	t.Run("the same burst 2 h later", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, cfg())
		burst := func(at time.Time) (decs [][]Decision) {
			for i := 0; i < 200; i++ {
				decs = append(decs, e.at(at.Add(time.Duration(i)*300*time.Millisecond), e.ev("api", "app", "p", model.SeverityError)))
			}
			return decs
		}
		if s := spikes(e, burst(t0)); len(s) != 0 {
			t.Fatalf("the first burst spiked: %+v", s)
		}
		s := spikes(e, burst(t0.Add(2*time.Hour)))
		if len(s) != 1 || !strings.HasPrefix(s[0].Reason.Text, "Spiking: 84 in the last 5 minutes, usually about 100 an hour") {
			t.Errorf("second burst: %+v", s)
		}
	})
	t.Run("flood", func(t *testing.T) {
		e := newEnv(t, Options{}, cfg()) // warming up: the pattern was never alerted
		e.at(t0, e.ev("api", "app", "other", model.SeverityError))
		wantDecision(t, e.at(t0.Add(10*time.Minute), e.ev("api", "app", "p", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
		var decs [][]Decision
		for i := 0; i < 50; i++ {
			decs = append(decs, e.at(t0.Add(30*time.Minute+time.Duration(i)*5*time.Second), e.ev("api", "app", "p", model.SeverityError)))
		}
		d := decisionFor(t, decs[49], "slack")
		if d.Action != ActionSent || d.Reason.Code != ReasonSpike || d.Reason.Text != "Flood: 50 in the last 5 minutes from a problem first seen at 09:10 UTC." {
			t.Errorf("flood: %+v", d)
		}
		if decisionFor(t, decs[48], "slack").Action != ActionDigest {
			t.Error("flooded before 50")
		}
	})
	t.Run("flood after a new alert", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, cfg())
		var decs [][]Decision
		for i := 0; i < 50; i++ {
			decs = append(decs, e.at(t0.Add(time.Duration(i)*5*time.Second), e.ev("api", "app", "p", model.SeverityError)))
		}
		wantDecision(t, decs[0], "slack", ActionSent, ReasonNew)
		wantDecision(t, decs[49], "slack", ActionThrottled, ReasonThrottled)
		if e.rightAway(ReasonSpike) != 0 {
			t.Error("a flood broke the window")
		}
	})
	t.Run("backlog", func(t *testing.T) {
		e := newEnv(t, Options{}, cfg())
		e.at(t0, e.ev("api", "app", "other", model.SeverityError))
		for i := 0; i < 60; i++ {
			now := t0.Add(30*time.Minute + time.Duration(i)*time.Second)
			ev := e.ev("api", "app", "p", model.SeverityError)
			ev.Timestamp = now.Add(-10 * time.Minute)
			wantDecision(t, e.at(now, ev), "slack", ActionDigest, ReasonDigest)
		}
	})
	t.Run("clock backwards", func(t *testing.T) {
		e := newEnv(t, Options{}, cfg())
		e.at(t0.Add(time.Hour), e.ev("api", "app", "other", model.SeverityError))
		for i := 0; i < 30; i++ {
			d := wantDecision(t, e.at(t0.Add(-time.Duration(i)*time.Minute), e.ev("api", "app", "p", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
			if d.Due.Before(t0.Add(time.Hour)) {
				t.Fatalf("due went backwards: %v", d.Due)
			}
		}
	})
}

// 8. "Worse" also compares with the pattern's usual severity.
func TestWorseThanUsual(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityWarning, 30, 3600, "slack")},
	})
	wantDecision(t, e.at(t0, e.ev("api", "app", "w", model.SeverityWarning)), "slack", ActionDigest, ReasonDigest)
	wantDecision(t, e.at(t0.Add(time.Minute), e.ev("api", "app", "w", model.SeverityWarning)), "slack", ActionDigest, ReasonDigest)
	dec := wantDecision(t, e.at(t0.Add(2*time.Minute), e.ev("api", "app", "w", model.SeverityError)), "slack", ActionSent, ReasonWorse)
	if dec.Reason.Text != "Got worse: now error; it was at most warning in the last 7 days." {
		t.Errorf("worse: %q", dec.Reason.Text)
	}
}

// 9. One promotion per (source, category) per channel in 10 minutes; the
// rest arrive in a summary within minutes, never at the digest time.
func TestSourceLimitAndSummary(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	for i := 0; i < 100; i++ {
		decs := e.at(t0.Add(time.Duration(i)*200*time.Millisecond), e.ev("orders-java", "database", fmt.Sprintf("rid-%03d", i), model.SeverityError))
		if i == 0 {
			wantDecision(t, decs, "slack", ActionSent, ReasonNew)
			continue
		}
		dec := wantDecision(t, decs, "slack", ActionHeld, ReasonSourceLimit)
		if i == 1 && dec.Reason.Text != "Held back: orders-java · database already had a right-away alert here in the last 10 min. In the summary due 09:02 UTC." {
			t.Errorf("held: %q", dec.Reason.Text)
		}
	}
	b := e.buffer("slack")
	due := t0.Add(200*time.Millisecond + 2*time.Minute)
	if b == nil || !b.Summary || b.Held != 99 || b.Events != 99 || !b.Due.Equal(due) {
		t.Fatalf("buffer = %+v", b)
	}
	wantDecision(t, e.at(t0.Add(30*time.Second), e.ev("orders-java", "network", "net-1", model.SeverityError)), "slack", ActionSent, ReasonNew)
	e.pump()
	e.flush(due)
	ds := e.sinks["slack"].digests()
	if len(ds) != 1 {
		t.Fatalf("digests = %d", len(ds))
	}
	m := ds[0]
	if m.Digest.Reason != ReasonSummary || m.Digest.Held != 99 || m.Digest.Patterns != 99 || m.Title() != "[SUMMARY] 99 alerts held back · orders-java" ||
		m.Reason.Text != "Held back so this channel isn't flooded: orders-java · database already had a right-away alert here. The first 20 critical alerts in any 10 minutes are never held." {
		t.Errorf("summary: %s / %q / %+v", m.Title(), m.Reason.Text, m.Digest.Reason)
	}
	if en := m.Digest.Entries[0]; en.Held != ReasonNew || en.HeldBy != ReasonSourceLimit {
		t.Errorf("entry = %+v", en)
	}
	// A second wave 3 minutes later waits for 10 minutes after the summary.
	wave := due.Add(3 * time.Minute)
	dec := wantDecision(t, e.at(wave, e.ev("orders-java", "database", "rid-200", model.SeverityError)), "slack", ActionHeld, ReasonSourceLimit)
	if b := e.buffer("slack"); !dec.Due.Equal(due.Add(10*time.Minute)) || !b.Summary || !b.Due.Equal(due.Add(10*time.Minute)) {
		t.Errorf("second wave due %v (buffer %+v)", dec.Due, b)
	}
}

// 10. Criticals don't use the lesser budget; storms of either are bounded
// on chat and email, never on PagerDuty or for immediate routes.
func TestChannelAndCriticalLimits(t *testing.T) {
	t.Run("budgets", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
		})
		for i := 0; i < 15; i++ {
			wantDecision(t, e.at(t0.Add(time.Duration(i)*time.Second), e.ev(fmt.Sprint("crit-src-", i), "app", fmt.Sprint("crit-", i), model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
		}
		for i := 0; i < 10; i++ {
			wantDecision(t, e.at(t0.Add(time.Minute+time.Duration(i)*time.Second), e.ev(fmt.Sprint("src-", i), "app", fmt.Sprint("new-", i), model.SeverityError)), "slack", ActionSent, ReasonNew)
		}
		dec := wantDecision(t, e.at(t0.Add(2*time.Minute), e.ev("src-10", "app", "new-10", model.SeverityError)), "slack", ActionHeld, ReasonChannelLimit)
		if dec.Reason.Text != "Held back: this channel had 10 right-away alerts below critical in the last 10 min. In the summary due 09:04 UTC." {
			t.Errorf("channel limit: %q", dec.Reason.Text)
		}
		// 10 minutes after the first ones, there is room again.
		wantDecision(t, e.at(t0.Add(11*time.Minute+10*time.Second), e.ev("src-11", "app", "new-11", model.SeverityError)), "slack", ActionSent, ReasonNew)
	})
	storm := func(t *testing.T, e *env, nid string) (sent, held int) {
		for i := 0; i < 25; i++ {
			d := decisionFor(t, e.at(t0.Add(time.Duration(i)*5*time.Second), e.ev(fmt.Sprint("src-", i%6), "app", fmt.Sprint("crit-", i), model.SeverityCritical)), nid)
			switch d.Action {
			case ActionSent:
				sent++
			case ActionHeld:
				held++
				if d.Reason.Code != ReasonCriticalLimit {
					t.Errorf("held by %s", d.Reason.Code)
				}
			}
		}
		return sent, held
	}
	t.Run("critical storm on chat", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
		})
		if sent, held := storm(t, e, "slack"); sent != 20 || held != 5 {
			t.Fatalf("sent %d, held %d", sent, held)
		}
		b := e.buffer("slack")
		firstHeld := t0.Add(20 * 5 * time.Second)
		if b.CriticalHeld != 5 || !b.Summary || !b.Due.Equal(firstHeld.Add(time.Minute)) {
			t.Fatalf("buffer = %+v", b)
		}
		e.pump()
		e.flush(b.Due)
		ds := e.sinks["slack"].digests()
		if len(ds) != 1 || ds[0].Digest.CriticalHeld != 5 || ds[0].Digest.Reason != ReasonSummary {
			t.Fatalf("summary: %+v", ds)
		}
		if !strings.Contains(ds[0].Reason.Text, "more than 20 critical alerts in 10 min") {
			t.Errorf("why: %q", ds[0].Reason.Text)
		}
		blocks := (&slack{s: map[string]string{"mention": "<!here>"}}).digestBlocks(ds[0])
		if head := blocks[0]["text"].(map[string]any)["text"].(string); !strings.HasPrefix(head, "<!here> ") {
			t.Errorf("summary of held criticals without the mention: %q", head)
		}
	})
	t.Run("critical storm on PagerDuty", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{pager("pd")},
			Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 900, "pd")},
		})
		if sent, held := storm(t, e, "pd"); sent != 25 || held != 0 {
			t.Errorf("sent %d, held %d", sent, held)
		}
	})
	t.Run("immediate route", func(t *testing.T) {
		e := newEnv(t, Options{Warmup: -1}, &config.Config{
			Notifiers: []config.Notifier{chat("slack")},
			Routes: []config.Route{
				immediateRoute("legacy", model.SeverityError, 3600, "slack"),
				smartRoute("smart", model.SeverityError, 30, 3600, "slack"),
			},
		})
		if sent, held := storm(t, e, "slack"); sent != 25 || held != 0 {
			t.Errorf("sent %d, held %d", sent, held)
		}
		for i := 0; i < 15; i++ {
			wantDecision(t, e.at(t0.Add(3*time.Minute), e.ev("api", "app", fmt.Sprint("new-", i), model.SeverityError)), "slack", ActionSent, ReasonNew)
		}
	})
}

// 11. One message per channel per event, whatever overlaps.
func TestOneMessagePerNotifierAcrossRoutesAndGroups(t *testing.T) {
	cfg := &config.Config{
		Notifiers: []config.Notifier{chat("n1")},
		Sources:   []config.Source{{ID: "pay", Name: "pay", AlertGroupIDs: []string{"g"}}},
		Routes: []config.Route{
			smartRoute("team-errors", model.SeverityWarning, 60, 7200, "n1"),
			smartRoute("team-fast", model.SeverityWarning, 30, 600, "n1"),
		},
		AlertGroups: []config.AlertGroup{{ID: "g", Name: "Payments", Enabled: true, MinSeverity: model.SeverityError, NotifierIDs: []string{"n1"}, ThrottleSeconds: 1800}},
	}
	e := newEnv(t, Options{}, cfg) // warming up: errors aren't new yet
	decs := e.at(t0, e.ev("pay", "app", "c", model.SeverityCritical))
	if len(decs) != 1 {
		t.Fatalf("decisions = %+v", decs)
	}
	wantDecision(t, decs, "n1", ActionSent, ReasonSeverity) // severity outranks the group's "every"
	e.pump()
	m := e.sinks["n1"].alerts()
	if len(m) != 1 || m[0].RouteName != "team-errors, team-fast, Alert group: Payments" || !slices.Equal(m[0].Routes, []string{"team-errors", "team-fast", "Alert group: Payments"}) {
		t.Fatalf("messages = %+v", m)
	}
	// The window is the smallest throttle among the routes wanting it now.
	wantDecision(t, e.at(t0.Add(5*time.Minute), e.ev("pay", "app", "c", model.SeverityCritical)), "n1", ActionThrottled, ReasonThrottled)
	wantDecision(t, e.at(t0.Add(11*time.Minute), e.ev("pay", "app", "c", model.SeverityCritical)), "n1", ActionSent, ReasonRepeat)
	// Right away for the group, a digest for the smart routes: sent only.
	decs = e.at(t0.Add(12*time.Minute), e.ev("pay", "app", "e", model.SeverityError))
	if len(decs) != 1 {
		t.Fatalf("decisions = %+v", decs)
	}
	wantDecision(t, decs, "n1", ActionSent, ReasonEvery)
	if b := e.buffer("n1"); b != nil && b.Entries["e"] != nil {
		t.Error("an event sent right away was batched too")
	}
	// The digest is the smallest interval among the routes batching it.
	dec := wantDecision(t, e.at(t0.Add(13*time.Minute), e.ev("pay", "app", "w", model.SeverityWarning)), "n1", ActionDigest, ReasonDigest)
	if b := e.buffer("n1"); b.Interval != 30 || !dec.Due.Equal(b.Opened.Add(30*time.Minute)) {
		t.Errorf("buffer interval %d, due %v (opened %v)", b.Interval, dec.Due, b.Opened)
	}
	e.pump()
	seen := map[string]int{}
	for _, m := range e.sinks["n1"].alerts() {
		seen[m.Event.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("event %s sent %d times", id, n)
		}
	}
}

// 12. PagerDuty gets only what is right away by severity: no digests, no
// promotions.
func TestPagerDutyNeverDigests(t *testing.T) {
	e := newEnv(t, Options{}, &config.Config{ // warming up
		Notifiers: []config.Notifier{chat("slack"), pager("oncall-pager")},
		Routes: []config.Route{
			smartRoute("dev", model.SeverityError, 30, 3600, "slack", "oncall-pager"),
			{ID: "d", Name: "digest only", Enabled: true, MinSeverity: model.SeverityWarning, NotifierIDs: []string{"oncall-pager"},
				DeliveryPolicy: config.DeliveryPolicy{Delivery: config.DeliveryDigest, DigestMinutes: 60}},
		},
	})
	decs := e.at(t0, e.ev("api", "app", "e", model.SeverityError))
	wantDecision(t, decs, "slack", ActionDigest, ReasonDigest)
	dec := wantDecision(t, decs, "oncall-pager", ActionSkipped, ReasonNoDigest)
	if dec.Reason.Text != "oncall-pager is Pager (test): it gets only what is sent right away (here: critical and up), never digests." {
		t.Errorf("skipped: %q", dec.Reason.Text)
	}
	decs = e.at(t0.Add(time.Minute), e.ev("api", "app", "c", model.SeverityCritical))
	wantDecision(t, decs, "slack", ActionSent, ReasonSeverity)
	wantDecision(t, decs, "oncall-pager", ActionSent, ReasonSeverity)
	decs = e.at(t0.Add(2*time.Hour), e.ev("api", "app", "new", model.SeverityError)) // warm now
	wantDecision(t, decs, "slack", ActionSent, ReasonNew)
	wantDecision(t, decs, "oncall-pager", ActionSkipped, ReasonNoDigest)
	dec = wantDecision(t, e.at(t0.Add(2*time.Hour), e.ev("api", "app", "w", model.SeverityWarning)), "oncall-pager", ActionSkipped, ReasonNoDigest)
	if !strings.Contains(dec.Reason.Text, "(here: this route sends nothing right away)") {
		t.Errorf("digest only: %q", dec.Reason.Text)
	}
	if e.buffer("oncall-pager") != nil {
		t.Error("PagerDuty has a digest buffer")
	}

	var c capture
	srv := c.server(t, 202)
	pd := config.Notifier{ID: "pd", Name: "pd", Type: "pagerduty", Settings: map[string]string{"routing_key": "rk", "api_url": srv.URL}}
	s, err := Build(pd)
	if err != nil {
		t.Fatal(err)
	}
	msg := testDigestMessage(3, 40)
	if err := s.Send(context.Background(), msg); !errors.Is(err, ErrNoDigest) || retryable(err) || len(c.all()) != 0 {
		t.Errorf("send a digest to PagerDuty: %v, %d requests", err, len(c.all()))
	}
	if err := e.d.deliver(context.Background(), &channel{n: pd, sender: s, noDigest: true}, msg); !errors.Is(err, ErrNoDigest) {
		t.Errorf("deliver: %v", err)
	}
	if del := e.d.Deliveries(1)[0]; del.Status != "failed" || del.Attempts != 1 || del.Kind != KindDigest {
		t.Errorf("recorded: %+v", del)
	}
	if err := e.d.TestDigest(context.Background(), pd); !errors.Is(err, ErrNoDigest) {
		t.Errorf("test digest: %v", err)
	}
}

// 13. A full queue holds an alert into the summary on a digest channel
// (no window opens), and drops it on PagerDuty (counted for the next one).
func TestQueueFull(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1, QueueSize: 1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack"), pager("pd")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack", "pd")},
	})
	decs := e.at(t0, e.ev("api", "app", "a", model.SeverityCritical))
	wantDecision(t, decs, "slack", ActionSent, ReasonSeverity)
	wantDecision(t, decs, "pd", ActionSent, ReasonSeverity)
	decs = e.at(t0.Add(time.Second), e.ev("api", "app", "b", model.SeverityCritical))
	dec := wantDecision(t, decs, "slack", ActionHeld, ReasonQueueFull)
	if dec.Reason.Text != "Held back: the channel's queue was full. In the summary due 09:02 UTC." {
		t.Errorf("held: %q", dec.Reason.Text)
	}
	dec = wantDecision(t, decs, "pd", ActionDropped, ReasonQueueFull)
	if dec.Reason.Text != "Not sent: the notification queue was full. Counted, and reported with the next alert." {
		t.Errorf("dropped: %q", dec.Reason.Text)
	}
	if e.state("slack", "b") != nil {
		t.Error("a held alert opened a window")
	}
	if st := e.state("pd", "b"); st == nil || st.Pending != 1 || !st.LastSent.IsZero() {
		t.Errorf("pd state = %+v", st)
	}
	if del := e.d.Deliveries(1)[0]; del.Status != "dropped" || del.NotifierID != "pd" || e.d.Dropped.Load() != 1 {
		t.Errorf("dropped record: %+v", del)
	}
	e.pump()
	decs = e.at(t0.Add(2*time.Second), e.ev("api", "app", "b", model.SeverityCritical))
	wantDecision(t, decs, "slack", ActionSent, ReasonSeverity)
	wantDecision(t, decs, "pd", ActionSent, ReasonRepeat)
	if as := e.sinks["pd"].all(); len(as) != 1 { // a's: b's is still queued
		t.Fatalf("pd = %d", len(as))
	}
	// A digest that can't be queued waits for the next tick.
	due := e.buffer("slack").Due
	e.d.flushDue(due)
	if e.buffer("slack") == nil {
		t.Fatal("a digest was lost to a full queue")
	}
	e.pump()
	e.flush(due.Add(flushTick))
	if e.buffer("slack") != nil || len(e.sinks["slack"].digests()) != 1 {
		t.Error("the digest didn't go out on the next tick")
	}
}

// 14. A retryable failure re-arms the window and returns the alert's
// events to be reported; a permanent one keeps the window.
func TestFailedAlertRearms(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	e.at(t0, e.ev("api", "app", "a", model.SeverityCritical))
	e.pump()
	for i := 1; i <= 3; i++ {
		e.at(t0.Add(time.Duration(i)*time.Second), e.ev("api", "app", "a", model.SeverityCritical))
	}
	wantDecision(t, e.at(t0.Add(61*time.Minute), e.ev("api", "app", "a", model.SeverityCritical)), "slack", ActionSent, ReasonRepeat)
	failed := e.pumpErr(&HTTPError{Status: 503})
	if len(failed) != 1 || failed[0].Suppressed != 3 {
		t.Fatalf("failed = %+v", failed)
	}
	if st := e.state("slack", "a"); !st.LastSent.Equal(t0) || st.LastSeverity != model.SeverityCritical {
		t.Errorf("state not restored: %+v", st)
	}
	b := e.buffer("slack")
	if b == nil || b.Events != 4 || b.Entries["a"].Undelivered != 4 || b.Entries["a"].Repeats != 0 {
		t.Fatalf("buffer = %+v", b)
	}
	m := wantDecision(t, e.at(t0.Add(62*time.Minute), e.ev("api", "app", "a", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
	_ = m
	e.pump()
	if as := e.sinks["slack"].alerts(); len(as) != 2 || as[1].Suppressed != 0 {
		t.Errorf("after the failure: %+v", as)
	}

	// A failure retrying can't fix keeps the window.
	e.at(t0.Add(63*time.Minute), e.ev("api", "app", "b", model.SeverityCritical))
	e.pumpErr(&HTTPError{Status: 400})
	wantDecision(t, e.at(t0.Add(64*time.Minute), e.ev("api", "app", "b", model.SeverityCritical)), "slack", ActionThrottled, ReasonThrottled)

	// A stale hook (a later alert already went out) leaves the window alone.
	wantDecision(t, e.at(t0.Add(65*time.Minute), e.ev("api", "app", "c", model.SeverityError)), "slack", ActionSent, ReasonNew)
	first, _ := e.next()
	wantDecision(t, e.at(t0.Add(66*time.Minute), e.ev("api", "app", "c", model.SeverityCritical)), "slack", ActionSent, ReasonWorse)
	before := e.state("slack", "c")
	e.d.completed(first.ch, first.msg, &HTTPError{Status: 503})
	e.d.pending.Add(-1)
	if after := e.state("slack", "c"); after.LastSent != before.LastSent || after.LastSeverity != before.LastSeverity || after.Seq != before.Seq {
		t.Errorf("a stale hook changed the window: %+v → %+v", before, after)
	}
	if b := e.buffer("slack"); b.Entries["c"] == nil || b.Entries["c"].Undelivered != 1 {
		t.Error("the stale alert's event was not returned to be reported")
	}
}

func TestFailedDigestMergedBackOnce(t *testing.T) {
	e := newEnv(t, Options{}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	for i := 0; i < 3; i++ {
		e.at(t0.Add(time.Duration(i)*time.Second), e.ev("api", "app", fmt.Sprint("e", i), model.SeverityError))
	}
	at := t0.Add(31 * time.Minute)
	e.clock.Set(at)
	e.d.flushDue(at)
	e.pumpErr(&HTTPError{Status: 503})
	b := e.buffer("slack")
	if b == nil || !b.Retried || b.Events != 3 || !b.Due.Equal(at.Add(30*time.Minute)) || !b.Opened.Equal(t0) {
		t.Fatalf("merged back = %+v", b)
	}
	e.clock.Set(b.Due)
	e.d.flushDue(b.Due)
	msgs := e.pumpErr(&HTTPError{Status: 503})
	if len(msgs) != 1 || !msgs[0].Digest.Retried {
		t.Fatalf("second attempt: %+v", msgs)
	}
	if e.buffer("slack") != nil || e.d.DigestsFailed.Load() != 1 || e.d.LostEvents.Load() != 3 {
		t.Errorf("after the second failure: buffer %+v, failed %d, lost %d", e.buffer("slack"), e.d.DigestsFailed.Load(), e.d.LostEvents.Load())
	}
}

// 17. Every decision and message says why, in one sentence.
func TestDecisionsAndReasons(t *testing.T) {
	e := newEnv(t, Options{Warmup: -1}, &config.Config{
		Notifiers: []config.Notifier{chat("dev-slack"), pager("oncall-pager"), chat("ops")},
		Routes: []config.Route{
			smartRoute("dev-slack: error and above", model.SeverityError, 30, 3600, "dev-slack", "oncall-pager"),
			immediateRoute("dev-errors", model.SeverityError, 900, "ops"),
		},
	})
	text := func(decs []Decision, nid string) string { return decisionFor(t, decs, nid).Reason.Text }
	check := func(got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}
	decs := e.at(t0, e.ev("checkout-api", "database", "c", model.SeverityCritical))
	check(text(decs, "dev-slack"), `Critical: at or above "right away" (critical) on "dev-slack: error and above". It is also new: nothing like it in the last 7 days. Repeats for the next 1 h go into the digest.`)
	check(text(decs, "oncall-pager"), `Critical: at or above "right away" (critical) on "dev-slack: error and above". It is also new: nothing like it in the last 7 days. Repeats for the next 1 h are counted and reported with the next alert.`)
	check(text(decs, "ops"), `Matches "dev-errors", which sends every matching event right away. Repeats are held back for 15 min.`)
	decs = e.at(t0.Add(time.Minute), e.ev("checkout-api", "database", "c", model.SeverityCritical))
	check(text(decs, "dev-slack"), "Repeat of the alert at 09:00 UTC: in the digest due 09:31 UTC.")
	check(text(decs, "oncall-pager"), "Repeat of the alert at 09:00 UTC: counted, and reported with the next alert after 10:00 UTC.")
	check(text(decs, "ops"), "Repeat of the alert at 09:00 UTC: counted, and reported with the next alert after 09:15 UTC.")
	decs = e.at(t0.Add(2*time.Minute), e.ev("checkout-api", "network", "n", model.SeverityError))
	check(text(decs, "dev-slack"), "New problem: first seen just now; nothing like it in the last 7 days.")
	decs = e.at(t0.Add(3*time.Minute), e.ev("checkout-api", "network", "n", model.SeverityError))
	check(text(decs, "dev-slack"), "In the digest due 09:31 UTC. Right away: critical and up, and errors that are new, spiking or getting worse. Everything else from error up: one digest every 30 min.")
	check(text(decs, "oncall-pager"), "oncall-pager is Pager (test): it gets only what is sent right away (here: critical and up), never digests.")
	decs = e.at(t0.Add(16*time.Minute), e.ev("checkout-api", "database", "c", model.SeverityCritical))
	check(text(decs, "ops"), "Still happening: 1 more since the alert at 09:00 UTC.")
	e.pump()

	// Delivery records carry the kind, the reason and the event count.
	var del Delivery
	for _, d := range e.d.Deliveries(0, KindAlert) {
		if d.NotifierID == "ops" && d.Reason == ReasonRepeat {
			del = d
		}
	}
	if del.Kind != KindAlert || del.Events != 2 || del.ReasonText != "Still happening: 1 more since the alert at 09:00 UTC." || del.Fingerprint != "c" ||
		!slices.Equal(del.Routes, []string{"dev-errors"}) || del.ID == "" {
		t.Errorf("alert delivery = %+v", del)
	}
	id, ok := e.d.FlushDigest("dev-slack")
	if !ok {
		t.Fatal("nothing to flush")
	}
	if del, dg, ok := e.d.DeliveryDetail(id); !ok || del.Status != "queued" || dg == nil || dg.ID != id {
		t.Errorf("queued digest detail: %+v %v", del, ok)
	}
	e.pump()
	dm := e.sinks["dev-slack"].digests()[0]
	check(dm.Reason.Text, `Sent on request ("Send now").`)
	del, dg, ok := e.d.DeliveryDetail(id)
	if !ok || dg == nil || del.Kind != KindDigest || del.Reason != ReasonManual || del.Events != dg.Events || del.Patterns != 2 || del.Status != "sent" {
		t.Errorf("digest delivery = %+v (%v)", del, ok)
	}
	if ds := e.d.Deliveries(0, KindDigest); len(ds) != 1 || ds[0].ID != id {
		t.Errorf("kind filter: %+v", ds)
	}
	if _, ok := e.d.FlushDigest("dev-slack"); ok {
		t.Error("flushed an empty buffer")
	}

	// Preview reads no state: a critical, an error and the pager.
	p := e.d.Preview(&model.Event{SourceID: "x", Severity: model.SeverityCritical, Fingerprint: "c"})
	if d := decisionFor(t, p, "dev-slack"); d.Action != ActionRightAway || d.Reason.Text != `Right away (critical on "dev-slack: error and above").` {
		t.Errorf("preview critical: %+v", d)
	}
	if d := decisionFor(t, p, "oncall-pager"); d.Action != ActionRightAway {
		t.Errorf("preview critical on the pager: %+v", d)
	}
	p = e.d.Preview(&model.Event{SourceID: "x", Severity: model.SeverityError, Fingerprint: "new-one"})
	if d := decisionFor(t, p, "dev-slack"); d.Action != ActionDigest || d.Reason.Text != "In the next digest (every 30 min), unless it is new, spiking or getting worse." {
		t.Errorf("preview error: %+v", d)
	}
	if d := decisionFor(t, p, "oncall-pager"); d.Action != ActionSkipped {
		t.Errorf("preview error on the pager: %+v", d)
	}
	if d := decisionFor(t, p, "ops"); d.Action != ActionRightAway || d.Reason.Text != `Right away (every matching event on "dev-errors").` {
		t.Errorf("preview immediate: %+v", d)
	}

	// Back after a long silence.
	decs = e.at(t0.Add(12*24*time.Hour), e.ev("checkout-api", "network", "n", model.SeverityError))
	check(text(decs, "dev-slack"), "Back after 11 days: last seen Oct 19 09:03 UTC.")
	if got := e.d.Decisions("nope"); got != nil {
		t.Errorf("unknown event: %+v", got)
	}
}

// policySentence wording, which the UI mirrors.
func TestPolicySentence(t *testing.T) {
	smart := func(imm model.Severity, minutes int) config.DeliveryPolicy {
		return config.DeliveryPolicy{Delivery: config.DeliverySmart, ImmediateSeverity: imm, DigestMinutes: minutes}
	}
	for _, tc := range []struct {
		p        config.DeliveryPolicy
		min      model.Severity
		throttle time.Duration
		want     string
	}{
		{smart(model.SeverityCritical, 30), model.SeverityError, time.Hour, "Right away: critical and up, and errors that are new, spiking or getting worse. Everything else from error up: one digest every 30 min."},
		{smart(model.SeverityCritical, 60), model.SeverityWarning, time.Hour, "Right away: critical and up, and errors that are new, spiking or getting worse. Everything else from warning up: one digest every 1 h."},
		{smart(model.SeverityError, 30), model.SeverityError, time.Hour, "Everything from error up right away, with flood protection; repeats in a digest every 30 min."},
		{smart(model.SeverityError, 90), model.SeverityWarning, time.Hour, "Right away: error and up. Everything else from warning up: one digest every 1 h 30 min."},
		{config.DeliveryPolicy{Delivery: config.DeliveryDigest, DigestMinutes: 60}, model.SeverityError, 0, "Everything this route matches: one digest every 1 h."},
		{config.DeliveryPolicy{Delivery: config.DeliveryImmediate}, model.SeverityError, 15 * time.Minute, "Every matching event right away; repeats of a problem are held back for 15 min."},
		{config.DeliveryPolicy{}, model.SeverityError, 0, "Every matching event right away."},
	} {
		if got := policySentence(tc.p, tc.min, tc.throttle); got != tc.want {
			t.Errorf("%+v %s: %q", tc.p, tc.min, got)
		}
	}
	if fmtDur(90*time.Second) != "2 min" || fmtDur(30*time.Second) != "30 s" || fmtDur(24*time.Hour) != "24 h" {
		t.Error("fmtDur")
	}
}
