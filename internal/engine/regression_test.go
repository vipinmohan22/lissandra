package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine/enginetest"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// Regression: fixing a wrong URL (404 => 5-minute config pause) does not lift the
// pause, although Configure's doc and the README say it does.
func TestURLFixLiftsPause(t *testing.T) {
	bad := httptest.NewServer(http.NotFoundHandler()) // some other HTTP service
	defer bad.Close()
	fake := enginetest.New()
	defer fake.Close()
	c := config.Defaults()
	c.Settings.Engine.URL = bad.URL
	c.Settings.Engine.APIKey = ""
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	cl := New(quiet())
	cl.syncHealth = true
	defer cl.Close()
	cl.Configure(c)
	ev := event("database down")
	cl.Enrich(context.Background(), &ev)
	t.Logf("after 404: paused_until=%v last_error=%q", cl.Status().PausedUntil, cl.Status().LastError)

	c2 := *c
	c2.Settings.Engine.URL = fake.URL // the operator corrects the URL
	cl.Configure(&c2)
	ev2 := event("database down")
	cl.Enrich(context.Background(), &ev2)
	st := cl.Status()
	t.Logf("after URL fix: urls=%v paused_until=%v available=%v fake_calls=%d engine=%v skipped=%d",
		st.URLs, st.PausedUntil, cl.Available(&ev2), fake.Calls.Load(), ev2.Engine, st.Skipped)
	if fake.Calls.Load() == 0 {
		t.Errorf("corrected URL not used: calls stay paused until %v", st.PausedUntil)
	}
}

// Regression: an event joining a flight started before the categories changed gets
// the answer to the old question.
func TestNoStaleSharedRequestAfterCategoryChange(t *testing.T) {
	cl, fake, c := setup(t, nil)
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	dflt := fake.Decide
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		started <- struct{}{}
		<-release
		return dflt(state, q)
	}
	a := event("database down")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); cl.Enrich(context.Background(), &a) }()
	<-started
	// "database" is renamed to "storage".
	c2 := *c
	c2.Settings.Categories = nil
	for _, cat := range c.Settings.Categories {
		if cat.Name == "database" {
			cat.Name = "storage"
		}
		c2.Settings.Categories = append(c2.Settings.Categories, cat)
	}
	cl.Configure(&c2)
	b := event("database down")
	wg.Add(1)
	go func() { defer wg.Done(); cl.Enrich(context.Background(), &b) }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	t.Logf("event after reconfig: category=%q classifier=%q fake calls=%d", b.Category, b.Classifier, fake.Calls.Load())
	for _, cat := range c2.Settings.Categories {
		if cat.Name == b.Category {
			return
		}
	}
	if b.Category != model.Uncategorized {
		t.Errorf("event got category %q, which is not configured any more (%d calls)", b.Category, fake.Calls.Load())
	}
}

// Regression: a waiter with plenty of time loses its answer because the leader of
// its flight had too little time left (errNoSlot).
func TestWaiterRetriesWhenLeaderHadNoSlot(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	dflt := fake.Decide
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		time.Sleep(300 * time.Millisecond)
		return dflt(state, q)
	}
	warm := event("database warm")
	cl.Enrich(context.Background(), &warm) // avgAttempt ~300ms
	var wg sync.WaitGroup
	blocker := event("network blocker")
	wg.Add(1)
	go func() { defer wg.Done(); cl.Enrich(context.Background(), &blocker) }() // holds the only slot ~300ms
	time.Sleep(50 * time.Millisecond)
	leader := event("database pattern P")
	lctx, lcancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer lcancel()
	wg.Add(1)
	go func() { defer wg.Done(); cl.Enrich(lctx, &leader) }()
	time.Sleep(50 * time.Millisecond)
	waiter := event("database pattern P")
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	wg.Add(1)
	go func() { defer wg.Done(); cl.Enrich(wctx, &waiter) }()
	wg.Wait()
	skipped := cl.Skipped.Load()
	after := event("database pattern P")
	cl.Enrich(context.Background(), &after)
	t.Logf("waiter: category=%q engine=%v ctx_err=%v skipped=%d; a later event: category=%q", waiter.Category, waiter.Engine, wctx.Err(), skipped, after.Category)
	if waiter.Engine == nil && wctx.Err() == nil {
		t.Errorf("waiter with %v left got no answer: it inherited the leader's errNoSlot", time.Until(func() time.Time { d, _ := wctx.Deadline(); return d }()).Round(time.Second))
	}
}

// Regression: the leader's own deadline runs out mid-request; a waiter with time
// left gets the leader's context error, which is counted nowhere.
func TestWaiterRetriesWhenLeaderTimedOut(t *testing.T) {
	cl, fake, _ := setup(t, nil)
	dflt := fake.Decide
	fake.Decide = func(state string, q enginetest.Question) enginetest.Answer {
		if strings.Contains(state, "slow") {
			time.Sleep(time.Second)
		}
		return dflt(state, q)
	}
	defer func(old time.Duration) { ampleTime = old }(ampleTime)
	ampleTime = 10 * time.Millisecond
	warm := event("database warm")
	cl.Enrich(context.Background(), &warm)
	before := cl.Status()
	var wg sync.WaitGroup
	leader := event("database slow P")
	lctx, lcancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer lcancel()
	wg.Add(1)
	go func() { defer wg.Done(); cl.Enrich(lctx, &leader) }()
	time.Sleep(50 * time.Millisecond)
	waiter := event("database slow P")
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	wg.Add(1)
	go func() { defer wg.Done(); cl.Enrich(wctx, &waiter) }()
	wg.Wait()
	st := cl.Status()
	t.Logf("waiter engine=%v ctx_err=%v; deltas: timed_out=%d skipped=%d errors=%d cache_hits=%d",
		waiter.Engine, wctx.Err(), st.TimedOut-before.TimedOut, st.Skipped-before.Skipped, st.Errors-before.Errors, st.CacheHits-before.CacheHits)
	if waiter.Engine == nil && wctx.Err() == nil {
		t.Errorf("waiter with time left got the leader's timeout; counted: timed_out=%d skipped=%d errors=%d",
			st.TimedOut-before.TimedOut, st.Skipped-before.Skipped, st.Errors-before.Errors)
	}
}

// hungServer accepts TCP connections and never answers (a frozen process).
func hungServer(t *testing.T) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			cn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, cn)
			mu.Unlock()
		}
	}()
	return "http://" + ln.Addr().String(), func() {
		ln.Close()
		mu.Lock()
		for _, cn := range conns {
			cn.Close()
		}
		mu.Unlock()
	}
}

// Regression: one hung instance listed first uses up checkHealth's shared 10 s
// budget, so a healthy instance after it is never marked ready.
func TestHungInstanceDoesNotHideHealthyOne(t *testing.T) {
	defer func(old time.Duration) { healthTimeout = old }(healthTimeout)
	healthTimeout = time.Second
	for _, order := range []string{"good,hung", "hung,good"} {
		t.Run(order, func(t *testing.T) {
			hung, stop := hungServer(t)
			defer stop()
			fake := enginetest.New()
			defer fake.Close()
			urls := strings.NewReplacer("good", fake.URL, "hung", hung).Replace(order)
			c := config.Defaults()
			c.Settings.Engine.URL = urls
			c.Settings.Engine.APIKey = ""
			c.Settings.Engine.TimeoutSeconds = 30
			c.Sources = append(c.Sources, config.Source{ID: "multi", Name: "es", Type: config.SourcePush, EngineModel: "multilingual"})
			if err := config.Normalize(c); err != nil {
				t.Fatal(err)
			}
			cl := New(quiet())
			cl.syncHealth = true
			defer cl.Close()
			start := time.Now()
			cl.Configure(c)
			took := time.Since(start)
			cl.mu.Lock()
			ready := cl.client.health.Load().ready
			loaded := cl.loaded
			cl.mu.Unlock()
			ml := event("Verbindung zur Datenbank fehlgeschlagen")
			ml.SourceID = "multi"
			okN := 0
			for i := 0; i < 6; i++ {
				ev := event("database down " + strings.Repeat("x", i))
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				cl.Enrich(ctx, &ev)
				cancel()
				if ev.Engine != nil {
					okN++
				}
			}
			t.Logf("health took %v ready=%v loaded=%v multilingual-available=%v; answered %d/6, timed_out=%d paused_until=%v",
				took.Round(time.Millisecond), ready, loaded, cl.Available(&ml), okN, cl.TimedOut.Load(), cl.Status().PausedUntil)
			if !ready[fake.URL] {
				t.Errorf("healthy instance %s not marked ready", fake.URL)
			}
		})
	}
}

// Regression: ErrWrongModel from one instance pauses the checkpoint everywhere, and
// requests for a checkpoint go to instances that don't have it.
func TestCheckpointGoesOnlyToInstancesWithIt(t *testing.T) {
	a := enginetest.New() // english + multilingual
	defer a.Close()
	b := enginetest.New()
	defer b.Close()
	b.Models = []string{"english"} // multilingual is routed to english, as laya-serve does for names it doesn't serve
	c := config.Defaults()
	c.Settings.Engine.URL = a.URL + "," + b.URL
	c.Settings.Engine.APIKey = ""
	c.Sources = append(c.Sources, config.Source{ID: "multi", Name: "es", Type: config.SourcePush, EngineModel: "multilingual"})
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	cl := New(quiet())
	cl.syncHealth = true
	defer cl.Close()
	cl.Configure(c)
	t.Logf("loaded (union) = %v", cl.Status().Loaded)
	answered := 0
	for i := 0; i < 6; i++ {
		ev := event("Verbindung fehlgeschlagen " + strings.Repeat("x", i))
		ev.SourceID = "multi"
		cl.Enrich(context.Background(), &ev)
		if ev.Engine != nil {
			answered++
		}
	}
	st := cl.Status()
	t.Logf("answered %d/6; a.calls=%d b.calls=%d paused_checkpoints=%v errors=%d last_error=%q",
		answered, a.Calls.Load(), b.Calls.Load(), st.PausedCheckpoints, st.Errors, st.LastError)
	if b.Calls.Load() > 0 {
		t.Errorf("multilingual requests were sent to %s, which has not loaded it", b.URL)
	}
}

// Regression: failover never leaves the ready set: the only ready instance died, the
// other one is up now, and every attempt goes to the dead one.
func TestFailoverReachesInstanceThatCameUp(t *testing.T) {
	a := enginetest.New()
	b := enginetest.New() // its handler is served on bAddr once "up"
	defer b.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	bAddr := ln.Addr().String()
	ln.Close() // b is down during the health check
	c := config.Defaults()
	c.Settings.Engine.URL = a.URL + ",http://" + bAddr
	c.Settings.Engine.APIKey = ""
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	cl := New(quiet())
	cl.syncHealth = true
	defer cl.Close()
	cl.Configure(c) // ready = {a}
	a.Close()       // a goes away
	ln2, err := net.Listen("tcp", bAddr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: b.Server.Config.Handler}
	go srv.Serve(ln2) // b is up now
	defer srv.Close()
	if _, err := http.Get("http://" + bAddr + "/health"); err != nil {
		t.Fatal(err)
	}
	ev := event("database down")
	cl.Enrich(context.Background(), &ev)
	st := cl.Status()
	t.Logf("engine=%v b.calls=%d errors=%d paused_until=%v last_error=%q", ev.Engine, b.Calls.Load(), st.Errors, st.PausedUntil, st.LastError)
	if ev.Engine == nil {
		t.Errorf("failover never tried the instance that is up")
	}
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) get() bool  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
func (a *atomicBool) set(v bool) { a.mu.Lock(); a.v = v; a.mu.Unlock() }

var _ = errors.Is

// Regression: a dial timeout (instance's node went dark) is returned at once as a
// timeout instead of failing over to the next instance.
func TestDialTimeoutFailsOver(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	dark := "http://10.255.255.1:8000" // connects hang: blackholed
	c := config.Defaults()
	c.Settings.Engine.URL = dark + "," + fake.URL
	c.Settings.Engine.APIKey = ""
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	cl := New(quiet())
	defer cl.Close()
	cl.Configure(c)
	cl.mu.Lock()
	client := cl.client
	cl.loaded = map[string]bool{"english": true}
	cl.mu.Unlock()
	client.SetReady(map[string]bool{dark: true, fake.URL: true}) // both answered the last check
	for i := 0; i < 4; i++ {
		ev := event("database down " + strings.Repeat("x", i))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		_, _, err := cl.classify(ctx, &ev, textKey(&ev))
		cancel()
		t.Logf("event %d: %v err=%v", i, time.Since(start).Round(time.Millisecond), err)
	}
	t.Logf("fake calls=%d timeouts=%d", fake.Calls.Load(), cl.timeouts)
}

// Rotating the key: Lissandra gets the new key first, the engine later. Once the
// engine accepts the new key (it lists its checkpoints to it), calls resume
// at the next health check, not 5 minutes after the first 401.
func TestKeyRotationLiftsAuthPause(t *testing.T) {
	cl, fake, _ := setup(t, func(c *config.Config) { c.Settings.Engine.APIKey = "new-key" })
	fake.Key = "old-key"
	ev := event("database rotated")
	cl.Enrich(context.Background(), &ev)
	if cl.Available(&ev) {
		t.Fatal("a rejected key should pause calls")
	}
	cl.checkHealth()
	if cl.Available(&ev) {
		t.Fatal("still rejected: the pause stays")
	}
	fake.Key = "new-key"
	cl.checkHealth()
	if !cl.Available(&ev) {
		t.Errorf("the engine accepts the key now: %+v", cl.Status())
	}
}

// While the key rotates, an instance still on the old key (it hides its
// checkpoints) isn't asked while another lists them.
func TestInstanceOnOldKeyIsNotAsked(t *testing.T) {
	stale := enginetest.New()
	defer stale.Close()
	stale.Key = "old-key"
	cl, fake, cfg := setup(t, func(c *config.Config) { c.Settings.Engine.APIKey = "new-key" })
	fake.Key = "new-key"
	cfg.Settings.Engine.URL = stale.URL + "," + fake.URL
	cl.Configure(cfg)
	cl.checkHealth()
	for _, w := range []string{"alpha", "bravo", "charlie", "delta"} {
		ev := event("database rotated " + w)
		cl.Enrich(context.Background(), &ev)
		if ev.Engine == nil {
			t.Errorf("%s: no answer (%s)", w, cl.Status().LastError)
		}
	}
	if stale.Calls.Load() != 0 {
		t.Errorf("the instance on the old key was asked %d times", stale.Calls.Load())
	}
	st := cl.Status()
	if st.Capacity != 1 || !strings.Contains(strings.Join(st.Warnings, ";"), "its key differs") {
		t.Errorf("capacity %d, warnings %v", st.Capacity, st.Warnings)
	}
}

// Changing only the timeout keeps the last error and the pause; an engine
// nothing answers on shows an error before any event fails.
func TestTimeoutChangeKeepsErrorsAndHealthReportsDeadEngine(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cl, _, cfg := setup(t, nil)
	cfg.Settings.Engine.URL = dead.URL
	cl.Configure(cfg)
	if st := cl.Status(); !strings.Contains(st.LastError, "no engine instance passed its health check") {
		t.Errorf("health failure not shown: %q", st.LastError)
	}
	ev := event("database down")
	cl.mu.Lock()
	cl.loaded = map[string]bool{"english": true}
	cl.mu.Unlock()
	cl.Enrich(context.Background(), &ev)
	before := cl.Status()
	cfg.Settings.Engine.TimeoutSeconds = 45
	cl.Configure(cfg)
	after := cl.Status()
	if after.LastError == "" || after.PausedUntil.IsZero() || before.PausedUntil.IsZero() {
		t.Errorf("a timeout change cleared the state: before %+v after %+v", before, after)
	}
}

// The wrong-checkpoint error doesn't show the URL's credentials.
func TestWrongModelErrorHidesCredentials(t *testing.T) {
	cl, fake, cfg := setup(t, nil)
	fake.HideLoaded = true
	cfg.Settings.Engine.URL = strings.Replace(fake.URL, "http://", "http://laya:hunter2@", 1)
	cfg.Settings.Engine.Model = "mylogs"
	cl.Configure(cfg)
	ev := event("database down")
	cl.Enrich(context.Background(), &ev)
	if st := cl.Status(); strings.Contains(st.LastError, "hunter2") || st.LastError == "" {
		t.Errorf("last error: %q", st.LastError)
	}
}

// warnCounter counts warnings.
type warnCounter struct {
	slog.Handler
	n *atomic.Int64
}

func (h warnCounter) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.n.Add(1)
	}
	return nil
}

// One failed request logs once, not once per event that shared it.
func TestSharedFailureLogsOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok","loaded":["english"]}`))
			return
		}
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"detail":"boom"}`))
	}))
	defer srv.Close()
	var warns atomic.Int64
	cl := New(slog.New(warnCounter{slog.NewTextHandler(io.Discard, nil), &warns}))
	cl.syncHealth = true
	t.Cleanup(cl.Close)
	c := config.Defaults()
	c.Settings.Engine.URL, c.Settings.Engine.APIKey = srv.URL, ""
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	cl.Configure(c)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := event("ERROR one shared pattern")
			cl.Enrich(context.Background(), &ev)
		}()
	}
	wg.Wait()
	if n := warns.Load(); n != 1 {
		t.Errorf("%d warnings for one failed request", n)
	}
}
