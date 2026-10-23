package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/engine/enginetest"
	"github.com/vipinmohan22/layalogs/internal/learn"
	"github.com/vipinmohan22/layalogs/internal/model"
)

// waitFor polls cond for up to 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) ingest(src config.Source, body string) {
	e.t.Helper()
	if code, data := e.do("POST", "/ingest/"+src.ID, body, map[string]string{"Authorization": "Bearer " + src.IngestToken}); code != 202 {
		e.t.Fatalf("ingest: %d %s", code, data)
	}
}

// latest returns the newest stored event once there are n.
func (e *env) latest(n int) model.Event {
	e.t.Helper()
	var evs []model.Event
	waitFor(e.t, "events", func() bool {
		evs = nil // decoding into the old elements would keep omitted fields
		e.admin("GET", "/api/events?limit=50", nil, &evs)
		return len(evs) >= n
	})
	return evs[0]
}

// A pattern the rules leave uncategorized is answered by the engine once;
// the answer becomes a rule awaiting review, which classifies the next
// event of the pattern without the engine. Rejected, it keeps the pattern
// from the engine, and nothing is learned again.
func TestLearnsRulesFromTheEngine(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	e := newEnv(t)
	var st config.Settings
	e.admin("GET", "/api/settings", nil, &st)
	st.Engine.URL = fake.URL
	if code := e.admin("PUT", "/api/settings", st, nil); code != 200 {
		t.Fatalf("settings: %d", code)
	}
	var sources []config.Source
	e.admin("GET", "/api/sources", nil, &sources)
	src := sources[0]

	e.ingest(src, `{"level":"error","message":"shipment 4411 stuck in audit queue for 12 minutes"}`)
	if ev := e.latest(1); ev.Category != "audit" || ev.Classifier != "laya" {
		t.Fatalf("first event: category %q by %q", ev.Category, ev.Classifier)
	}
	var learned config.Rule
	waitFor(t, "a learned rule", func() bool {
		var rules []config.Rule
		e.admin("GET", "/api/rules", nil, &rules)
		i := slices.IndexFunc(rules, func(r config.Rule) bool { return r.Status == config.RuleReview })
		if i >= 0 {
			learned = rules[i]
		}
		return i >= 0
	})
	want := config.Condition{Field: "pattern", Op: config.OpEquals, Value: "shipment <n> stuck in audit queue for <n> minutes"}
	if len(learned.Conditions) != 1 || learned.Conditions[0] != want || learned.Category != "audit" ||
		!slices.Equal(learned.SourceIDs, []string{src.ID}) || learned.Learned == nil || learned.Learned.Confidence != 0.9 {
		t.Fatalf("learned rule: %+v %+v", learned, learned.Learned)
	}
	var stats struct {
		Learning learn.Stats `json:"learning"`
	}
	// The rule is listed as soon as it is saved, a moment before the
	// learner hears of it.
	waitFor(t, "learning stats", func() bool {
		e.admin("GET", "/api/stats", nil, &stats)
		return stats.Learning.Review == 1 && stats.Learning.Learned == 1
	})

	e.ingest(src, `{"level":"error","message":"shipment 9 stuck in audit queue for 3 minutes"}`)
	ev := e.latest(2)
	if ev.Category != "audit" || ev.Classifier != "rules" || ev.Engine != nil || !slices.Contains(ev.MatchedRules, learned.Name) {
		t.Errorf("second event: %q by %q, rules %v, engine %+v", ev.Category, ev.Classifier, ev.MatchedRules, ev.Engine)
	}
	if n := fake.Calls.Load(); n != 1 {
		t.Errorf("engine asked %d times", n)
	}

	if code := e.admin("POST", "/api/rules/review", map[string]any{"action": "reject", "ids": []string{learned.ID}}, nil); code != 200 {
		t.Fatalf("reject: %d", code)
	}
	e.ingest(src, `{"level":"error","message":"shipment 77 stuck in audit queue for 40 minutes"}`)
	ev = e.latest(3)
	if ev.Category != model.Uncategorized || ev.Engine != nil {
		t.Errorf("after rejecting: %q, engine %+v", ev.Category, ev.Engine)
	}
	var rules []config.Rule
	e.admin("GET", "/api/rules", nil, &rules)
	i := slices.IndexFunc(rules, func(r config.Rule) bool { return r.ID == learned.ID })
	if fake.Calls.Load() != 1 || i < 0 || rules[i].Status != config.RuleRejected || rules[i].Category != "" ||
		slices.ContainsFunc(rules, func(r config.Rule) bool { return r.Status == config.RuleReview }) {
		t.Errorf("after rejecting: %d engine calls, rules %+v", fake.Calls.Load(), rules)
	}

	// Approving makes it an ordinary rule, with what was learned.
	if code := e.admin("POST", "/api/rules/review", map[string]any{"action": "approve", "ids": []string{learned.ID}}, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	rules = nil
	e.admin("GET", "/api/rules", nil, &rules)
	if r := rules[slices.IndexFunc(rules, func(r config.Rule) bool { return r.ID == learned.ID })]; r.Status != config.RuleActive || r.Category != "audit" || r.SkipEngine || r.Learned == nil {
		t.Errorf("approved: %+v", r)
	}
	if code := e.admin("POST", "/api/rules/review", map[string]any{"action": "maybe", "ids": []string{learned.ID}}, nil); code != 400 {
		t.Errorf("unknown action: %d", code)
	}
}

// With learning off, the engine still classifies but nothing is learned.
func TestNoLearningWhenOff(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	e := newEnv(t)
	var st config.Settings
	e.admin("GET", "/api/settings", nil, &st)
	if !st.Engine.LearnRules || st.Engine.LearnMinConfidence != config.DefaultLearnMinConfidence {
		t.Fatalf("defaults: %+v", st.Engine)
	}
	st.Engine.URL, st.Engine.LearnRules = fake.URL, false
	e.admin("PUT", "/api/settings", st, nil)
	var sources []config.Source
	e.admin("GET", "/api/sources", nil, &sources)
	e.ingest(sources[0], `{"level":"error","message":"shipment 4411 stuck in audit queue"}`)
	if ev := e.latest(1); ev.Category != "audit" {
		t.Fatalf("event: %q", ev.Category)
	}
	time.Sleep(1500 * time.Millisecond) // longer than a learning batch
	var rules []config.Rule
	e.admin("GET", "/api/rules", nil, &rules)
	if slices.ContainsFunc(rules, func(r config.Rule) bool { return r.Learned != nil }) {
		t.Errorf("learned with learning off")
	}
}

// Alerts from a source go to the alert groups assigned to it, and only
// from those sources; the assignment can be made from either side.
func TestAlertGroupsRouteBySource(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	got := map[string]int{}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Event model.Event `json:"event"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got[r.URL.Path+" "+m.Event.SourceName]++
		mu.Unlock()
	}))
	defer hook.Close()

	var payTeam, opsTeam config.Notifier
	e.admin("POST", "/api/notifiers", map[string]any{"name": "payments chat", "type": "webhook", "enabled": true, "settings": map[string]string{"url": hook.URL + "/payments"}}, &payTeam)
	e.admin("POST", "/api/notifiers", map[string]any{"name": "ops chat", "type": "webhook", "enabled": true, "settings": map[string]string{"url": hook.URL + "/ops"}}, &opsTeam)
	var payments, checkout config.Source
	e.admin("POST", "/api/sources", map[string]any{"name": "payments", "type": "push", "enabled": true}, &payments)
	e.admin("POST", "/api/sources", map[string]any{"name": "checkout", "type": "push", "enabled": true}, &checkout)

	var g struct {
		config.AlertGroup
		SourceIDs []string `json:"source_ids"`
	}
	if code := e.admin("POST", "/api/alert-groups", map[string]any{"name": "Payments team", "enabled": true, "min_severity": "error",
		"notifier_ids": []string{payTeam.ID}, "source_ids": []string{payments.ID}}, &g); code != 201 || !slices.Equal(g.SourceIDs, []string{payments.ID}) {
		t.Fatalf("create group: %d %+v", code, g)
	}
	var ops config.AlertGroup
	e.admin("POST", "/api/alert-groups", map[string]any{"name": "Ops", "enabled": true, "min_severity": "critical", "notifier_ids": []string{opsTeam.ID}}, &ops)
	// Assigned from the source's side.
	checkout.AlertGroupIDs = []string{ops.ID, ops.ID, "gone"}
	var saved config.Source
	if code := e.admin("PUT", "/api/sources/"+checkout.ID, checkout, &saved); code != 200 || !slices.Equal(saved.AlertGroupIDs, []string{ops.ID}) {
		t.Fatalf("assign from source: %d %v", code, saved.AlertGroupIDs)
	}

	var sources []config.Source
	e.admin("GET", "/api/sources", nil, &sources)
	tok := map[string]config.Source{}
	for _, s := range sources {
		tok[s.ID] = s
	}
	e.ingest(tok[payments.ID], `{"level":"error","message":"card declined"}`)
	e.ingest(tok[checkout.ID], `{"level":"error","message":"cart lost"}`)    // below Ops' minimum
	e.ingest(tok[checkout.ID], `{"level":"critical","message":"cart gone"}`) // to Ops
	e.ingest(sources[0], `{"level":"critical","message":"default is unassigned"}`)
	e.latest(4)
	waitFor(t, "alerts", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got["/payments payments"] == 1 && got["/ops checkout"] == 1
	})
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if len(got) != 2 {
		t.Errorf("alerts: %v", got)
	}
	mu.Unlock()

	// Members need an email notifier to be emailed through.
	if code := e.admin("PUT", "/api/alert-groups/"+g.ID, map[string]any{"name": "Payments team", "enabled": true, "emails": []string{"a@example.com"}}, nil); code != 400 {
		t.Errorf("emails without an email notifier: %d", code)
	}
	if code := e.admin("PUT", "/api/alert-groups/"+g.ID, map[string]any{"name": "Payments team", "enabled": true, "emails": []string{"a@example.com"}, "email_notifier_id": payTeam.ID}, nil); code != 400 {
		t.Errorf("a webhook as the email notifier: %d", code)
	}

	if code := e.admin("DELETE", "/api/alert-groups/"+g.ID, nil, nil); code != 204 {
		t.Fatalf("delete group: %d", code)
	}
	sources = nil // decoding into the old elements would keep omitted fields
	e.admin("GET", "/api/sources", nil, &sources)
	for _, s := range sources {
		if s.ID == payments.ID && len(s.AlertGroupIDs) != 0 {
			t.Errorf("deleted group still assigned: %v", s.AlertGroupIDs)
		}
	}
}
