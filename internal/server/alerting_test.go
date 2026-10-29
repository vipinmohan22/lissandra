package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
	"github.com/vipinmohan22/lissandra/internal/notify"
)

// hookPayload is the generic webhook's standard payload, as received.
type hookPayload struct {
	ID     string        `json:"id"`
	Kind   string        `json:"kind"`
	Reason notify.Reason `json:"reason"`
	Event  model.Event   `json:"event"`
	Test   bool          `json:"test"`
	Digest *notify.Digest
}

// receiver is a webhook endpoint that keeps what it is sent.
type receiver struct {
	*httptest.Server
	mu  sync.Mutex
	got []hookPayload
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p hookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("webhook body: %v", err)
		}
		rc.mu.Lock()
		rc.got = append(rc.got, p)
		rc.mu.Unlock()
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) payloads() []hookPayload {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.got)
}

// A smart route sends the critical and the first new error right away and
// holds the other 19 new errors from the same source and category into one
// summary, which "Send now" delivers; every step is visible in the API.
func TestSmartAlertingEndToEnd(t *testing.T) {
	e := newEnvFrom(t, envConfig{notify: notify.Options{Warmup: -1}})
	rc := newReceiver(t)

	var hook config.Notifier
	if code := e.admin("POST", "/api/notifiers", map[string]any{"name": "dev-hook", "type": "webhook", "enabled": true, "settings": map[string]string{"url": rc.URL}}, &hook); code != 201 {
		t.Fatalf("create notifier: %d", code)
	}
	var rt config.Route
	if code := e.admin("POST", "/api/routes", map[string]any{"name": "dev: error and above", "enabled": true, "min_severity": "error", "notifier_ids": []string{hook.ID},
		"throttle_seconds": 3600, "delivery": "smart", "digest_minutes": 30}, &rt); code != 201 {
		t.Fatalf("create route: %d", code)
	}
	if rt.Delivery != config.DeliverySmart || rt.ImmediateSeverity != model.SeverityCritical || rt.DigestMinutes != 30 || rt.ThrottleSeconds != 3600 {
		t.Errorf("route as created: %+v", rt.DeliveryPolicy)
	}
	// The policy round-trips: a PUT of what GET returned changes nothing.
	var routes []config.Route
	e.admin("GET", "/api/routes", nil, &routes)
	var again config.Route
	if code := e.admin("PUT", "/api/routes/"+rt.ID, routes[0], &again); code != 200 || again.DeliveryPolicy != rt.DeliveryPolicy {
		t.Errorf("route round trip: %d %+v", code, again.DeliveryPolicy)
	}

	var sources []config.Source
	e.admin("GET", "/api/sources", nil, &sources)
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliett",
		"kilo", "lima", "mike", "november", "oscar", "papa", "quebec", "romeo", "sierra", "tango"}
	var body strings.Builder
	for _, w := range words {
		fmt.Fprintf(&body, `{"level":"error","message":"postgres query failed for tenant %s"}`+"\n", w)
	}
	body.WriteString(`{"level":"critical","message":"postgres primary lost, failing over"}` + "\n")
	e.ingest(sources[0], body.String())
	e.latest(21)

	waitFor(t, "two right-away alerts", func() bool { return len(rc.payloads()) >= 2 })
	time.Sleep(300 * time.Millisecond)
	got := rc.payloads()
	if len(got) != 2 {
		t.Fatalf("right away: %d messages, want 2", len(got))
	}
	reasons := []string{got[0].Reason.Code, got[1].Reason.Code}
	slices.Sort(reasons)
	if !slices.Equal(reasons, []string{"new", "severity"}) || got[0].Kind != "alert" || got[0].Reason.Text == "" {
		t.Errorf("right-away alerts: %+v", got)
	}
	sent := map[string]bool{got[0].Event.ID: true, got[1].Event.ID: true}

	var al notify.Alerting
	e.admin("GET", "/api/alerting", nil, &al)
	if len(al.Pending) != 1 || al.Pending[0].NotifierID != hook.ID || al.Pending[0].Events != 19 || al.Pending[0].Held != 19 || !al.Pending[0].Summary {
		t.Fatalf("pending digests: %+v", al.Pending)
	}
	if al.Pending[0].NotifierName != "dev-hook" || len(al.Pending[0].Top) != 5 || al.Limits.ChannelAlerts != 10 || al.State.Saved || len(al.WarmingUp) != 0 {
		t.Errorf("alerting status: %+v", al)
	}

	// Why each event did what it did.
	var evs []model.Event
	e.admin("GET", "/api/events?limit=50", nil, &evs)
	var heldID string
	for _, ev := range evs {
		if !sent[ev.ID] {
			heldID = ev.ID
			break
		}
	}
	var detail struct {
		Routes []string          `json:"routes"`
		Alerts []notify.Decision `json:"alerts"`
	}
	e.admin("GET", "/api/events/"+heldID, nil, &detail)
	if len(detail.Alerts) != 1 || detail.Alerts[0].Action != "held" || detail.Alerts[0].Reason.Code != "source_limit" ||
		detail.Alerts[0].NotifierName != "dev-hook" || detail.Alerts[0].Due.IsZero() || !slices.Equal(detail.Routes, []string{"dev: error and above"}) {
		t.Errorf("held event's decisions: %+v", detail)
	}

	var st struct {
		Notifications struct {
			Sent      int            `json:"sent"`
			Held      int            `json:"held"`
			RightAway map[string]int `json:"right_away"`
			Pending   int            `json:"pending_digest_events"`
			Tracked   int            `json:"patterns_tracked"`
		} `json:"notifications"`
	}
	e.admin("GET", "/api/stats", nil, &st)
	if n := st.Notifications; n.Held != 19 || n.RightAway["new"] != 1 || n.RightAway["severity"] != 1 || n.Pending != 19 || n.Tracked != 21 {
		t.Errorf("stats: %+v", n)
	}
	_, metrics := e.do("GET", "/metrics", nil, nil)
	for _, want := range []string{`lissandra_alert_decisions_total{action="held"} 19`, `lissandra_alerts_right_away_total{reason="new"} 1`,
		`lissandra_digest_pending_events 19`, `lissandra_alert_patterns_tracked 21`, `lissandra_alert_sources_warming_up 0`} {
		if !bytes.Contains(metrics, []byte(want)) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// Send now.
	var flushed struct {
		ID string `json:"id"`
	}
	if code := e.admin("POST", "/api/alerting/digests/"+hook.ID+"/send", nil, &flushed); code != 202 || flushed.ID == "" {
		t.Fatalf("send now: %d %+v", code, flushed)
	}
	waitFor(t, "the digest", func() bool { return len(rc.payloads()) >= 3 })
	dg := rc.payloads()[2]
	if dg.Kind != "digest" || dg.ID != flushed.ID || dg.Reason.Code != "manual" || dg.Digest == nil || dg.Digest.Patterns != 19 || dg.Digest.Held != 19 {
		t.Fatalf("digest payload: %+v", dg)
	}
	if dg.Event.Severity != model.SeverityError || dg.Event.Category != "digest" {
		t.Errorf("a digest's summary event: %+v", dg.Event)
	}
	if code := e.admin("POST", "/api/alerting/digests/"+hook.ID+"/send", nil, nil); code != 404 {
		t.Errorf("send now with nothing batched: %d", code)
	}

	var del struct {
		Delivery notify.Delivery `json:"delivery"`
		Digest   *notify.Digest  `json:"digest"`
	}
	waitFor(t, "the digest's delivery record", func() bool {
		e.admin("GET", "/api/deliveries/"+flushed.ID, nil, &del)
		return del.Delivery.Status == "sent"
	})
	if del.Digest == nil || len(del.Digest.Entries) != 19 || del.Delivery.Kind != "digest" || del.Delivery.Events != 19 || del.Delivery.Reason != "manual" {
		t.Errorf("digest detail: %+v", del)
	}
	var list []notify.Delivery
	e.admin("GET", "/api/deliveries?kind=digest", nil, &list)
	if len(list) != 1 || list[0].ID != flushed.ID {
		t.Errorf("digest deliveries: %+v", list)
	}
	list = nil
	e.admin("GET", "/api/deliveries?kind=alert", nil, &list)
	if len(list) != 2 || list[0].Kind != "alert" || list[0].ReasonText == "" || list[0].Fingerprint == "" {
		t.Errorf("alert deliveries: %+v", list)
	}
	if code := e.admin("GET", "/api/deliveries?kind=page", nil, nil); code != 400 {
		t.Errorf("unknown kind: %d", code)
	}
	if code, body := e.do("GET", "/api/deliveries/nope", nil, map[string]string{"Authorization": "Bearer " + adminToken}); code != 404 || !bytes.Contains(body, []byte("only the last 500")) {
		t.Errorf("unknown delivery: %d %s", code, body)
	}

	// Test digests: a webhook takes one; PagerDuty never does.
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if code := e.admin("POST", "/api/notifiers/test?kind=digest", hook, &res); code != 200 || !res.OK {
		t.Errorf("test digest: %d %+v", code, res)
	}
	waitFor(t, "the test digest", func() bool { return len(rc.payloads()) >= 4 })
	if p := rc.payloads()[3]; p.Kind != "digest" || !p.Test || p.Reason.Code != "test" {
		t.Errorf("test digest payload: %+v", p)
	}
	pd := map[string]any{"name": "pager", "type": "pagerduty", "settings": map[string]string{"routing_key": "R0123456789"}}
	if code, body := e.do("POST", "/api/notifiers/test?kind=digest", mustJSON(pd), map[string]string{"Authorization": "Bearer " + adminToken}); code != 400 || !bytes.Contains(body, []byte("PagerDuty never receives digests")) {
		t.Errorf("PagerDuty test digest: %d %s", code, body)
	}
	if code := e.admin("POST", "/api/notifiers/test?kind=fax", hook, nil); code != 400 {
		t.Errorf("unknown test kind: %d", code)
	}

	// The rule tester and its preview: a critical goes right away.
	var tested struct {
		Events []previewEvent `json:"events"`
	}
	e.admin("POST", "/api/rules/test", map[string]any{"sample": `{"level":"critical","message":"disk array failed"}` + "\n" + `{"level":"error","message":"one more thing broke"}` + "\n" + `{"level":"info","message":"fine"}`}, &tested)
	if len(tested.Events) != 3 {
		t.Fatalf("rule test: %+v", tested)
	}
	if a := tested.Events[0].Alerts; len(a) != 1 || a[0].Action != "right_away" || a[0].Reason.Code != "severity" {
		t.Errorf("preview of a critical: %+v", a)
	}
	if a := tested.Events[1].Alerts; len(a) != 1 || a[0].Action != "digest" || !strings.Contains(a[0].Reason.Text, "unless it is new") {
		t.Errorf("preview of an error: %+v", a)
	}
	if a := tested.Events[2].Alerts; a == nil || len(a) != 0 {
		t.Errorf("preview of an unrouted event: %+v", a)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// An event no route matched has no decisions: "alerts" is [] (not null),
// and notifier types say which never get digests.
func TestEventAlertsAndNotifierTypes(t *testing.T) {
	e := newEnv(t)
	var sources []config.Source
	e.admin("GET", "/api/sources", nil, &sources)
	e.ingest(sources[0], `{"level":"error","message":"nobody listens"}`)
	ev := e.latest(1)
	if e.dispatcher.Decisions(ev.ID) != nil {
		t.Fatalf("decisions for an unrouted event: %+v", e.dispatcher.Decisions(ev.ID))
	}
	_, body := e.do("GET", "/api/events/"+ev.ID, nil, map[string]string{"Authorization": "Bearer " + adminToken})
	if !bytes.Contains(body, []byte(`"alerts":[]`)) || !bytes.Contains(body, []byte(`"routes":null`)) && !bytes.Contains(body, []byte(`"routes":[]`)) {
		t.Errorf("event without routes: %s", body)
	}

	var types []struct {
		Type     string `json:"type"`
		NoDigest bool   `json:"no_digest"`
	}
	e.admin("GET", "/api/notifier-types", nil, &types)
	for _, tp := range types {
		if tp.NoDigest != (tp.Type == "pagerduty") {
			t.Errorf("%s: no_digest %v", tp.Type, tp.NoDigest)
		}
	}
	_, metrics := e.do("GET", "/metrics", nil, nil)
	for _, name := range []string{"lissandra_alert_decisions_total", "lissandra_alerts_right_away_total", "lissandra_digests_total",
		"lissandra_digest_events_total", "lissandra_alert_lost_events_total", "lissandra_digest_pending_events",
		"lissandra_alert_patterns_tracked", "lissandra_alert_sources_warming_up", "lissandra_alert_state_save_errors_total"} {
		if !bytes.Contains(metrics, []byte("# TYPE "+name+" ")) {
			t.Errorf("metrics lack %s", name)
		}
	}
	var al notify.Alerting
	e.admin("GET", "/api/alerting", nil, &al)
	if al.Pending == nil || len(al.WarmingUp) != 1 || al.WarmingUp[0].SourceID != sources[0].ID || al.Limits.WarmupMinutes != 60 {
		t.Errorf("a fresh source warms up: %+v", al)
	}
}

// "Default rules" lists the defaults a config lacks or changed, and
// applies the ones chosen, keeping each rule's ID and enabled state.
func TestDefaultRulesEndpoints(t *testing.T) {
	e := newEnv(t)
	type changes struct {
		Missing      []string          `json:"missing"`
		Changed      []string          `json:"changed"`
		Descriptions map[string]string `json:"descriptions"`
	}
	var ch changes
	if code := e.admin("GET", "/api/rules/defaults", nil, &ch); code != 200 || ch.Missing == nil || ch.Changed == nil || len(ch.Missing)+len(ch.Changed) != 0 {
		t.Fatalf("a fresh config: %d %+v", code, ch)
	}

	var rules []config.Rule
	e.admin("GET", "/api/rules", nil, &rules)
	edited, removed := rules[2], rules[4]
	edited.Category, edited.Enabled = "mine", false
	if code := e.admin("PUT", "/api/rules/"+edited.ID, edited, nil); code != 200 {
		t.Fatalf("edit: %d", code)
	}
	if code := e.admin("DELETE", "/api/rules/"+removed.ID, nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	e.admin("GET", "/api/rules/defaults", nil, &ch)
	if !slices.Equal(ch.Changed, []string{edited.Name}) || !slices.Equal(ch.Missing, []string{removed.Name}) || ch.Descriptions[removed.Name] != removed.Description {
		t.Fatalf("changes: %+v", ch)
	}

	var res struct {
		Restored int           `json:"restored"`
		Rules    []config.Rule `json:"rules"`
	}
	if code := e.admin("POST", "/api/rules/defaults", map[string]any{"names": []string{edited.Name}}, &res); code != 200 || res.Restored != 1 {
		t.Fatalf("restore one: %d %+v", code, res.Restored)
	}
	i := slices.IndexFunc(res.Rules, func(r config.Rule) bool { return r.Name == edited.Name })
	if i < 0 || res.Rules[i].ID != edited.ID || res.Rules[i].Enabled || res.Rules[i].Category != rules[2].Category {
		t.Errorf("restored rule: %+v", res.Rules[i])
	}
	ch = changes{}
	e.admin("GET", "/api/rules/defaults", nil, &ch)
	if len(ch.Changed) != 0 || !slices.Equal(ch.Missing, []string{removed.Name}) {
		t.Errorf("after restoring one: %+v", ch)
	}

	// No body restores everything that differs, at its place.
	if code := e.admin("POST", "/api/rules/defaults", nil, &res); code != 200 || res.Restored != 1 || res.Rules[4].Name != removed.Name || res.Rules[4].ID == "" {
		t.Fatalf("restore all: %d %d %v", code, res.Restored, res.Rules[4].Name)
	}
	version := e.cfg.Get().Version
	if code := e.admin("POST", "/api/rules/defaults", map[string]any{}, &res); code != 200 || res.Restored != 0 || e.cfg.Get().Version != version {
		t.Errorf("nothing to restore: %d %d, version %d → %d", code, res.Restored, version, e.cfg.Get().Version)
	}
	if code := e.admin("POST", "/api/rules/defaults", `{"names":`, nil); code != 400 {
		t.Errorf("invalid JSON: %d", code)
	}
}
