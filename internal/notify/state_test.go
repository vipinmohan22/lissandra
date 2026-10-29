package notify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

func stateConfig() *config.Config {
	return &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	}
}

// restart builds a second dispatcher on the same state file and clock, as
// a restart would.
func (e *env) restart(t *testing.T, path string) *env {
	t.Helper()
	d := NewDispatcherWith(quiet, Options{StatePath: path, Now: e.clock.Now})
	d.Configure(e.cfg)
	return &env{t: t, d: d, clock: e.clock, cfg: e.cfg, sinks: e.sinks, n: e.n}
}

func readState(t *testing.T, path string) savedState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s savedState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func writeState(t *testing.T, path string, s any) {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 15. Windows, warm-up, baselines and pending digests survive a restart;
// a file that can't be used is set aside.
func TestStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting.json")
	e := newEnv(t, Options{StatePath: path}, stateConfig())
	e.at(t0, e.ev("api", "app", "x", model.SeverityError)) // warming up: batched
	e.flush(t0.Add(30 * time.Minute))
	wantDecision(t, e.at(t0.Add(61*time.Minute), e.ev("api", "app", "a", model.SeverityError)), "slack", ActionSent, ReasonNew)
	e.pump()
	wantDecision(t, e.at(t0.Add(62*time.Minute), e.ev("api", "app", "a", model.SeverityError)), "slack", ActionDigest, ReasonDigest)
	wantDecision(t, e.at(t0.Add(62*time.Minute), e.ev("api", "app", "c", model.SeverityCritical)), "slack", ActionSent, ReasonSeverity)
	e.pump()
	if err := e.d.SaveState(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", info, err)
	}
	if st := e.d.AlertingStatus().State; !st.Saved || st.File != "alerting.json" || st.SavedAt == nil || st.Error != "" {
		t.Errorf("state status = %+v", st)
	}

	e2 := e.restart(t, path)
	e2.clock.Set(t0.Add(63 * time.Minute))
	wantDecision(t, e2.handle(e2.ev("api", "app", "c", model.SeverityCritical)), "slack", ActionThrottled, ReasonThrottled)
	wantDecision(t, e2.handle(e2.ev("api", "network", "n", model.SeverityError)), "slack", ActionSent, ReasonNew) // still warm
	e2.d.amu.Lock()
	total, promoted := e2.d.al.patterns["a"].Total, e2.d.al.promoted["slack|api|app"]
	e2.d.amu.Unlock()
	if total != 2 || !promoted.Equal(t0.Add(61*time.Minute)) {
		t.Errorf("pattern total = %d, promoted %v", total, promoted)
	}
	before := len(e.sinks["slack"].digests())
	e2.flush(t0.Add(91 * time.Minute))
	if len(e.sinks["slack"].digests()) != before {
		t.Error("the pending digest went out early")
	}
	e2.flush(t0.Add(92 * time.Minute))
	ds := e.sinks["slack"].digests()
	if len(ds) != before+1 || ds[len(ds)-1].Digest.Events != 2 || !ds[len(ds)-1].Digest.From.Equal(t0.Add(62*time.Minute)) {
		t.Fatalf("the pending digest at its due: %d digests", len(ds))
	}

	// Overdue digests go out when Run starts.
	e2.at(t0.Add(93*time.Minute), e2.ev("api", "app", "a", model.SeverityError))
	if err := e2.d.SaveState(); err != nil {
		t.Fatal(err)
	}
	e2.clock.Set(t0.Add(5 * time.Hour))
	e3 := e2.restart(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e3.d.Run(ctx, 1); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.sinks["slack"].digests()) == before+1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(e.sinks["slack"].digests()) != before+2 {
		t.Error("an overdue digest didn't go out at Run")
	}

	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "alerting.json")
		os.WriteFile(p, []byte("{nope"), 0o600)
		e := newEnv(t, Options{StatePath: p}, stateConfig())
		if bad, _ := filepath.Glob(p + ".bad-*"); len(bad) != 1 {
			t.Errorf("set aside: %v", bad)
		}
		wantDecision(t, e.at(t0, e.ev("api", "app", "n", model.SeverityError)), "slack", ActionDigest, ReasonDigest) // warming up again
	})
	t.Run("version 2", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "alerting.json")
		writeState(t, p, map[string]any{"version": 2})
		newEnv(t, Options{StatePath: p}, stateConfig())
		if bad, _ := filepath.Glob(p + ".bad-*"); len(bad) != 1 {
			t.Errorf("set aside: %v", bad)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Error("the unknown version was left in place")
		}
	})
	t.Run("pattern version", func(t *testing.T) {
		s := readState(t, path)
		if s.PatternVersion != classify.PatternVersion || len(s.Patterns) == 0 || len(s.Alerts) == 0 || len(s.Sources) == 0 || len(s.Digests) == 0 {
			t.Fatalf("saved state = %+v", s)
		}
		s.PatternVersion--
		p := filepath.Join(t.TempDir(), "alerting.json")
		writeState(t, p, s)
		e := newEnv(t, Options{StatePath: p}, stateConfig())
		e.d.amu.Lock()
		defer e.d.amu.Unlock()
		if len(e.d.al.patterns)+len(e.d.al.alerts)+len(e.d.al.sources) != 0 || len(e.d.al.digests) != 1 {
			t.Errorf("after a pattern change: %d patterns, %d alerts, %d sources, %d digests", len(e.d.al.patterns), len(e.d.al.alerts), len(e.d.al.sources), len(e.d.al.digests))
		}
	})
	t.Run("future times", func(t *testing.T) {
		s := readState(t, path)
		future := t0.Add(48 * time.Hour)
		for _, src := range s.Sources {
			src.First, src.Last = future, future
		}
		for _, st := range s.Alerts {
			st.LastSent = future
		}
		for _, b := range s.Digests {
			b.Opened, b.Due = future, future.Add(time.Hour)
		}
		p := filepath.Join(t.TempDir(), "alerting.json")
		writeState(t, p, s)
		e := newEnv(t, Options{StatePath: p}, stateConfig())
		e.d.amu.Lock()
		defer e.d.amu.Unlock()
		for _, src := range e.d.al.sources {
			if !src.First.Equal(t0) {
				t.Errorf("source first = %v", src.First)
			}
		}
		for _, st := range e.d.al.alerts {
			if !st.LastSent.Equal(t0) {
				t.Errorf("last sent = %v", st.LastSent)
			}
		}
		for _, b := range e.d.al.digests {
			if !b.Opened.Equal(t0) || b.Due.After(t0.Add(time.Duration(b.Interval)*time.Minute)) {
				t.Errorf("digest %v → %v", b.Opened, b.Due)
			}
		}
	})
}

// 16. Drain keeps digests not yet due for after the restart, unless the
// state can't be saved: then it sends them.
func TestDrainKeepsOrSends(t *testing.T) {
	run := func(t *testing.T, e *env) func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { e.d.Run(ctx, 1); close(done) }()
		return func() { cancel(); <-done }
	}
	drain := func(t *testing.T, e *env) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := e.d.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	batch := func(e *env) {
		for i := 0; i < 3; i++ {
			e.at(t0, e.ev("api", "app", string(rune('a'+i)), model.SeverityError))
		}
	}
	t.Run("saved", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerting.json")
		e := newEnv(t, Options{StatePath: path}, stateConfig())
		stop := run(t, e)
		defer stop()
		batch(e)
		drain(t, e)
		if n := len(e.sinks["slack"].all()); n != 0 {
			t.Errorf("%d messages at shutdown", n)
		}
		if s := readState(t, path); s.Digests["slack"] == nil || s.Digests["slack"].Events != 3 {
			t.Errorf("saved digests = %+v", s.Digests)
		}
		e.d.flushDue(t0.Add(time.Hour))
		if e.buffer("slack") == nil {
			t.Error("flushed after Drain")
		}
	})
	for name, path := range map[string]func(t *testing.T) string{
		"unwritable": func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "file")
			os.WriteFile(file, nil, 0o600)
			return filepath.Join(file, "alerting.json") // a file where the directory should be
		},
		"no state path": func(t *testing.T) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Options{StatePath: path(t)}, stateConfig())
			stop := run(t, e)
			defer stop()
			batch(e)
			drain(t, e)
			ds := e.sinks["slack"].digests()
			if len(ds) != 1 || ds[0].Digest.Reason != ReasonShutdown || ds[0].Digest.Events != 3 ||
				ds[0].Reason.Text != "Sent at shutdown: alert state couldn't be saved for after the restart." {
				t.Fatalf("digests at shutdown: %+v", ds)
			}
			if name == "unwritable" && (e.d.StateSaveErrors.Load() == 0 || e.d.AlertingStatus().State.Error == "") {
				t.Error("the save error isn't reported")
			}
		})
	}
	t.Run("failing during drain", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerting.json")
		e := newEnv(t, Options{StatePath: path}, stateConfig())
		e.sinks["slack"].fail = func(*Message) error { return &HTTPError{Status: 503} }
		stop := run(t, e)
		defer stop()
		batch(e)
		e.clock.Set(t0.Add(31 * time.Minute)) // due
		drain(t, e)
		s := readState(t, path)
		if b := s.Digests["slack"]; b == nil || !b.Retried || b.Events != 3 {
			t.Errorf("saved after a failed digest: %+v", s.Digests)
		}
		if e.d.LostEvents.Load() != 0 {
			t.Error("events lost")
		}
	})
}

func TestSaveStateWithoutPath(t *testing.T) {
	e := newEnv(t, Options{}, stateConfig())
	if err := e.d.SaveState(); err != nil {
		t.Error(err)
	}
	if st := e.d.AlertingStatus().State; st.Saved || st.SavedAt != nil {
		t.Errorf("status = %+v", st)
	}
}

// A source seen only through events no route matches is saved by the next
// minute's save too, so a crash doesn't restart its warm-up.
func TestNewSourceIsSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting.json")
	e := newEnv(t, Options{StatePath: path}, stateConfig())
	e.at(t0, e.ev("quiet", "app", "hello", model.SeverityInfo)) // matches no route
	e.d.amu.Lock()
	dirty := e.d.al.dirty
	e.d.amu.Unlock()
	if !dirty {
		t.Fatal("a new source doesn't mark alert state changed, so the minute's save skips it")
	}
	if err := e.d.SaveState(); err != nil {
		t.Fatal(err)
	}
	e2 := e.restart(t, path)
	wantDecision(t, e2.at(t0.Add(61*time.Minute), e2.ev("quiet", "app", "x", model.SeverityError)), "slack", ActionSent, ReasonNew)
}
