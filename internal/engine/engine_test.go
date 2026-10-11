package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/engine/enginetest"
	"github.com/vipinmohan22/layalogs/internal/model"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// setup returns a classifier configured against a fake laya-serve. Health
// is checked synchronously; call cl.checkHealth() after changing the fake.
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
	cl.syncHealth = true
	t.Cleanup(cl.Close)
	cl.Configure(c)
	return cl, fake, c
}

func event(msg string) model.Event {
	return model.Event{ID: model.NewID(), SourceID: "src", SourceName: "app", Message: msg, Severity: model.SeverityError,
		Category: model.Uncategorized, Fingerprint: "fp-" + msg}
}

func TestRequestContract(t *testing.T) {
	cl, fake, _ := setup(t, func(c *config.Config) {
		c.Settings.Engine.APIKey = "k3y"
		c.Settings.Categories[3].Description = "connection pools, queries, replication"
	})
	fake.Key = "k3y"
	cl.checkHealth()
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
	crit := string(req.Questions["category"])
	if !strings.Contains(crit, `"security":null`) || !strings.Contains(crit, `"database":"connection pools, queries, replication"`) || !strings.Contains(crit, `"uncategorized":"none of the above"`) {
		t.Errorf("criteria: names alone, descriptions when given, uncategorized as none of the above: %s", crit)
	}
	if _, ok := req.Questions["severity"]; ok {
		t.Error("severity asked although the record stated its level")
	}
	state, _ := req.State.(string)
	lines := strings.Split(state, "\n")
	if !strings.HasPrefix(lines[1], "Caused by: java.sql.SQLTransientConnectionException") || !strings.Contains(lines[2], "level: ERROR") {
		t.Errorf("root cause should come right after the first line:\n%s", state)
	}
	if ev.Category != "database" || ev.Classifier != ByEngine || ev.Engine == nil || !ev.Engine.Applied || ev.Engine.Model != "english" || ev.Engine.Threshold != 0.6 {
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
	ev.Severity, ev.SeverityGuessed = model.SeverityWarning, true
	cl.Enrich(context.Background(), &ev)
	if _, ok := fake.LastRequest().Questions["severity"]; !ok {
		t.Fatal("severity not asked for a guessed severity")
	}
	if ev.Category != model.Uncategorized || !ev.Engine.Applied || ev.Severity != model.SeverityError || ev.Engine.SeverityConfidence != 0.7 {
		t.Errorf("low-confidence category must not apply, confident severity must: %+v %+v", ev, ev.Engine)
	}
	if ev.Engine.Category != "network" || len(ev.Engine.Top) != 3 || cl.LowConfidence.Load() != 1 || cl.Applied.Load() != 1 {
		t.Errorf("answer not recorded or counted: %+v low=%d applied=%d", ev.Engine, cl.LowConfidence.Load(), cl.Applied.Load())
	}

	// A stated severity is never replaced, and an answer that changes
	// nothing is not "applied".
	stated := event("upstream hiccup two")
	stated.Severity = model.SeverityWarning
	cl.Enrich(context.Background(), &stated)
	if _, ok := fake.LastRequest().Questions["severity"]; ok || stated.Severity != model.SeverityWarning {
		t.Errorf("stated severity: asked or replaced: %+v", stated)
	}
	if stated.Engine.Applied || stated.Classifier == ByEngine {
		t.Errorf("nothing changed, yet marked applied: %+v", stated.Engine)
	}
	same := event("already an error")
	same.SeverityGuessed = true
	cl.Enrich(context.Background(), &same)
	if same.Engine.Applied {
		t.Error("an equal severity is not a change")
	}

	// A rule's category always wins.
	ruled := event("database pool exhausted")
	ruled.Category = "custom"
	cl.Enrich(context.Background(), &ruled)
	if ruled.Category != "custom" || ruled.Classifier == ByEngine {
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

func TestCacheCoalescingAndSharing(t *testing.T) {
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
	if hits := cl.CacheHits.Load(); hits != 11 {
		t.Errorf("events answered without a request: %d, want 11", hits)
	}
	// Another source with the same checkpoint shares the answer; file
	// analysis reads it without counting.
	hits := cl.CacheHits.Load()
	other := event("disk full on /var (database)")
	other.SourceID, other.Fingerprint = "upload", "different-fingerprint"
	if !cl.ApplyCachedQuiet(&other) || other.Category != "database" || cl.CacheHits.Load() != hits {
		t.Error("answers should be shared across sources by message pattern")
	}
	// A source with its own checkpoint gets its own answer.
	ml := event("disk full on /var (database)")
	ml.SourceID = "multi"
	cl.Enrich(context.Background(), &ml)
	if fake.Calls.Load() != 2 || fake.LastRequest().Model != "multilingual" {
		t.Errorf("per-source checkpoint: calls=%d model=%q", fake.Calls.Load(), fake.LastRequest().Model)
	}
	// Changing the categories clears the cache; changing the timeout doesn't.
	cfg.Settings.Engine.TimeoutSeconds = 45
	cl.Configure(cfg)
	if again := event("disk full on /var (database)"); !cl.ApplyCached(&again) {
		t.Error("cache cleared by an unrelated setting")
	}
	cfg.Settings.Categories = append(cfg.Settings.Categories, config.Category{Name: "storage"})
	cl.Configure(cfg)
	if fresh := event("disk full on /var (database)"); cl.ApplyCached(&fresh) {
		t.Error("cache survived a category change")
	}
}

func TestCheckpointsMustBeLoaded(t *testing.T) {
	cl, fake, cfg := setup(t, func(c *config.Config) {
		c.Sources = append(c.Sources, config.Source{ID: "multi", Name: "es", Type: config.SourcePush, EngineModel: "multilingual"})
	})
	fake.Models = []string{"english"}
	cl.checkHealth()
	ml := event("Verbindung fehlgeschlagen")
	ml.SourceID = "multi"
	if cl.Available(&ml) {
		t.Fatal("a checkpoint the engine hasn't loaded must not be asked (laya-serve would load it)")
	}
	cl.Enrich(context.Background(), &ml)
	if fake.Calls.Load() != 0 || cl.Skipped.Load() != 1 {
		t.Errorf("calls=%d skipped=%d", fake.Calls.Load(), cl.Skipped.Load())
	}
	en := event("database down")
	if !cl.Available(&en) {
		t.Error("the default checkpoint is loaded and should be asked")
	}
	st := cl.Status()
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], `"multilingual"`) || strings.Join(st.Loaded, ",") != "english" {
		t.Errorf("status %+v", st)
	}
	eps, _ := cl.Probe(context.Background())
	if len(eps) != 1 || eps[0].ModelLoaded || strings.Join(eps[0].Missing, ",") != "multilingual" {
		t.Errorf("probe should name the missing checkpoint: %+v", eps)
	}
	_ = cfg
}

func TestWrongCheckpointPausesOnlyThatCheckpoint(t *testing.T) {
	cl, fake, cfg := setup(t, func(c *config.Config) {
		c.Sources = append(c.Sources, config.Source{ID: "ft", Name: "ft", Type: config.SourcePush, EngineModel: "logs-finetune"})
	})
	fake.HideLoaded = true // the engine doesn't say what it has
	cl.checkHealth()
	ev := event("database down")
	ev.SourceID = "ft"
	_, err := cl.Classify(context.Background(), &ev)
	if !errors.Is(err, ErrWrongModel) || !strings.Contains(err.Error(), `"logs-finetune"`) {
		t.Fatalf("silently rerouted checkpoint not detected: %v", err)
	}
	cl.Enrich(context.Background(), &ev)
	if cl.Available(&ev) {
		t.Error("a checkpoint mismatch should pause that checkpoint")
	}
	other := event("another")
	if !cl.Available(&other) {
		t.Error("other checkpoints must keep working")
	}
	// Fixing the setting lifts the pause at once.
	cfg.Sources[len(cfg.Sources)-1].EngineModel = "multilingual"
	cl.Configure(cfg)
	if !cl.Available(&ev) {
		t.Error("pause not lifted after the checkpoint was corrected")
	}
}

func TestBusyAndSlowEnginesAreNotDown(t *testing.T) {
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

	// Busy until the event's deadline: no pause.
	fake.Busy.Store(1000)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	busy := event("busy one")
	cl.Enrich(ctx, &busy)
	cancel()
	if !cl.Available(&busy) || busy.Engine != nil {
		t.Errorf("a busy engine must not be paused: %+v", cl.Status())
	}
	fake.Busy.Store(0)

	// Requests running out of time: a pause only after several in a row.
	block := make(chan struct{})
	defer close(block)
	fake.Decide = func(string, enginetest.Question) enginetest.Answer { <-block; return enginetest.Answer{} }
	for i := 0; i < timeoutsToPause; i++ {
		ev := event("slow " + string(rune('a'+i)))
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		cl.Enrich(ctx, &ev)
		cancel()
		if paused := !cl.Available(&ev); paused != (i == timeoutsToPause-1) {
			t.Fatalf("after %d timeouts paused=%v", i+1, paused)
		}
	}
	if cl.TimedOut.Load() != uint64(timeoutsToPause)+1 { // the busy one ran out of time too
		t.Errorf("timed out = %d", cl.TimedOut.Load())
	}
}

func TestFailuresBackOffOnceAndHealthLiftsThePause(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	fake.Fail.Store(100)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := event("failing " + string(rune('a'+i)))
			cl.Enrich(context.Background(), &ev)
		}(i)
	}
	wg.Wait()
	st := cl.Status()
	if st.PausedUntil.IsZero() || time.Until(st.PausedUntil) > 6*time.Second {
		t.Errorf("errors arriving together should back off once (5s): paused until %v", st.PausedUntil)
	}
	fake.Fail.Store(0)
	cl.checkHealth()
	ev := event("recovered")
	if !cl.Available(&ev) {
		t.Error("a passing health check should lift a pause from failures")
	}

	// A rejected key pauses until the configuration changes.
	keyed, fake2, _ := setup(t, nil)
	fake2.Key = "required"
	k := event("y")
	keyed.Enrich(context.Background(), &k)
	keyed.checkHealth()
	if keyed.Available(&k) {
		t.Error("401 should pause, and a health check shouldn't lift it")
	}
}

func TestTestCallsHaveNoSideEffects(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	fake.Key = "wrong"
	ev := event("database down")
	if _, err := cl.Classify(context.Background(), &ev); err == nil {
		t.Fatal("expected 401")
	}
	fake.Key = ""
	if _, err := cl.Classify(context.Background(), &ev); err != nil {
		t.Fatal(err)
	}
	st := cl.Status()
	if !cl.Available(&ev) || st.Calls != 0 || st.Errors != 0 || st.CachedResults != 0 || ev.Engine != nil {
		t.Errorf("test calls changed live state: %+v %+v", st, ev)
	}
}

func TestFileURLIsReread(t *testing.T) {
	fakeA, fakeB := enginetest.New(), enginetest.New()
	defer fakeA.Close()
	defer fakeB.Close()
	path := filepath.Join(t.TempDir(), "engine_urls")
	os.WriteFile(path, []byte(fakeA.URL+"\n"), 0o600)
	cl, _, _ := setup(t, func(c *config.Config) { c.Settings.Engine.URL = "${file:" + path + "}" })
	ev := event("database one")
	cl.Enrich(context.Background(), &ev)
	os.WriteFile(path, []byte(fakeB.URL+"\n"), 0o600)
	cl.mu.Lock()
	cl.rebuildLocked() // what the health loop does every healthEvery
	cl.mu.Unlock()
	ev2 := event("database two")
	cl.Enrich(context.Background(), &ev2)
	if fakeA.Calls.Load() != 1 || fakeB.Calls.Load() != 1 {
		t.Errorf("calls a=%d b=%d", fakeA.Calls.Load(), fakeB.Calls.Load())
	}
}

func TestRetriesAndFailover(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cl, fake, cfg := setup(t, nil)
	cfg.Settings.Engine.URL = dead.URL + ", " + fake.URL
	cl.Configure(cfg)
	for i := 0; i < 3; i++ {
		ev := event("database timeout " + string(rune('a'+i)))
		if _, err := cl.Classify(context.Background(), &ev); err != nil {
			t.Fatalf("failover: %v", err)
		}
	}
	if st := cl.Status(); st.Capacity != 2 {
		t.Errorf("capacity should be max_in_flight per instance: %d", st.Capacity)
	}
}

func TestLoadEstimate(t *testing.T) {
	cl, _, _ := setup(t, nil)
	if cl.WouldWait(100) != 0 {
		t.Error("no estimate before anything was measured")
	}
	ev := event("database x")
	cl.Enrich(context.Background(), &ev)
	if w := cl.WouldWait(10); w <= 0 {
		t.Errorf("estimate %v", w)
	}
}

func TestWarningsAndHints(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	fake.Collapse = 5
	ev := event("database down")
	cl.Enrich(context.Background(), &ev)
	if w := strings.Join(cl.Status().Warnings, ";"); !strings.Contains(w, "5 of 11 categories") {
		t.Errorf("option collapse not reported: %q", w)
	}

	keyed, fake2, _ := setup(t, nil)
	fake2.Key = "secret" // our client sends none
	eps, _ := keyed.Probe(context.Background())
	if len(eps) != 1 || eps[0].OK || eps[0].ModelLoaded || !strings.Contains(eps[0].Error, "API key") {
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

func TestTrainingRecord(t *testing.T) {
	cl, _, _ := setup(t, nil)
	ev := event("ERROR db pool exhausted")
	ev.Category = "database"
	rec, ok := cl.TrainingRecord(&ev)
	if !ok || rec.Expected["category"] != "database" || rec.State != "ERROR db pool exhausted\nsource: app" || rec.Questions["category"].Type != TypeChoice {
		t.Errorf("record %+v %v", rec, ok)
	}
	ev.Category = "made-up"
	if _, ok := cl.TrainingRecord(&ev); ok {
		t.Error("only configured categories are training labels")
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
		func(c *config.Config) { c.Settings.Engine.MaxInFlight = 17 },
		func(c *config.Config) { c.Settings.Engine.TimeoutSeconds = 301 },
		func(c *config.Config) {
			c.Settings.Categories = append(c.Settings.Categories, config.Category{Name: "uncategorized"})
		},
		func(c *config.Config) {
			c.Settings.Categories = append(c.Settings.Categories, config.Category{Name: "db", Description: "a"}, config.Category{Name: "DB", Description: "b"})
		},
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
	c.Settings.Engine.Model = "convaiinnovations/laya-multilingual"
	c.Settings.Engine.URL = "http://a:8000/, https://b"
	if err := config.Normalize(c); err != nil || c.Settings.Engine.Model != "multilingual" {
		t.Errorf("alias: %v %q", err, c.Settings.Engine.Model)
	}
	urls, _ := config.ParseEngineURLs(c.Settings.Engine.URL)
	if strings.Join(urls, ",") != "http://a:8000,https://b" {
		t.Errorf("urls %v", urls)
	}
}
