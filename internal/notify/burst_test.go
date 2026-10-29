package notify

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// burstConfig is the wizard's channels: email takes warnings, Slack errors
// and up, the pager criticals.
func burstConfig() *config.Config {
	return &config.Config{
		Notifiers: []config.Notifier{chat("email"), chat("slack"), pager("pd")},
		Routes: []config.Route{
			smartRoute("email: warning and above", model.SeverityWarning, 60, 3600, "email"),
			smartRoute("slack: error and above", model.SeverityError, 30, 3600, "slack"),
			immediateRoute("pd: critical and above", model.SeverityCritical, 900, "pd"),
		},
	}
}

// sshFailure is one failed login from node-syslog, one of seven patterns.
func (e *env) sshFailure(i int, sev model.Severity) model.Event {
	ev := e.ev("node-syslog", "auth", fmt.Sprint("ssh-", i%7), sev)
	ev.Message = fmt.Sprintf("Failed password for invalid user u%d from 203.0.113.50 port %d ssh2", i%7, 40000+i)
	return ev
}

// bursts lists the burst alerts sent to nid.
func (e *env) bursts(nid string) []*Message {
	var out []*Message
	for _, m := range e.sinks[nid].alerts() {
		if m.Reason.Code == ReasonBurst {
			out = append(out, m)
		}
	}
	return out
}

// 21. Many auth or security warnings from one source, across patterns, go
// right away once: the event that makes 20 in 5 minutes.
func TestBurst(t *testing.T) {
	t.Run("the 20th in 5 minutes", func(t *testing.T) {
		e := newEnv(t, Options{}, burstConfig()) // warming up: nothing is new
		for i := 0; i < 45; i++ {
			decs := e.at(t0.Add(time.Duration(i)*700*time.Millisecond), e.sshFailure(i, model.SeverityWarning))
			if len(decs) != 1 { // warnings reach email only
				t.Fatalf("event %d: %+v", i, decs)
			}
			want := ActionDigest
			if i == 19 {
				want = ActionSent
			}
			if d := decisionFor(t, decs, "email"); d.Action != want {
				t.Fatalf("event %d: %s (%q)", i, d.Action, d.Reason.Text)
			}
		}
		e.pump()
		bs := e.bursts("email")
		if len(bs) != 1 {
			t.Fatalf("bursts = %d", len(bs))
		}
		m := bs[0]
		if m.Reason.Text != "Burst: 20 auth warnings or worse from node-syslog in the last 5 minutes." ||
			m.Headline() != "[WARNING] auth · node-syslog · burst" || m.Event.Fingerprint != "ssh-5" {
			t.Errorf("burst alert: %q %q %s", m.Reason.Text, m.Headline(), m.Event.Fingerprint)
		}
		if e.rightAway(ReasonBurst) != 1 || len(e.sinks["slack"].all())+len(e.sinks["pd"].all()) != 0 {
			t.Error("counted or sent elsewhere")
		}
		// the rest stay in the digest: every event reported once
		e.flush(t0.Add(time.Hour))
		ds := e.sinks["email"].digests()
		if len(ds) != 1 || ds[0].Digest.Events != 44 || len(e.sinks["email"].alerts()) != 1 {
			t.Fatalf("digests %d, alerts %d", len(ds), len(e.sinks["email"].alerts()))
		}
	})
	t.Run("spread out, other categories, backlog", func(t *testing.T) {
		e := newEnv(t, Options{}, burstConfig())
		for _, start := range []time.Duration{0, 11 * time.Minute} { // 19 in 5 minutes, twice
			for i := 0; i < 19; i++ {
				e.at(t0.Add(start+time.Duration(i)*15*time.Second), e.sshFailure(i, model.SeverityWarning))
			}
		}
		for i := 0; i < 50; i++ {
			at := t0.Add(20*time.Minute + time.Duration(i)*time.Second)
			e.at(at, e.ev("node-syslog", "database", fmt.Sprint("db-", i%5), model.SeverityWarning))
			e.at(at, e.ev("node-syslog", "application", fmt.Sprint("app-", i%5), model.SeverityError))
			old := e.sshFailure(i, model.SeverityWarning) // a pull source catching up
			old.Timestamp = at.Add(-time.Hour)
			e.at(at, old)
		}
		e.pump()
		if n := len(e.sinks["email"].alerts()) + len(e.sinks["slack"].alerts()); n != 0 || e.rightAway(ReasonBurst) != 0 {
			t.Errorf("%d alerts", n)
		}
	})
	t.Run("errors count, once an hour", func(t *testing.T) {
		e := newEnv(t, Options{}, burstConfig())
		var at time.Time
		for i := 0; at.Before(t0.Add(2*time.Hour + 30*time.Minute)); i++ {
			at = t0.Add(time.Duration(i) * 10 * time.Second)
			sev := model.SeverityWarning
			if i%2 == 0 {
				sev = model.SeverityError
			}
			e.at(at, e.sshFailure(i, sev))
			e.pump()
		}
		// the 20th at 190 s, then the first event an hour after each burst
		for _, nid := range []string{"email", "slack"} {
			bs := e.bursts(nid)
			if nid == "email" && (len(bs) != 3 || !bs[0].at.Equal(t0.Add(190*time.Second)) ||
				!bs[1].at.Equal(t0.Add(time.Hour+190*time.Second)) || !bs[2].at.Equal(t0.Add(2*time.Hour+190*time.Second))) {
				t.Errorf("%s bursts: %d", nid, len(bs))
			}
			// Slack sees only the errors: the burst counts them all, and the
			// route that matches the event that makes it sends it.
			if nid == "slack" && len(bs) != 0 {
				t.Errorf("slack got a burst alert from a warning: %d", len(bs))
			}
		}
	})
	t.Run("flood protection", func(t *testing.T) {
		cfg := burstConfig()
		e := newEnv(t, Options{Warmup: -1}, cfg)
		// a new auth error goes right away and uses the source's promotion
		wantDecision(t, e.at(t0, e.sshFailure(0, model.SeverityError)), "email", ActionSent, ReasonNew)
		for i := 1; i < 19; i++ {
			wantDecision(t, e.at(t0.Add(time.Duration(i)*time.Second), e.sshFailure(i, model.SeverityWarning)), "email", ActionDigest, ReasonDigest)
		}
		d := wantDecision(t, e.at(t0.Add(19*time.Second), e.sshFailure(1, model.SeverityWarning)), "email", ActionHeld, ReasonSourceLimit)
		if d.Due.After(t0.Add(19*time.Second + limitWindow)) {
			t.Errorf("held until %v", d.Due)
		}
		wantDecision(t, e.at(t0.Add(20*time.Second), e.sshFailure(2, model.SeverityWarning)), "email", ActionDigest, ReasonDigest) // told once
		e.pump()
		e.flush(d.Due)
		ds := e.sinks["email"].digests()
		if len(ds) != 1 || ds[0].Digest.Reason != ReasonSummary || ds[0].Digest.Entries[0].Held != ReasonBurst {
			t.Fatalf("summary: %+v", ds)
		}

		// ten right-away alerts in 10 minutes fill a channel: the burst waits
		e = newEnv(t, Options{Warmup: -1}, cfg)
		for i := 0; i < channelLimit; i++ {
			wantDecision(t, e.at(t0.Add(time.Duration(i)*time.Second), e.ev(fmt.Sprint("svc-", i), "app", fmt.Sprint("e-", i), model.SeverityError)), "email", ActionSent, ReasonNew)
		}
		for i := 0; i < 19; i++ {
			e.at(t0.Add(time.Minute+time.Duration(i)*time.Second), e.sshFailure(i, model.SeverityWarning))
		}
		wantDecision(t, e.at(t0.Add(2*time.Minute), e.sshFailure(19, model.SeverityWarning)), "email", ActionHeld, ReasonChannelLimit)
	})
	t.Run("a critical in a burst tells it", func(t *testing.T) {
		e := newEnv(t, Options{}, burstConfig())
		for i := 0; i < 19; i++ {
			e.at(t0.Add(time.Duration(i)*time.Second), e.sshFailure(i, model.SeverityWarning))
		}
		decs := e.at(t0.Add(19*time.Second), e.sshFailure(19, model.SeverityCritical))
		d := wantDecision(t, decs, "email", ActionSent, ReasonSeverity)
		if !strings.Contains(d.Reason.Text, " It is also part of a burst: 20 auth warnings or worse from node-syslog in the last 5 minutes.") {
			t.Errorf("why: %q", d.Reason.Text)
		}
		wantDecision(t, decs, "pd", ActionSent, ReasonEvery) // the pager gets the critical, never a burst
		wantDecision(t, e.at(t0.Add(20*time.Second), e.sshFailure(20, model.SeverityWarning)), "email", ActionDigest, ReasonDigest)
	})
	t.Run("survives a restart", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerting.json")
		e := newEnv(t, Options{StatePath: path}, burstConfig())
		for i := 0; i < 15; i++ {
			e.at(t0.Add(time.Duration(i)*time.Second), e.sshFailure(i, model.SeverityWarning))
		}
		if err := e.d.SaveState(); err != nil {
			t.Fatal(err)
		}
		if s := readState(t, path); s.Bursts["node-syslog|auth"] == nil || s.Bursts["node-syslog|auth"].Recent[0] != 15 {
			t.Fatalf("saved bursts: %+v", s.Bursts)
		}
		e = e.restart(t, path)
		for i := 15; i < 19; i++ {
			wantDecision(t, e.at(t0.Add(time.Duration(i)*time.Second), e.sshFailure(i, model.SeverityWarning)), "email", ActionDigest, ReasonDigest)
		}
		wantDecision(t, e.at(t0.Add(19*time.Second), e.sshFailure(19, model.SeverityWarning)), "email", ActionSent, ReasonBurst)
		e.pump()
		if err := e.d.SaveState(); err != nil {
			t.Fatal(err)
		}
		e = e.restart(t, path)
		wantDecision(t, e.at(t0.Add(30*time.Minute), e.sshFailure(20, model.SeverityWarning)), "email", ActionDigest, ReasonDigest)
		e.d.prune(t0.Add(50 * time.Minute))
		if e.d.al.bursts["node-syslog|auth"] == nil {
			t.Error("pruned a burst told in the last hour")
		}
		e.d.prune(t0.Add(90 * time.Minute))
		if len(e.d.al.bursts) != 0 {
			t.Error("kept a burst state nothing needs")
		}
	})
	t.Run("preview", func(t *testing.T) {
		e := newEnv(t, Options{}, burstConfig())
		for _, c := range []struct {
			cat  string
			sev  model.Severity
			want string
		}{
			{"auth", model.SeverityWarning, "In the next digest (every 1 h), unless it is part of a burst: 20 auth warnings or worse from one source in 5 minutes."},
			{"security", model.SeverityError, "In the next digest (every 1 h), unless it is new, spiking, getting worse or part of a burst."},
			{"database", model.SeverityWarning, "In the next digest (every 1 h)."},
		} {
			p := e.d.Preview(&model.Event{SourceID: "x", Category: c.cat, Severity: c.sev, Fingerprint: "f"})
			if d := decisionFor(t, p, "email"); d.Action != ActionDigest || d.Reason.Text != c.want {
				t.Errorf("%s %s: %q", c.cat, c.sev, d.Reason.Text)
			}
		}
		if l := e.d.AlertingStatus().Limits; l.BurstEvents != 20 || strings.Join(l.BurstCategories, ",") != "auth,security" {
			t.Errorf("limits: %+v", l)
		}
	})
}
