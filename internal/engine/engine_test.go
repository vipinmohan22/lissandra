package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/engine/enginetest"
	"github.com/vipinmohan22/layalogs/internal/model"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// setup returns a classifier configured against a fake laya-serve.
func setup(t *testing.T, mutate func(*config.Config)) (*Classifier, *enginetest.Server, *config.Config) {
	t.Helper()
	fake := enginetest.New()
	t.Cleanup(fake.Close)
	c := config.Defaults()
	c.Settings.Engine.URL = fake.URL
	c.Settings.Engine.APIKey = ""
	if mutate != nil {
		mutate(c)
	}
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	cl := New(quiet())
	cl.Configure(c)
	return cl, fake, c
}

func event(msg string) model.Event {
	return model.Event{ID: model.NewID(), SourceID: "src", SourceName: "app", Message: msg, Severity: model.SeverityError,
		Category: model.Uncategorized, Fingerprint: "fp-" + msg}
}

func TestRequestContract(t *testing.T) {
	cl, fake, _ := setup(t, func(c *config.Config) { c.Settings.Engine.APIKey = "k3y" })
	fake.Key = "k3y"
	ev := event("HikariPool-1 - Connection is not available\n\tat com.zaxxer.Pool.get(Pool.java:1)\nCaused by: java.sql.SQLTransientConnectionException: database pool exhausted")
	ev.Level = "ERROR"
	cl.Enrich(context.Background(), &ev)
	req := fake.LastRequest()
	if req.Model != "english" {
		t.Errorf("model not pinned: %q", req.Model)
	}
	if got := fake.Headers.Get("Authorization"); got != "Bearer k3y" {
		t.Errorf("auth header %q", got)
	}
	labels := req.Order["category"]
	if len(labels) != len(config.DefaultCategories)+1 || labels[0] != "security" || labels[len(labels)-1] != model.Uncategorized {
		t.Errorf("category labels not in configured order: %v", labels)
	}
	if _, ok := req.Questions["severity"]; ok {
		t.Error("severity asked although the record has a level")
	}
	state, _ := req.State.(string)
	lines := strings.Split(state, "\n")
	if !strings.HasPrefix(lines[1], "Caused by: java.sql.SQLTransientConnectionException") || !strings.Contains(lines[2], "level: ERROR") {
		t.Errorf("root cause should come right after the first line:\n%s", state)
	}
	if ev.Category != "database" || ev.Classifier != ByEngine || ev.Engine == nil || !ev.Engine.Applied || ev.Engine.Model != "english" {
		t.Errorf("answer not applied: %+v %+v", ev, ev.Engine)
	}
}

func TestConfidenceThresholdAndSeverity(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		if q.ID == "severity" {
			return enginetest.Answer{Probs: map[string]float64{q.Labels[4]: 0.7, q.Labels[3]: 0.3}}
		}
		return enginetest.Answer{Choice: "network", Probs: map[string]float64{"network": 0.4, "database": 0.35, model.Uncategorized: 0.25}}
	}
	ev := event("upstream hiccup")
	ev.Severity = model.SeverityWarning
	cl.Enrich(context.Background(), &ev)
	if _, ok := fake.LastRequest().Questions["severity"]; !ok {
		t.Fatal("severity not asked for a record with no level or rule")
	}
	if ev.Category != model.Uncategorized || ev.Engine.Applied != true || ev.Severity != model.SeverityError {
		t.Errorf("low-confidence category must not apply, confident severity must: %+v %+v", ev, ev.Engine)
	}
	if ev.Engine.Category != "network" || len(ev.Engine.Top) != 3 {
		t.Errorf("engine answer not recorded: %+v", ev.Engine)
	}

	// A rule's category and the record's level always win.
	ruled := event("database pool exhausted")
	ruled.Category, ruled.Level, ruled.MatchedRules = "custom", "WARN", []string{"r"}
	ruled.Severity = model.SeverityWarning
	cl.Enrich(context.Background(), &ruled)
	if ruled.Category != "custom" || ruled.Severity != model.SeverityWarning || ruled.Classifier == ByEngine {
		t.Errorf("rules overridden: %+v", ruled)
	}
}

func TestEligibility(t *testing.T) {
	cl, _, _ := setup(t, nil)
	ev := event("x")
	if !cl.Eligible(&ev) {
		t.Error("uncategorized error should be eligible")
	}
	info := event("x")
	info.Severity = model.SeverityInfo
	ruled := event("x")
	ruled.Category = "database"
	if cl.Eligible(&info) || cl.Eligible(&ruled) {
		t.Error("below min severity or already categorized: not eligible")
	}
	off, _, _ := setup(t, func(c *config.Config) { c.Settings.Engine.Enabled = false })
	none, _, _ := setup(t, func(c *config.Config) { c.Settings.Engine.URL = "" })
	if off.Eligible(&ev) || none.Eligible(&ev) {
		t.Error("disabled or unconfigured engine: nothing is eligible")
	}
	t.Setenv("LAYA_TEST_ENGINE_URL", "")
	envOnly, _, _ := setup(t, func(c *config.Config) { c.Settings.Engine.URL = "${env:LAYA_TEST_ENGINE_URL}" })
	if envOnly.Eligible(&ev) || envOnly.Status().Configured {
		t.Error("empty env URL means not configured")
	}
}

func TestCacheAndCoalescing(t *testing.T) {
	cl, fake, cfg := setup(t, func(c *config.Config) {
		c.Sources = append(c.Sources, config.Source{ID: "multi", Name: "es", Type: config.SourcePush, EngineModel: "multilingual"})
	})
	var wg sync.WaitGroup
	evs := make([]model.Event, 12)
	for i := range evs {
		evs[i] = event("disk full on /var (database)")
		wg.Add(1)
		go func(ev *model.Event) {
			defer wg.Done()
			cl.Enrich(context.Background(), ev)
		}(&evs[i])
	}
	wg.Wait()
	if n := fake.Calls.Load(); n != 1 {
		t.Errorf("12 concurrent events of one pattern made %d calls", n)
	}
	for _, ev := range evs {
		if ev.Category != "database" {
			t.Fatalf("coalesced event not classified: %+v", ev)
		}
	}
	// Cached: no call, and quiet lookups don't count.
	hits := cl.CacheHits.Load()
	again := event("disk full on /var (database)")
	if !cl.ApplyCachedQuiet(&again) || again.Category != "database" || cl.CacheHits.Load() != hits {
		t.Error("quiet cache lookup")
	}
	// Another source's checkpoint is another answer.
	other := event("disk full on /var (database)")
	other.SourceID = "multi"
	cl.Enrich(context.Background(), &other)
	if fake.Calls.Load() != 2 || fake.LastRequest().Model != "multilingual" {
		t.Errorf("per-source checkpoint: calls=%d model=%q", fake.Calls.Load(), fake.LastRequest().Model)
	}
	// Changing the categories clears the cache.
	cfg.Settings.Categories = append(cfg.Settings.Categories, config.Category{Name: "storage", Description: "disks"})
	cl.Configure(cfg)
	fresh := event("disk full on /var (database)")
	if cl.ApplyCached(&fresh) {
		t.Error("cache survived a category change")
	}
}

func TestWrongCheckpointPauses(t *testing.T) {
	cl, fake, _ := setup(t, func(c *config.Config) { c.Settings.Engine.Model = "logs-finetune" })
	ev := event("database down")
	_, err := cl.Classify(context.Background(), &ev)
	if !errors.Is(err, ErrWrongModel) || !strings.Contains(err.Error(), `"logs-finetune"`) {
		t.Fatalf("silently rerouted checkpoint not detected: %v", err)
	}
	if cl.Available() {
		t.Error("a checkpoint mismatch should pause calls")
	}
	before := fake.Calls.Load()
	ev2 := event("another")
	cl.Enrich(context.Background(), &ev2)
	if fake.Calls.Load() != before || cl.Skipped.Load() != 1 || ev2.Engine != nil {
		t.Error("paused classifier still called the engine")
	}
}

func TestRetriesAndFailover(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	fake.Busy.Store(1)
	start := time.Now()
	ev := event("database timeout")
	if _, err := cl.Classify(context.Background(), &ev); err != nil {
		t.Fatalf("503 not retried: %v", err)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Error("Retry-After not honoured")
	}

	// First instance down: the second answers.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cl2, fake2, _ := setup(t, nil)
	cfg := config.Defaults()
	cfg.Settings.Engine.URL = dead.URL + ", " + fake2.URL
	config.Normalize(cfg)
	cl2.Configure(cfg)
	for i := 0; i < 3; i++ {
		ev := event("database timeout " + string(rune('a'+i)))
		if _, err := cl2.Classify(context.Background(), &ev); err != nil {
			t.Fatalf("failover: %v", err)
		}
	}
}

func TestErrorsPauseOnlyWhenTheNextCallWouldFailToo(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	fake.Fail.Store(10)
	ev := event("x")
	cl.Enrich(context.Background(), &ev)
	if cl.Available() || ev.Engine != nil || ev.Category != model.Uncategorized {
		t.Errorf("5xx should back off and leave the event as the rules classified it: %+v", ev)
	}
	if st := cl.Status(); st.Errors == 0 || st.LastError == "" || st.PausedUntil.IsZero() {
		t.Errorf("status %+v", st)
	}

	cl2, fake2, _ := setup(t, nil)
	fake2.Decide = func(string, enginetest.Question) enginetest.Answer { return enginetest.Answer{} }
	fake2.Key = "required"
	ev2 := event("y")
	if _, err := cl2.Classify(context.Background(), &ev2); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("auth error: %v", err)
	}
	if cl2.Available() {
		t.Error("401 should pause")
	}
}

func TestProbe(t *testing.T) {
	cl, _, _ := setup(t, nil)
	eps, err := cl.Probe(context.Background())
	if err != nil || len(eps) != 1 || !eps[0].OK || !eps[0].ModelLoaded || eps[0].Device != "cpu" || eps[0].Revision == "" {
		t.Errorf("probe %v %+v", err, eps)
	}
	none, _, _ := setup(t, func(c *config.Config) { c.Settings.Engine.URL = "" })
	if _, err := none.Probe(context.Background()); err == nil {
		t.Error("probe without a URL should fail")
	}
}

func TestBuildState(t *testing.T) {
	ev := model.Event{SourceName: "api", Level: "error", Message: "Request failed\n\tat a.B.c(B.java:1)\n\tat a.B.d(B.java:2)\nCaused by: java.io.IOException: disk full",
		Fields: map[string]any{"status": 500, "user": map[string]any{"id": 7}}}
	got := buildState(&ev)
	want := "Request failed\nCaused by: java.io.IOException: disk full\nlevel: error, source: api\nstatus=500 user={\"id\":7}\n\tat a.B.c(B.java:1)\n\tat a.B.d(B.java:2)"
	if got != want {
		t.Errorf("state:\n%q\nwant\n%q", got, want)
	}
	long := model.Event{Message: strings.Repeat("é", 3000)}
	if s := buildState(&long); len(s) > stateBudget || !strings.HasSuffix(s, "é") {
		t.Errorf("clipped state: %d bytes", len(s))
	}
	py := model.Event{Message: "job failed\nTraceback (most recent call last):\n  File \"w.py\", line 1\nValueError: bad input"}
	if s := buildState(&py); !strings.HasPrefix(s, "job failed\nValueError: bad input") {
		t.Errorf("python root cause: %q", s)
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []func(*config.Config){
		func(c *config.Config) { c.Settings.Engine.URL = "ftp://x" },
		func(c *config.Config) { c.Settings.Engine.Model = "../etc" },
		func(c *config.Config) { c.Settings.Engine.MinConfidence = 1.5 },
		func(c *config.Config) {
			c.Sources = append(c.Sources, config.Source{Name: "s", Type: config.SourcePush, EngineModel: "a/b"})
		},
	}
	for i, m := range bad {
		c := config.Defaults()
		m(c)
		if err := config.Normalize(c); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	c := config.Defaults()
	c.Settings.Engine.Model = "ML"
	c.Settings.Engine.URL = "http://a:8000/, https://b"
	if err := config.Normalize(c); err != nil || c.Settings.Engine.Model != "multilingual" {
		t.Errorf("alias: %v %q", err, c.Settings.Engine.Model)
	}
	urls, _ := config.ParseEngineURLs(c.Settings.Engine.URL)
	if strings.Join(urls, ",") != "http://a:8000,https://b" {
		t.Errorf("urls %v", urls)
	}
}

func TestWarningsAndHints(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	fake.Collapse = 5
	ev := event("database down")
	if _, err := cl.Classify(context.Background(), &ev); err != nil {
		t.Fatal(err)
	}
	if w := cl.Status().Warning; !strings.Contains(w, "5 of 11 categories") {
		t.Errorf("option collapse not reported: %q", w)
	}

	keyed, fake2, _ := setup(t, nil)
	fake2.Key = "secret" // our client sends none
	eps, _ := keyed.Probe(context.Background())
	if len(eps) != 1 || !eps[0].OK || eps[0].ModelLoaded || !strings.Contains(eps[0].Error, "API key") {
		t.Errorf("health without the key: %+v", eps)
	}
}

func TestTimeoutIsNotRetried(t *testing.T) {
	var calls int
	var mu sync.Mutex
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	c := NewClient([]string{slow.URL}, "", 100*time.Millisecond, 2)
	start := time.Now()
	_, err := c.Decide(context.Background(), Request{State: "x", Questions: map[string]Question{}, Model: "english"})
	if err == nil || time.Since(start) > 250*time.Millisecond {
		t.Errorf("timeout: %v after %v", err, time.Since(start))
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("a timed-out request was retried: %d calls", calls)
	}
}
