package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine/enginetest"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// An answer is shared only by events the engine reads the same way, so one
// source can't plant the answer another source's event gets.
func TestAnswersAreSharedOnlyForTheSameText(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	msg := "ERROR card vault replica lost quorum, refusing writes for merchant 4411"
	victim := event(msg)
	planted := event(msg)
	planted.SourceID = "other"
	planted.Fields = map[string]any{"event": "deployment rollout finished"}
	cl.Enrich(context.Background(), &planted)
	cl.Enrich(context.Background(), &victim)
	if fake.Calls.Load() != 2 {
		t.Errorf("events with different fields shared an answer: %d calls", fake.Calls.Load())
	}
	again := event(strings.Replace(msg, "4411", "5522", 1))
	cl.Enrich(context.Background(), &again)
	if fake.Calls.Load() != 2 {
		t.Errorf("the same source's text with other numbers should share the answer: %d calls", fake.Calls.Load())
	}
	// Numbers and case still matter to the engine, so another source
	// gets its own answer even for text that normalizes the same.
	cross := event(strings.ToUpper(strings.Replace(msg, "4411", "3306", 1)))
	cross.SourceID = "third"
	cl.Enrich(context.Background(), &cross)
	if fake.Calls.Load() != 3 {
		t.Errorf("another source's event shared an answer: %d calls", fake.Calls.Load())
	}
	long := event(msg + "\n" + strings.Repeat("at frame\n", 60) + "Caused by: java.io.IOException: disk full")
	other := event(msg + "\n" + strings.Repeat("at frame\n", 60) + "Caused by: javax.net.ssl.SSLHandshakeException: expired")
	cl.Enrich(context.Background(), &long)
	cl.Enrich(context.Background(), &other)
	if fake.Calls.Load() != 5 {
		t.Errorf("records differing past 400 bytes shared an answer: %d calls", fake.Calls.Load())
	}
}

func TestAnswersOutsideTheQuestionAreRejected(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	fake.Decide = func(string, enginetest.Question) enginetest.Answer {
		return enginetest.Answer{Choice: "pwned-by-engine", Probs: map[string]float64{"pwned-by-engine": 0.9}}
	}
	ev := event("ERROR something nobody categorized")
	cl.Enrich(context.Background(), &ev)
	if ev.Category == "pwned-by-engine" || ev.Engine != nil || cl.Status().CachedResults != 0 {
		t.Errorf("a label that wasn't offered was applied: %q %+v", ev.Category, ev.Engine)
	}
	// (A broken answer counts as an engine failure; start afresh.)
	cl, fake, _ = setup(t, nil)
	fake.Decide = func(string, enginetest.Question) enginetest.Answer {
		return enginetest.Answer{Choice: "database", Probs: map[string]float64{"database": 7.5}}
	}
	ev = event("ERROR another thing")
	cl.Enrich(context.Background(), &ev)
	if ev.Engine != nil {
		t.Errorf("a confidence over 1 was applied: %+v", ev.Engine)
	}
	probs := map[string]float64{"database": 0.9}
	for _, extra := range []string{"label-a", "label-b", "label-c"} {
		probs[extra] = 0.01
	}
	cl, fake, _ = setup(t, nil)
	fake.Decide = func(string, enginetest.Question) enginetest.Answer {
		return enginetest.Answer{Choice: "database", Probs: probs}
	}
	ev = event("ERROR third thing")
	r, _, err := cl.classify(context.Background(), &ev, textKey(&ev))
	if err != nil {
		t.Fatal(err)
	}
	for k := range r.Probabilities {
		if strings.HasPrefix(k, "label-") {
			t.Errorf("probability for %q, which wasn't offered, was kept", k)
		}
	}
}

func TestEngineURLCredentialsStayHidden(t *testing.T) {
	var logs bytes.Buffer
	cl := New(slog.New(slog.NewTextHandler(&logs, nil)))
	cl.syncHealth = true
	t.Cleanup(cl.Close)
	c := config.Defaults()
	c.Settings.Engine.URL = "http://laya:hunter2@127.0.0.1:1, https://tok_9f8e7d@127.0.0.1:2"
	c.Settings.Engine.APIKey = ""
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	cl.Configure(c)
	cl.mu.Lock()
	cl.loaded = map[string]bool{"english": true}
	cl.mu.Unlock()
	ev := event("ERROR x")
	cl.Enrich(context.Background(), &ev)
	eps, _ := cl.Probe(context.Background())
	st := cl.Status()
	all := logs.String() + st.LastError + strings.Join(st.URLs, " ")
	for _, ep := range eps {
		all += ep.URL + ep.Error
	}
	if strings.Contains(all, "hunter2") || strings.Contains(all, "tok_9f8e7d") {
		t.Errorf("credentials shown: %s", all)
	}
}

func TestReferencedURLIsNotEchoed(t *testing.T) {
	dir := t.TempDir()
	dirs := config.FileRefDirs()
	config.SetFileRefDirs([]string{dir})
	t.Cleanup(func() { config.SetFileRefDirs(dirs) })
	t.Setenv("LISSANDRA_TEST_SECRET", "hvs.super-secret-token")
	cl, _, cfg := setup(t, nil)
	cfg.Settings.Engine.URL = "${env:LISSANDRA_TEST_SECRET}"
	cl.Configure(cfg)
	if st := cl.Status(); st.ConfigError == "" || strings.Contains(st.ConfigError, "super-secret") {
		t.Errorf("config error: %q", st.ConfigError)
	}
}

// The event's classifier is the engine's only when its category was used;
// a severity alone leaves the category's classifier.
func TestSeverityOnlyAnswerKeepsTheClassifier(t *testing.T) {
	cl, fake, _ := setup(t, func(c *config.Config) {
		c.Settings.Engine.SetSeverity = true
		c.Settings.Engine.OnlyUncategorized = false
	})
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		if q.ID == "severity" {
			return enginetest.Answer{Probs: map[string]float64{q.Labels[4]: 0.9}}
		}
		return enginetest.Answer{Choice: "network", Probs: map[string]float64{"network": 0.9}}
	}
	ev := event("upstream hiccup")
	ev.Category, ev.Classifier = "database", "rules"
	ev.Severity, ev.SeverityGuessed = model.SeverityWarning, true
	cl.Enrich(context.Background(), &ev)
	if ev.Severity != model.SeverityError || !ev.Engine.SeverityApplied || ev.Engine.CategoryApplied || ev.Classifier != "rules" {
		t.Errorf("severity-only answer: classifier %q, engine %+v", ev.Classifier, ev.Engine)
	}
}

// With no categories, only the severity is asked, and the tester refuses
// when there is nothing to ask.
func TestNoCategories(t *testing.T) {
	cl, fake, _ := setup(t, func(c *config.Config) {
		c.Settings.Categories = nil
		c.Settings.Engine.SetSeverity = true
	})
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		return enginetest.Answer{Probs: map[string]float64{q.Labels[4]: 0.9}}
	}
	ev := event("upstream hiccup")
	ev.Severity, ev.SeverityGuessed = model.SeverityWarning, true
	r, err := cl.Classify(context.Background(), &ev)
	if err != nil || r.CategoryAsked || r.Severity != model.SeverityError {
		t.Errorf("severity only: %+v %v", r, err)
	}
	cl.Enrich(context.Background(), &ev)
	if ev.Engine == nil || ev.Engine.Category != "" {
		t.Errorf("no category was asked, so none is recorded: %+v", ev.Engine)
	}
	stated := event("upstream hiccup")
	stated.Severity = model.SeverityWarning
	if _, err := cl.Classify(context.Background(), &stated); err == nil || !strings.Contains(err.Error(), "nothing to ask") {
		t.Errorf("nothing to ask: %v", err)
	}
}

// Fields that identify a request, trace or user differ per event; they are
// left out of what the engine reads, so they don't multiply requests.
func TestIDFieldsDontMultiplyRequests(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	for i, u := range []string{"alice", "bob", "carol", "dave", "erin"} {
		ev := event("ERROR request failed")
		ev.Fields = map[string]any{"user": u, "request_id": fmt.Sprintf("req-%c%d", 'a'+i, i), "traceId": u + "-trace", "err": "connection refused"}
		cl.Enrich(context.Background(), &ev)
	}
	if n := fake.Calls.Load(); n != 1 {
		t.Errorf("%d requests for one message with per-request ids", n)
	}
}
