package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine"
	"github.com/vipinmohan22/lissandra/internal/engine/enginetest"
	"github.com/vipinmohan22/lissandra/internal/events"
	"github.com/vipinmohan22/lissandra/internal/importer"
	"github.com/vipinmohan22/lissandra/internal/learn"
	"github.com/vipinmohan22/lissandra/internal/model"
	"github.com/vipinmohan22/lissandra/internal/notify"
	"github.com/vipinmohan22/lissandra/internal/pipeline"
	"github.com/vipinmohan22/lissandra/internal/poller"
)

const adminToken = "test-admin-token-0123456789"

type env struct {
	t       *testing.T
	srv     *httptest.Server
	cfg     *config.Store
	imports *importer.Registry
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

func newGzip(w io.Writer) *gzip.Writer { return gzip.NewWriter(w) }

func newEnvWith(t *testing.T, mutate func(*Options)) *env {
	t.Helper()
	return newEnvFrom(t, envConfig{mutate: mutate})
}

// envConfig sets up an env beyond Options.
type envConfig struct {
	configPath string       // default: a new file
	log        *slog.Logger // default: discarded
	mutate     func(*Options)
}

func newEnvFrom(t *testing.T, ec envConfig) *env {
	t.Helper()
	log := ec.log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if ec.configPath == "" {
		ec.configPath = filepath.Join(t.TempDir(), "config.json")
	}
	cfg, err := config.Open(ec.configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := events.NewStore(1000)
	d := notify.NewDispatcher(log)
	a := engine.New(log)
	t.Cleanup(a.Close)
	p := pipeline.New(100, store, d, a)
	l := learn.New(log, cfg.Update)
	p.Learn = l.Observe
	pm := poller.NewManager(ctx, log, p.SubmitWait)
	cfg.Subscribe(func(c *config.Config) {
		p.Configure(c)
		d.Configure(c)
		a.Configure(c)
		l.Configure(c)
		pm.Configure(c)
	})
	go d.Run(ctx, 1)
	go l.Run(ctx)
	go p.Run(ctx, 1, 1)
	ui := fstest.MapFS{"index.html": {Data: []byte("<html>lissandra</html>")}}
	opts := Options{Version: "test", AdminToken: adminToken, UI: ui}
	if ec.mutate != nil {
		ec.mutate(&opts)
	}
	reg := importer.NewRegistry(importer.RegistryConfig{Keep: 3, Limits: importer.Limits{Events: 1000, Bytes: 8 << 20}})
	s := New(opts, log, cfg, store, p, d, a, l, pm, reg)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, cfg: cfg, imports: reg}
}

func (e *env) do(method, path string, body any, headers map[string]string) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	case []byte:
		r = bytes.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (e *env) admin(method, path string, body any, out any) int {
	e.t.Helper()
	code, data := e.do(method, path, body, map[string]string{"Authorization": "Bearer " + adminToken, "Content-Type": "application/json"})
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return code
}

func TestAuthAndUI(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.do("GET", "/api/info", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := e.do("GET", "/api/info", nil, map[string]string{"Authorization": "Bearer wrong"}); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code := e.admin("GET", "/api/info", nil, nil); code != http.StatusOK {
		t.Errorf("admin: %d", code)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Errorf("ui: %d csp=%q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	if code, _ := e.do("GET", "/healthz", nil, nil); code != 200 {
		t.Errorf("healthz: %d", code)
	}
	if code, body := e.do("GET", "/metrics", nil, nil); code != 200 || !bytes.Contains(body, []byte("lissandra_events_received_total")) {
		t.Errorf("metrics: %d", code)
	}
}

func TestIngestClassifyAndNotify(t *testing.T) {
	e := newEnv(t)

	var mu sync.Mutex
	var hooks []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		hooks = append(hooks, m)
		mu.Unlock()
	}))
	defer hook.Close()

	var n config.Notifier
	if code := e.admin("POST", "/api/notifiers", map[string]any{"name": "hook", "type": "webhook", "enabled": true, "settings": map[string]string{"url": hook.URL}}, &n); code != 201 {
		t.Fatalf("create notifier: %d", code)
	}
	if code := e.admin("POST", "/api/routes", map[string]any{"name": "crit", "enabled": true, "min_severity": "critical", "notifier_ids": []string{n.ID}}, nil); code != 201 {
		t.Fatalf("create route: %d", code)
	}

	var sources []config.Source
	e.admin("GET", "/api/sources", nil, &sources)
	src := sources[0]

	if code, _ := e.do("POST", "/ingest/"+src.ID, `{"message":"x"}`, map[string]string{"Authorization": "Bearer nope"}); code != 401 {
		t.Errorf("bad ingest token: %d", code)
	}
	if code, _ := e.do("POST", "/ingest/missing", `{"message":"x"}`, nil); code != 404 {
		t.Errorf("unknown source: %d", code)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("panic: runtime error: nil map\nall fine here\n"))
	gz.Close()
	code, body := e.do("POST", "/ingest/default", buf.Bytes(), map[string]string{"X-Lissandra-Token": src.IngestToken, "Content-Encoding": "gzip", "Content-Type": "text/plain"})
	if code != 202 || !bytes.Contains(body, []byte(`"accepted":2`)) {
		t.Fatalf("ingest: %d %s", code, body)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(hooks)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hooks) != 1 {
		t.Fatalf("webhook calls = %d", len(hooks))
	}
	ev := hooks[0]["event"].(map[string]any)
	if ev["severity"] != "critical" || ev["category"] != "application" {
		t.Errorf("notified event = %v", ev)
	}

	var list []map[string]any
	e.admin("GET", "/api/events?min_severity=critical", nil, &list)
	if len(list) != 1 {
		t.Errorf("events filtered by severity = %d", len(list))
	}
}

func TestSecretsAreMaskedAndPreserved(t *testing.T) {
	e := newEnv(t)
	var n config.Notifier
	e.admin("POST", "/api/notifiers", map[string]any{"name": "s", "type": "slack", "enabled": true, "settings": map[string]string{"webhook_url": "https://hooks.slack.com/services/SECRET"}}, &n)
	if n.Settings["webhook_url"] != config.Mask {
		t.Fatalf("create response leaks secret: %v", n.Settings)
	}
	_, raw := e.do("GET", "/api/notifiers", nil, map[string]string{"Authorization": "Bearer " + adminToken})
	if bytes.Contains(raw, []byte("SECRET")) {
		t.Error("list leaks secret")
	}
	// Update with the mask sent back keeps the stored secret.
	n.Name = "renamed"
	if code := e.admin("PUT", "/api/notifiers/"+n.ID, n, nil); code != 200 {
		t.Fatalf("update: %d", code)
	}
	stored := e.cfg.Get().Notifiers[0]
	if stored.Name != "renamed" || stored.Settings["webhook_url"] != "https://hooks.slack.com/services/SECRET" {
		t.Errorf("stored = %+v", stored)
	}
	_, exported := e.do("GET", "/api/config/export", nil, map[string]string{"Authorization": "Bearer " + adminToken})
	if bytes.Contains(exported, []byte("SECRET")) {
		t.Error("export without include_secrets leaks secret")
	}

	var st config.Settings
	e.admin("PUT", "/api/settings", map[string]any{"categories": []string{"x"}, "engine": map[string]any{"api_key": "sk-live-123"}}, &st)
	if st.Engine.APIKey != config.Mask {
		t.Errorf("settings response leaks key: %q", st.Engine.APIKey)
	}
	e.admin("PUT", "/api/settings", map[string]any{"categories": []map[string]string{{"name": "x", "description": "x things"}, {"name": "y"}}, "engine": map[string]any{"api_key": config.Mask}}, nil)
	got := e.cfg.Get().Settings
	if got.Engine.APIKey != "sk-live-123" {
		t.Errorf("masked key not preserved: %q", got.Engine.APIKey)
	}
	if len(got.Categories) != 2 || got.Categories[0] != (config.Category{Name: "x", Description: "x things"}) {
		t.Errorf("categories: %+v", got.Categories)
	}
}

func TestRulesCRUDAndReorder(t *testing.T) {
	e := newEnv(t)
	var rules []config.Rule
	e.admin("GET", "/api/rules", nil, &rules)
	n := len(rules)

	if code := e.admin("POST", "/api/rules", map[string]any{"name": "bad", "conditions": []map[string]string{{"field": "message", "op": "regex", "value": "("}}}, nil); code != 400 {
		t.Errorf("invalid regex accepted: %d", code)
	}
	var r config.Rule
	if code := e.admin("POST", "/api/rules", map[string]any{"name": "mine", "enabled": true, "category": "custom", "conditions": []map[string]string{{"field": "message", "op": "contains", "value": "zebra"}}}, &r); code != 201 {
		t.Fatalf("create: %d", code)
	}
	ids := []string{r.ID}
	for _, x := range rules {
		ids = append(ids, x.ID)
	}
	if code := e.admin("POST", "/api/rules/reorder", map[string]any{"ids": ids}, &rules); code != 200 || rules[0].ID != r.ID || len(rules) != n+1 {
		t.Fatalf("reorder: %d first=%s", code, rules[0].Name)
	}
	if code := e.admin("POST", "/api/rules/reorder", map[string]any{"ids": ids[:1]}, nil); code != 400 {
		t.Errorf("partial reorder accepted: %d", code)
	}

	var res struct {
		Events []struct {
			Event struct{ Category string } `json:"event"`
		} `json:"events"`
	}
	e.admin("POST", "/api/rules/test", map[string]any{"sample": "a zebra appeared"}, &res)
	if len(res.Events) != 1 || res.Events[0].Event.Category != "custom" {
		t.Errorf("rule test = %+v", res)
	}
	if code := e.admin("DELETE", "/api/rules/"+r.ID, nil, nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code := e.admin("DELETE", "/api/rules/"+r.ID, nil, nil); code != 404 {
		t.Errorf("delete missing: %d", code)
	}
}

func TestSourceLifecycle(t *testing.T) {
	e := newEnv(t)
	var s config.Source
	code := e.admin("POST", "/api/sources", map[string]any{"name": "app", "type": "push", "enabled": true, "ingest_token": "attacker-chosen"}, &s)
	if code != 201 || s.IngestToken == "" || s.IngestToken == "attacker-chosen" {
		t.Fatalf("create: %d token=%q", code, s.IngestToken)
	}
	old := s.IngestToken
	e.admin("POST", "/api/sources/"+s.ID+"/rotate-token", nil, &s)
	if s.IngestToken == old {
		t.Error("token not rotated")
	}
	if code, _ := e.do("POST", "/ingest/"+s.ID, "hello", map[string]string{"Authorization": "Bearer " + old}); code != 401 {
		t.Errorf("old token still accepted: %d", code)
	}
	if code, _ := e.do("POST", "/ingest/"+s.ID, "hello", map[string]string{"Authorization": "Bearer " + s.IngestToken}); code != 202 {
		t.Errorf("new token rejected: %d", code)
	}
	if code := e.admin("POST", "/api/sources", map[string]any{"name": "pull", "type": "pull", "pull": map[string]any{"url": "not a url"}}, nil); code != 400 {
		t.Errorf("invalid pull accepted: %d", code)
	}
	if code := e.admin("DELETE", "/api/sources/"+s.ID, nil, nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
}

func TestEngineEndpoints(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	e := newEnv(t)
	var st config.Settings
	e.admin("GET", "/api/settings", nil, &st)
	st.Engine.URL = fake.URL
	if code := e.admin("PUT", "/api/settings", st, nil); code != 200 {
		t.Fatalf("settings: %d", code)
	}
	var health struct {
		OK        bool                    `json:"ok"`
		Model     string                  `json:"model"`
		Endpoints []engine.EndpointHealth `json:"endpoints"`
	}
	e.admin("GET", "/api/engine/health", nil, &health)
	if !health.OK || health.Model != "english" || len(health.Endpoints) != 1 || !health.Endpoints[0].ModelLoaded {
		t.Errorf("health %+v", health)
	}
	var res struct {
		OK     bool          `json:"ok"`
		Error  string        `json:"error"`
		Result engine.Result `json:"result"`
	}
	e.admin("POST", "/api/engine/classify", map[string]string{"message": "ERROR the database replica is lagging"}, &res)
	if !res.OK || res.Result.Category != "database" || res.Result.Model != "english" {
		t.Errorf("classify %+v", res)
	}
	if code := e.admin("POST", "/api/engine/classify", map[string]string{}, nil); code != 400 {
		t.Errorf("empty request: %d", code)
	}
	var stats struct {
		Engine engine.Status `json:"engine"`
	}
	// The engine's checkpoints are learned by a background health check.
	for deadline := time.Now().Add(5 * time.Second); len(stats.Engine.Loaded) == 0 && time.Now().Before(deadline); {
		e.admin("GET", "/api/stats", nil, &stats)
		time.Sleep(20 * time.Millisecond)
	}
	if !stats.Engine.Configured || stats.Engine.Calls != 0 || strings.Join(stats.Engine.Loaded, ",") != "english,multilingual" {
		t.Errorf("test calls must not count as live calls: %+v", stats.Engine)
	}
	_, metrics := e.do("GET", "/metrics", nil, map[string]string{"Authorization": "Bearer " + adminToken})
	if !bytes.Contains(metrics, []byte("lissandra_engine_calls_total 0")) || !bytes.Contains(metrics, []byte("lissandra_engine_paused 0")) {
		t.Errorf("metrics missing engine counters")
	}
}

func TestEngineTrainingDataAndRulesView(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	e := newEnv(t)
	var st config.Settings
	e.admin("GET", "/api/settings", nil, &st)
	st.Engine.URL = "http://user:s3cret@" + strings.TrimPrefix(fake.URL, "http://")
	e.admin("PUT", "/api/settings", st, nil)
	e.admin("GET", "/api/settings", nil, &st)
	if strings.Contains(st.Engine.URL, "s3cret") {
		t.Errorf("engine URL password not masked: %q", st.Engine.URL)
	}
	e.admin("PUT", "/api/settings", st, nil) // masked URL sent back
	if got := e.cfg.Get().Settings.Engine.URL; !strings.Contains(got, "s3cret") {
		t.Errorf("masked URL not restored: %q", got)
	}

	var sources []config.Source
	e.admin("GET", "/api/sources", nil, &sources)
	push := func(line string) {
		req, _ := http.NewRequest("POST", e.srv.URL+"/ingest/"+sources[0].Name, strings.NewReader(line))
		req.Header.Set("Authorization", "Bearer "+sources[0].IngestToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	push("ERROR java.lang.OutOfMemoryError: Java heap space\n") // rule: resource
	push("ERROR java.lang.OutOfMemoryError: Java heap space\n") // same pattern
	push("ERROR the replica of the database is lagging\n")      // engine: database
	deadline := time.Now().Add(5 * time.Second)
	var evs []model.Event
	for len(evs) < 3 && time.Now().Before(deadline) {
		e.admin("GET", "/api/events", nil, &evs)
		time.Sleep(20 * time.Millisecond)
	}
	var byEngine model.Event
	for _, ev := range evs {
		if ev.Classifier == engine.ByEngine {
			byEngine = ev
		}
	}
	if byEngine.Category != "database" {
		t.Fatalf("engine did not classify: %+v", evs)
	}
	// The rules' view of an engine-classified event is "uncategorized".
	var res struct {
		OK    bool `json:"ok"`
		Rules struct {
			Category string `json:"category"`
		} `json:"rules"`
	}
	e.admin("POST", "/api/engine/classify", map[string]string{"event_id": byEngine.ID}, &res)
	if !res.OK || res.Rules.Category != model.Uncategorized {
		t.Errorf("rules view: %+v", res)
	}

	code, body := e.do("GET", "/api/engine/training-data", nil, authHeaders(nil))
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if code != 200 || len(lines) != 1 {
		t.Fatalf("training data %d: %q", code, body)
	}
	var rec engine.TrainingRecord
	json.Unmarshal([]byte(lines[0]), &rec)
	if rec.Expected["category"] != "resource" || !strings.Contains(rec.State, "OutOfMemoryError") || rec.Questions["category"].Type != "choice" {
		t.Errorf("record %+v", rec)
	}
}
