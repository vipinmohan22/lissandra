package engine

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

const (
	cacheSize = 5000
	cacheTTL  = 24 * time.Hour

	// healthEvery is how often instances are checked: readiness, loaded
	// checkpoints, and a re-read of a ${file:...} URL.
	healthEvery = 30 * time.Second
	// timeoutsToPause is how many requests in a row may run out of time
	// before the engine is treated as unresponsive and paused.
	timeoutsToPause = 3
	// configPause pauses calls after errors that persist until the
	// configuration changes (wrong key, wrong URL, wrong checkpoint).
	configPause = 5 * time.Minute

	// ByEngine is Event.Classifier when an engine answer was applied.
	ByEngine = "laya"

	// questions sent to the engine
	qCategory = "category"
	qSeverity = "severity"
)

// healthTimeout bounds one instance's health check.
var healthTimeout = 10 * time.Second

// ampleTime is the least time left at the start of a request for its
// timing out to count against the engine.
var ampleTime = 5 * time.Second

// Errors for events that were not sent.
var (
	ErrPaused    = errors.New("laya engine calls are paused after errors")
	ErrNotLoaded = errors.New("checkpoint not loaded on the engine")
	errNoSlot    = errors.New("no free engine slot before the event's deadline")
)

// Result is the engine's classification of one event.
type Result struct {
	Model              string             `json:"model"`
	Category           string             `json:"category"`       // model.Uncategorized when none fits
	CategoryAsked      bool               `json:"category_asked"` // false when no categories are configured
	CategoryConfidence float64            `json:"category_confidence"`
	Probabilities      map[string]float64 `json:"probabilities,omitempty"`
	Severity           model.Severity     `json:"severity,omitempty"` // only when asked
	SeverityConfidence float64            `json:"severity_confidence,omitempty"`
	Truncated          bool               `json:"truncated,omitempty"`
	LatencyMs          int64              `json:"latency_ms"`
	InferenceMs        int64              `json:"inference_ms,omitempty"`
}

type cached struct {
	Result
	at time.Time
}

// flight coalesces concurrent requests for the same pattern.
type flight struct {
	done chan struct{}
	res  Result
	err  error
}

// Status is reported in the UI and /api/stats.
type Status struct {
	Enabled       bool      `json:"enabled"`
	Configured    bool      `json:"configured"`
	ConfigError   string    `json:"config_error,omitempty"`
	URLs          []string  `json:"urls"`
	Model         string    `json:"model"`
	ModelsInUse   []string  `json:"models_in_use"`  // the default and per-source checkpoints
	Loaded        []string  `json:"loaded"`         // checkpoints the engine reported, if it said
	Capacity      int       `json:"capacity"`       // concurrent requests: max_in_flight × instances Lissandra can ask
	Calls         uint64    `json:"calls_total"`    // requests made for live and file events
	CacheHits     uint64    `json:"cache_hits"`     // live events answered without a request
	Errors        uint64    `json:"errors"`         // failed requests
	TimedOut      uint64    `json:"timed_out"`      // events that ran out of time during a request
	Applied       uint64    `json:"applied"`        // live answers that changed a category or severity
	LowConfidence uint64    `json:"low_confidence"` // live category answers below min_confidence
	Truncated     uint64    `json:"truncated"`      // events cut to fit the model
	Skipped       uint64    `json:"skipped"`        // eligible live events not sent: busy, paused or not loaded
	AvgLatencyMs  int64     `json:"avg_latency_ms"` // recent round trips, queueing on the engine included
	AvgInferMs    int64     `json:"avg_inference_ms"`
	InFlight      int       `json:"in_flight"`
	CachedResults int       `json:"cached_results"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`
	PausedUntil   time.Time `json:"paused_until,omitzero"`
	// PausedCheckpoints are paused alone: the engine answered as another.
	PausedCheckpoints []string `json:"paused_checkpoints,omitempty"`
	// Warnings are problems with the setup: checkpoints the engine hasn't
	// loaded, categories it can't tell apart.
	Warnings []string `json:"warnings,omitempty"`
}

// Classifier classifies events with laya-serve. All methods are safe for
// concurrent use.
type Classifier struct {
	log *slog.Logger

	mu           sync.Mutex
	settings     config.EngineSettings
	categories   []config.Category
	sourceModels map[string]string
	urls         []string
	key          string
	client       *Client
	clientKey    string
	configErr    string
	sem          chan struct{}
	cache        map[string]cached
	cacheGen     uint64 // bumped whenever cache is replaced
	flights      map[string]*flight
	lastErr      string
	lastErrAt    time.Time
	optionsWarn  string
	pausedUntil  time.Time // all calls: engine unreachable, failing or rejecting the key
	connPause    bool      // pausedUntil is from connection failures (a passing health check lifts it)
	authPause    bool      // pausedUntil is from a rejected key (an instance listing its checkpoints to the key lifts it)
	healthErr    bool      // lastErr is from a health check no instance passed (one passing clears it)
	failures     int       // consecutive failures, for backoff
	timeouts     int       // consecutive requests that ran out of time
	modelPause   map[string]time.Time
	loaded       map[string]bool   // union over instances; nil when unknown
	revisions    map[string]string // checkpoint revisions the engine reported
	avgLatency   float64           // ms, moving average of round trips
	avgAttempt   float64           // …of the answering request alone
	avgInfer     float64
	lastOK       time.Time

	stop       chan struct{}
	stopOnce   sync.Once
	loopOnce   sync.Once
	syncHealth bool // tests: check health inside Configure

	Calls, CacheHits, Errors, TimedOut, Applied, LowConfidence, Truncated, Skipped atomic.Uint64
	// Totals for metrics.
	LatencyMsTotal, InferMsTotal, Completed atomic.Uint64
	inFlight, waiting                       atomic.Int64
}

// New returns an unconfigured classifier; call Configure. Close stops its
// background health checks.
func New(log *slog.Logger) *Classifier {
	return &Classifier{
		log: log, cache: map[string]cached{}, flights: map[string]*flight{}, modelPause: map[string]time.Time{},
		sem: make(chan struct{}, 1), stop: make(chan struct{}),
	}
}

// Close stops background work.
func (c *Classifier) Close() { c.stopOnce.Do(func() { close(c.stop) }) }

// Configure applies cfg. Changing the URL, key, checkpoints or categories
// clears cached answers and lifts pauses, so a corrected setting takes
// effect at once.
func (c *Classifier) Configure(cfg *config.Config) {
	models := map[string]string{}
	for _, src := range cfg.Sources {
		if src.EngineModel != "" {
			models[src.ID] = src.EngineModel
		}
	}
	s := cfg.Settings.Engine
	x := expand(s) // may read files: not under mu
	c.mu.Lock()
	answersChange := s.Model != c.settings.Model || !maps.Equal(models, c.sourceModels) || !slices.Equal(cfg.Settings.Categories, c.categories)
	c.settings = s
	c.categories = slices.Clone(cfg.Settings.Categories)
	c.sourceModels = models
	if answersChange {
		c.resetAnswersLocked()
	}
	c.rebuildLocked(x)
	c.mu.Unlock()
	c.loopOnce.Do(func() { go c.healthLoop() })
	if c.syncHealth {
		c.checkHealth()
	} else {
		go c.checkHealth()
	}
}

// resetAnswersLocked forgets cached answers and pauses. Caller holds mu.
func (c *Classifier) resetAnswersLocked() {
	c.cache = map[string]cached{}
	c.cacheGen++
	c.optionsWarn = ""
	c.modelPause = map[string]time.Time{}
	c.pausedUntil, c.connPause, c.authPause, c.failures, c.timeouts = time.Time{}, false, false, 0, 0
}

// expanded is the engine URL and key with ${env:}/${file:} references
// expanded.
type expanded struct{ url, key string }

func expand(s config.EngineSettings) expanded {
	return expanded{config.ExpandEnv(s.URL), config.ExpandEnv(s.APIKey)}
}

// rebuildLocked (re)creates the client when the expanded URL or key
// changed. Caller holds mu.
func (c *Classifier) rebuildLocked(x expanded) {
	s := c.settings
	urls, err := config.ParseEngineURLs(x.url)
	key := x.key
	c.configErr = ""
	if err != nil {
		c.configErr = "engine url: " + err.Error()
		var ue *config.URLError
		if config.HasRef(s.URL) && errors.As(err, &ue) {
			// Don't show what a reference expanded to: it may be a secret.
			c.configErr = fmt.Sprintf("engine url: entry %d of the list %s expands to is not an http(s) base URL", ue.Entry, config.MaskURLs(s.URL))
		}
		urls = nil
	} else if len(urls) == 0 {
		// A reference Lissandra may not read expands to nothing: say so, not "no URL".
		if rerr := config.CheckRefs(s.URL); rerr != nil {
			c.configErr = "engine url: " + rerr.Error() + "; it reads as empty"
		}
	}
	capacity := max(s.MaxInFlight, 1) * max(len(urls), 1)
	clientKey := fmt.Sprint(urls, "\x00", key, "\x00", s.TimeoutSeconds, "\x00", capacity)
	if clientKey != c.clientKey {
		if c.client != nil {
			c.client.Close()
		}
		keyChanged := key != c.key
		endpointChanged := keyChanged || !slices.Equal(urls, c.urls)
		c.client, c.clientKey, c.urls, c.key = nil, clientKey, urls, key
		if len(urls) > 0 {
			c.client = NewClient(urls, key, time.Duration(s.TimeoutSeconds)*time.Second, capacity)
		}
		c.loaded = nil
		switch {
		case keyChanged:
			// Possibly another engine. Instances coming and going (Nomad
			// scaling or rescheduling) keep the answers: a new checkpoint
			// revision is caught by the health check.
			c.resetAnswersLocked()
		case endpointChanged:
			// A corrected URL takes effect at once.
			c.modelPause = map[string]time.Time{}
			c.pausedUntil, c.connPause, c.authPause, c.failures, c.timeouts = time.Time{}, false, false, 0, 0
		}
		if endpointChanged {
			c.lastErr, c.lastErrAt, c.healthErr = "", time.Time{}, false // about the old engine
		}
	}
	if cap(c.sem) != capacity {
		c.sem = make(chan struct{}, capacity)
	}
}

// healthLoop checks the engine regularly: it re-reads a ${file:...} URL,
// learns which instances are up and which checkpoints they hold, and lifts
// a pause from connection failures once an instance answers again.
func (c *Classifier) healthLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var last time.Time
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			// Every 30 s, or every 5 s while no instance has answered yet.
			c.mu.Lock()
			every := healthEvery
			if c.loaded == nil {
				every = 5 * time.Second
			}
			c.mu.Unlock()
			if time.Since(last) < every {
				continue
			}
			last = time.Now()
			c.mu.Lock()
			s := c.settings
			c.mu.Unlock()
			if config.HasRef(s.URL) || config.HasRef(s.APIKey) {
				x := expand(s)
				c.mu.Lock()
				if c.settings.URL == s.URL && c.settings.APIKey == s.APIKey {
					c.rebuildLocked(x)
				}
				c.mu.Unlock()
			}
			c.checkHealth()
		}
	}
}

// CheckHealth probes every instance now and records the result: which are
// up and which checkpoints each has. The classifier also does this in the
// background; call it to know the engine before the first events.
func (c *Classifier) CheckHealth() { c.checkHealth() }

// checkHealth probes the instances concurrently, so a hung one can't hide
// the others, and records the result.
func (c *Classifier) checkHealth() {
	c.mu.Lock()
	client := c.client
	enabled := c.settings.Enabled
	c.mu.Unlock()
	if client == nil || !enabled {
		return
	}
	type probe struct {
		h   Health
		err error
	}
	urls := client.URLs()
	probes := make([]probe, len(urls))
	var wg sync.WaitGroup
	for i, base := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
			defer cancel()
			h, err := client.Health(ctx, base)
			probes[i] = probe{h, err}
		}()
	}
	wg.Wait()
	ready := map[string]bool{}
	listed := false // an instance listed its checkpoints: it accepts the key
	models := map[string]map[string]bool{}
	var loaded map[string]bool
	revisions := map[string]string{}
	for i, base := range urls {
		p := probes[i]
		if p.err != nil {
			continue
		}
		ready[base] = true
		if p.h.Loaded == nil {
			models[base] = nil // didn't say (no key)
			continue
		}
		models[base] = map[string]bool{}
		if loaded == nil {
			loaded = map[string]bool{}
		}
		for _, m := range p.h.Loaded {
			loaded[m] = true
			models[base][m] = true
		}
		listed = true
		for m, r := range p.h.Revisions {
			if r != "" {
				revisions[m] = r
			}
		}
	}
	client.SetHealth(ready, models)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != client {
		return // reconfigured meanwhile
	}
	if len(ready) == 0 && len(probes) > 0 {
		// Show an unreachable engine before any event fails on it.
		c.lastErr, c.lastErrAt, c.healthErr = "no engine instance passed its health check: "+probes[0].err.Error(), time.Now(), true
	} else if c.healthErr {
		c.lastErr, c.lastErrAt, c.healthErr = "", time.Time{}, false
	}
	if len(ready) > 0 {
		// Capacity is max_in_flight per instance Lissandra can ask: one that is
		// down, or hides its checkpoints while another lists them (another
		// key), takes no requests, so the others must not get its share.
		usable := 0
		for u := range ready {
			if !listed || models[u] != nil {
				usable++
			}
		}
		if n := max(c.settings.MaxInFlight, 1) * usable; n > 0 && cap(c.sem) != n {
			c.sem = make(chan struct{}, n)
		}
		c.loaded = loaded
		if c.connPause || c.authPause && listed {
			c.pausedUntil, c.connPause, c.authPause, c.failures = time.Time{}, false, false, 0
		}
		for m, r := range revisions {
			if old, ok := c.revisions[m]; ok && old != r {
				c.resetAnswersLocked() // new weights: earlier answers no longer hold
				break
			}
		}
		if c.revisions == nil {
			c.revisions = map[string]string{}
		}
		maps.Copy(c.revisions, revisions)
	}
}

// Eligible reports whether ev should be classified by the engine under the
// current settings.
func (c *Classifier) Eligible(ev *model.Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.settings
	if !s.Enabled || c.client == nil || ev.NoEngine || !ev.Severity.AtLeast(s.MinSeverity) {
		return false
	}
	if s.OnlyUncategorized && ev.Category != model.Uncategorized {
		return false
	}
	return len(c.categories) > 0 || c.asksSeverity(ev)
}

// asksSeverity reports whether the engine should also judge ev's severity:
// only when it was guessed from keywords (the record stated no level and no
// rule set one). Caller holds mu.
func (c *Classifier) asksSeverity(ev *model.Event) bool {
	return c.settings.SetSeverity && ev.SeverityGuessed
}

// Available reports whether ev's checkpoint may be asked now: calls aren't
// paused, and the engine has it loaded (when the engine says what it has).
func (c *Classifier) Available(ev *model.Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.availableLocked(c.modelLocked(ev)) == nil
}

func (c *Classifier) availableLocked(checkpoint string) error {
	now := time.Now()
	switch {
	case now.Before(c.pausedUntil):
		return ErrPaused
	case now.Before(c.modelPause[checkpoint]):
		return fmt.Errorf("%w: %q", ErrPaused, checkpoint)
	case c.loaded != nil && !c.loaded[checkpoint]:
		// Asking anyway would make laya-serve load it: a second model in
		// memory, possibly more than its allocation holds.
		return fmt.Errorf("%w: %q", ErrNotLoaded, checkpoint)
	case c.loaded == nil && checkpoint != c.settings.Model:
		// Until the engine says what it has, only the default is asked.
		return fmt.Errorf("%w: %q (the engine hasn't reported its checkpoints yet)", ErrNotLoaded, checkpoint)
	case c.client != nil && !c.client.Has(checkpoint, checkpoint != c.settings.Model):
		// No instance has it (one may have answered with another).
		return c.notLoadedErrLocked(checkpoint)
	}
	return nil
}

// Pending reports whether an answer for ev's pattern is already on its way:
// another event's request will answer it too.
func (c *Classifier) Pending(ev *model.Event) bool {
	text := textKey(ev)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.flights[c.flightKeyLocked(ev, text)]
	return ok
}

// AnswerKey identifies the request that answers ev: events with the same
// key, asked about together, cost one request.
func (c *Classifier) AnswerKey(ev *model.Event) string {
	text := textKey(ev)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flightKeyLocked(ev, text)
}

// flightKeyLocked identifies an open request: the answer's key, and the
// cache generation so no event joins a request about old categories.
func (c *Classifier) flightKeyLocked(ev *model.Event, text string) string {
	return fmt.Sprint(c.cacheGen, "\x00", c.keyLocked(ev, text))
}

// MaxWait is how long an event may wait for an answer, queueing included,
// before it goes on with the rules' classification.
func (c *Classifier) MaxWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Duration(max(c.settings.TimeoutSeconds, 1)) * time.Second
}

// unmeasured is the time per answer WouldWait assumes until one is
// measured, so a burst at startup isn't all queued only to time out.
const unmeasured = 1000.0 // ms

// WouldWait estimates how long an event queued behind queued others would
// wait for its answer, from recent request times and the engine's capacity.
func (c *Classifier) WouldWait(queued int) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	per := c.avgAttempt
	if c.Completed.Load() == 0 {
		per = unmeasured
	}
	ahead := float64(queued+int(c.waiting.Load())+int(c.inFlight.Load())) / float64(max(cap(c.sem), 1))
	return time.Duration((ahead + 1) * per * float64(time.Millisecond))
}

// Capacity is how many requests the engine takes at once: max_in_flight
// per instance times the instances.
func (c *Classifier) Capacity() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cap(c.sem)
}

// ApplyCached applies a cached answer for ev's pattern. It returns false on
// a miss.
func (c *Classifier) ApplyCached(ev *model.Event) bool {
	if !c.applyCached(ev, true) {
		return false
	}
	c.CacheHits.Add(1)
	return true
}

// ApplyCachedQuiet is ApplyCached for file analysis: nothing is counted in
// the live metrics.
func (c *Classifier) ApplyCachedQuiet(ev *model.Event) bool { return c.applyCached(ev, false) }

func (c *Classifier) applyCached(ev *model.Event, live bool) bool {
	return c.applyCachedText(ev, textKey(ev), live)
}

func (c *Classifier) applyCachedText(ev *model.Event, text string, live bool) bool {
	c.mu.Lock()
	key := c.keyLocked(ev, text)
	r, ok := c.cache[key]
	s := c.settings
	c.mu.Unlock()
	if !ok || time.Since(r.at) > cacheTTL {
		return false
	}
	c.apply(ev, r.Result, s, live)
	return true
}

// textKey identifies everything the engine reads about ev: the state
// buildState sends, with numbers, ids and times normalized as in message
// patterns. Events of one source that share it get the same answer.
func textKey(ev *model.Event) string {
	sum := sha1.Sum([]byte(classify.Normalize(buildState(ev))))
	return hex.EncodeToString(sum[:10])
}

// keyLocked identifies the answer for ev: its source, its text (from
// textKey), the checkpoint and whether severity is asked. Answers aren't
// shared between sources: the normalized text can differ in what the engine
// reads (numbers, case), so one source's events must not decide another's.
// Caller holds mu.
func (c *Classifier) keyLocked(ev *model.Event, text string) string {
	sev := "c"
	if c.asksSeverity(ev) {
		sev = "cs"
	}
	return ev.SourceID + "\x00" + c.modelLocked(ev) + "\x00" + sev + "\x00" + text
}

func (c *Classifier) modelLocked(ev *model.Event) string {
	if m := c.sourceModels[ev.SourceID]; m != "" {
		return m
	}
	return c.settings.Model
}

// Enrich classifies a live event (from cache, or by asking the engine) and
// applies the answer. Failures leave ev as the rules classified it.
func (c *Classifier) Enrich(ctx context.Context, ev *model.Event) { c.enrich(ctx, ev, true) }

// EnrichFile is Enrich for file analysis: requests count as engine calls,
// but cache hits and applied answers are not counted as live events.
func (c *Classifier) EnrichFile(ctx context.Context, ev *model.Event) { c.enrich(ctx, ev, false) }

func (c *Classifier) enrich(ctx context.Context, ev *model.Event, live bool) {
	text := textKey(ev)
	if c.applyCachedText(ev, text, live) {
		if live {
			c.CacheHits.Add(1)
		}
		return
	}
	r, coalesced, err := c.classify(ctx, ev, text)
	switch {
	case err == nil:
		if coalesced && live {
			c.CacheHits.Add(1)
		}
		c.mu.Lock()
		s := c.settings
		c.mu.Unlock()
		c.apply(ev, r, s, live)
	case errors.Is(err, ErrPaused), errors.Is(err, ErrNotLoaded), errors.Is(err, errNoSlot):
		if live {
			c.Skipped.Add(1)
		}
	case ctx.Err() != nil:
		c.TimedOut.Add(1)
	case coalesced:
		// The request this event shared has logged its failure.
	default:
		c.log.Warn("laya engine classification failed", "err", err)
	}
}

// Classify asks the engine about ev right now, for testing from the UI:
// no cache, pause or counter is read or changed, and ev is not modified.
func (c *Classifier) Classify(ctx context.Context, ev *model.Event) (Result, error) {
	c.mu.Lock()
	client, sem := c.client, c.sem
	cfgErr := c.configErr
	checkpoint := c.modelLocked(ev)
	req := buildRequest(ev, c.categories, checkpoint, c.asksSeverity(ev))
	req.Strict = checkpoint != c.settings.Model
	var notLoaded error
	if c.loaded != nil && !c.loaded[checkpoint] || c.loaded == nil && checkpoint != c.settings.Model {
		notLoaded = c.notLoadedErrLocked(checkpoint)
	}
	c.mu.Unlock()
	if client == nil {
		if cfgErr == "" {
			cfgErr = "no engine URL is configured"
		}
		return Result{}, errors.New(cfgErr)
	}
	if notLoaded != nil {
		return Result{}, notLoaded // asking would make the engine load it
	}
	if len(req.Questions) == 0 {
		return Result{}, errors.New("nothing to ask: no categories are configured, and the severity isn't asked (set_severity is off, or the record states its level)")
	}
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-sem }()
	resp, err := client.Decide(ctx, req)
	if err != nil {
		return Result{}, err
	}
	return parseResult(resp, req)
}

// classify asks the engine about ev, coalescing concurrent requests for the
// same answer. coalesced reports that another request's answer was used.
func (c *Classifier) classify(ctx context.Context, ev *model.Event, text string) (r Result, coalesced bool, err error) {
	for {
		c.mu.Lock()
		if c.client == nil {
			c.mu.Unlock()
			return Result{}, false, errors.New("no engine URL is configured")
		}
		checkpoint := c.modelLocked(ev)
		if err := c.availableLocked(checkpoint); err != nil {
			c.mu.Unlock()
			return Result{}, false, err
		}
		key := c.keyLocked(ev, text)
		if hit, ok := c.cache[key]; ok && time.Since(hit.at) <= cacheTTL {
			c.mu.Unlock() // answered while this event waited for the lock
			return hit.Result, true, nil
		}
		fkey := c.flightKeyLocked(ev, text)
		if f, ok := c.flights[fkey]; ok {
			c.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return Result{}, false, ctx.Err()
			}
			// The first event may have run out of its own time; this one
			// still has time, so it asks itself.
			if ctx.Err() == nil && (errors.Is(f.err, errNoSlot) || errors.Is(f.err, context.DeadlineExceeded) || errors.Is(f.err, context.Canceled)) {
				continue
			}
			return f.res, true, f.err
		}
		f := &flight{done: make(chan struct{})}
		c.flights[fkey] = f
		client, sem, gen := c.client, c.sem, c.cacheGen
		req := buildRequest(ev, c.categories, checkpoint, c.asksSeverity(ev))
		req.Strict = checkpoint != c.settings.Model
		c.mu.Unlock()

		f.res, f.err = c.call(ctx, client, sem, req)
		c.mu.Lock()
		delete(c.flights, fkey)
		// Don't store an answer to questions a reconfiguration replaced.
		if f.err == nil && gen == c.cacheGen {
			if len(c.cache) >= cacheSize {
				for k := range c.cache { // drop an arbitrary entry
					delete(c.cache, k)
					break
				}
			}
			c.cache[key] = cached{Result: f.res, at: time.Now()}
		}
		c.mu.Unlock()
		close(f.done)
		return f.res, false, f.err
	}
}

func (c *Classifier) call(ctx context.Context, client *Client, sem chan struct{}, req Request) (Result, error) {
	c.waiting.Add(1)
	select {
	case sem <- struct{}{}:
		c.waiting.Add(-1)
	case <-ctx.Done():
		c.waiting.Add(-1)
		return Result{}, errNoSlot
	}
	defer func() { <-sem }()
	// The engine may have been paused while this event waited for a slot,
	// and an event with too little time left would only time out.
	c.mu.Lock()
	err := c.availableLocked(req.Model)
	per := time.Duration(c.avgAttempt * float64(time.Millisecond))
	c.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	deadline, hasDeadline := ctx.Deadline()
	budget := time.Until(deadline)
	if hasDeadline && per > 0 && budget < 2*per {
		return Result{}, errNoSlot
	}
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)

	c.Calls.Add(1)
	resp, err := client.Decide(ctx, req)
	if errors.Is(err, ErrNotLoaded) {
		return Result{}, err // no instance has it (any more): not an engine failure
	}
	if err != nil && ctx.Err() != nil {
		// The event's time ran out: the engine is slow or busy, not
		// necessarily failing. Only a request that started with ample
		// time counts toward a pause.
		c.noteTimeout(!hasDeadline || budget >= max(ampleTime, 3*per))
		return Result{}, err
	}
	if err == nil {
		var r Result
		if r, err = parseResult(resp, req); err == nil {
			c.noteSuccess(resp)
			if r.Truncated {
				c.Truncated.Add(1)
			}
			if o, ok := resp.Usage.Options[qCategory]; ok && o.Distinct < o.Total {
				c.mu.Lock()
				c.optionsWarn = fmt.Sprintf("the engine could only tell %d of %d categories apart (options cut to %d tokens each): shorten category names and descriptions, or use fewer categories", o.Distinct, o.Total, o.TokensPerOption)
				c.mu.Unlock()
			}
			return r, nil
		}
	}
	c.Errors.Add(1)
	c.noteError(ctx, req.Model, err)
	return Result{}, err
}

// noteError records err and decides whether to pause. Only failures the
// next event would hit too pause calls: a checkpoint the engine doesn't
// serve pauses that checkpoint; a rejected key or wrong URL pauses all
// calls; an unreachable or failing engine backs off; requests running out
// of time pause only when several in a row do (a busy engine still answers
// some). A busy reply (503) is back-pressure, not failure.
func (c *Classifier) noteError(ctx context.Context, checkpoint string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.lastErr, c.lastErrAt, c.healthErr = err.Error(), now, false
	var apiErr *APIError
	var netErr net.Error
	switch {
	case errors.Is(err, ErrWrongModel):
		c.modelPause[checkpoint] = now.Add(configPause)
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound):
		c.pausedUntil, c.connPause = now.Add(configPause), false
		c.authPause = apiErr.Status != http.StatusNotFound
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable:
	case errors.As(err, &apiErr) && apiErr.Status < 500:
		// This event's input; the next one may be fine.
	case errors.As(err, &netErr) && netErr.Timeout():
		if c.timeouts++; c.timeouts >= timeoutsToPause {
			c.backoffLocked(now)
		}
	default: // unreachable or failing
		c.backoffLocked(now)
	}
}

// noteTimeout records a request that ran out of the event's time. ample
// says it started with enough time that a working engine would have
// answered; three such in a row pause calls.
func (c *Classifier) noteTimeout(ample bool) {
	if !ample {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timeouts++; c.timeouts >= timeoutsToPause {
		c.lastErr, c.lastErrAt, c.healthErr = fmt.Sprintf("%d requests in a row got no answer in time", c.timeouts), time.Now(), false
		c.backoffLocked(time.Now())
	}
}

// backoffLocked pauses calls with exponential backoff. Errors that arrive
// together (from requests already in flight) count once.
func (c *Classifier) backoffLocked(now time.Time) {
	if now.Before(c.pausedUntil) {
		return
	}
	c.failures++
	c.pausedUntil = now.Add(min(5*time.Second<<min(c.failures-1, 5), 2*time.Minute))
	c.connPause = true
}

func (c *Classifier) noteSuccess(resp Response) {
	ms := float64(resp.Elapsed) / float64(time.Millisecond)
	infer := float64(resp.Inference) / float64(time.Millisecond)
	c.LatencyMsTotal.Add(uint64(max(ms, 0)))
	c.InferMsTotal.Add(uint64(max(infer, 0)))
	c.Completed.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures, c.timeouts = 0, 0
	c.lastOK = time.Now()
	c.avgLatency = ewma(c.avgLatency, ms)
	c.avgAttempt = ewma(c.avgAttempt, float64(resp.Attempt)/float64(time.Millisecond))
	c.avgInfer = ewma(c.avgInfer, infer)
}

func ewma(avg, v float64) float64 {
	if avg == 0 {
		return max(v, 0.001)
	}
	return avg*0.8 + v*0.2
}

// apply merges r into ev. Rules stay authoritative: the engine only fills
// in a missing category, and sets a severity only when it was guessed from
// keywords; answers below the confidence threshold are recorded but not
// used. live counts the outcome in the live metrics.
func (c *Classifier) apply(ev *model.Event, r Result, s config.EngineSettings, live bool) {
	info := &model.EngineResult{
		Model:              r.Model,
		Category:           r.Category,
		Confidence:         r.CategoryConfidence,
		Top:                top(r.Probabilities, 3),
		Severity:           r.Severity,
		SeverityConfidence: r.SeverityConfidence,
		Threshold:          s.MinConfidence,
		Truncated:          r.Truncated,
	}
	if ev.Category == model.Uncategorized && r.Category != "" && r.Category != model.Uncategorized {
		if r.CategoryConfidence >= s.MinConfidence {
			ev.Category = r.Category
			info.Applied, info.CategoryApplied = true, true
		} else if live {
			c.LowConfidence.Add(1)
		}
	}
	if r.Severity.Valid() && r.Severity != ev.Severity && s.SetSeverity && ev.SeverityGuessed && r.SeverityConfidence >= s.MinConfidence {
		ev.Severity = r.Severity
		info.Applied, info.SeverityApplied = true, true
	}
	if !r.CategoryAsked {
		info.Category = "" // only the severity was asked
	}
	if info.CategoryApplied {
		ev.Classifier = ByEngine // the category is the engine's
	}
	if info.Applied {
		if live {
			c.Applied.Add(1)
		}
	}
	ev.Engine = info
}

// top returns the n most likely labels.
func top(p map[string]float64, n int) map[string]float64 {
	if len(p) == 0 {
		return nil
	}
	keys := slices.Collect(maps.Keys(p))
	sort.Slice(keys, func(i, j int) bool {
		if p[keys[i]] != p[keys[j]] {
			return p[keys[i]] > p[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make(map[string]float64, n)
	for _, k := range keys[:min(n, len(keys))] {
		out[k] = p[k]
	}
	return out
}

// parseResult reads the answers to req's questions.
func parseResult(resp Response, req Request) (Result, error) {
	r := Result{Model: resp.Routing.Model, Truncated: resp.Usage.Truncated,
		LatencyMs: resp.Elapsed.Milliseconds(), InferenceMs: resp.Inference.Milliseconds()}
	if _, ok := req.Questions[qCategory]; ok {
		a, ok := resp.Answers[qCategory]
		if !ok {
			return Result{}, errors.New("laya engine: reply has no category answer")
		}
		// Only what was offered is accepted, and only its probabilities are
		// kept (they are cached).
		offered, _ := req.Questions[qCategory].Criteria.(criteria)
		choice, _ := a.Choice.(string)
		if !slices.ContainsFunc(offered, func(l label) bool { return l.name == choice }) {
			return Result{}, fmt.Errorf("laya engine: category answer %q was not offered", model.Truncate(choice, 64))
		}
		if !(a.AnswerConfidence >= 0 && a.AnswerConfidence <= 1) {
			return Result{}, fmt.Errorf("laya engine: category confidence %v is not a probability", a.AnswerConfidence)
		}
		probs := make(map[string]float64, len(offered))
		for _, l := range offered {
			if p, ok := a.Probabilities[l.name]; ok {
				if !(p >= 0 && p <= 1) {
					return Result{}, fmt.Errorf("laya engine: probability %v for %q is not a probability", p, l.name)
				}
				probs[l.name] = p
			}
		}
		r.Category, r.CategoryConfidence, r.Probabilities = choice, a.AnswerConfidence, probs
		r.CategoryAsked = true
	} else {
		r.Category = model.Uncategorized
	}
	if _, ok := req.Questions[qSeverity]; ok {
		a, ok := resp.Answers[qSeverity]
		if !ok {
			return Result{}, errors.New("laya engine: reply has no severity answer")
		}
		// The score is an expected level; take the most likely level instead.
		best, bestP := -1, -1.0
		for k, p := range a.Probabilities {
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(model.Severities) || !(p >= 0 && p <= 1) {
				continue
			}
			if p > bestP || p == bestP && i < best {
				best, bestP = i, p
			}
		}
		if best >= 0 {
			r.Severity, r.SeverityConfidence = model.Severities[best], bestP
		}
	}
	return r, nil
}

// EndpointHealth describes one laya-serve instance.
type EndpointHealth struct {
	URL         string   `json:"url"`
	OK          bool     `json:"ok"`
	Error       string   `json:"error,omitempty"`
	Loaded      []string `json:"loaded,omitempty"`
	Missing     []string `json:"missing,omitempty"` // checkpoints in use that it hasn't loaded
	Device      string   `json:"device,omitempty"`
	Revision    string   `json:"revision,omitempty"`
	ModelLoaded bool     `json:"model_loaded"` // the default checkpoint is resident
	LatencyMs   int64    `json:"latency_ms"`
}

// Probe checks every configured instance's /health against the checkpoints
// in use (the default and any per-source ones).
func (c *Classifier) Probe(ctx context.Context) ([]EndpointHealth, error) {
	c.mu.Lock()
	client, s, cfgErr := c.client, c.settings, c.configErr
	inUse := c.modelsInUseLocked()
	c.mu.Unlock()
	if client == nil {
		if cfgErr == "" {
			cfgErr = "no engine URL is configured"
		}
		return nil, errors.New(cfgErr)
	}
	out := make([]EndpointHealth, len(client.URLs()))
	var wg sync.WaitGroup
	for i, base := range client.URLs() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			h, err := client.Health(ctx, base)
			eh := EndpointHealth{URL: redactURL(base), LatencyMs: time.Since(start).Milliseconds()}
			switch {
			case err != nil:
				eh.Error = err.Error()
			case h.Loaded == nil:
				// laya-serve shows its checkpoints only to callers with its key.
				eh.Error = "the engine did not list its checkpoints: check the API key"
			default:
				eh.OK = true
				eh.Loaded, eh.Device = h.Loaded, h.Device
				eh.Revision = h.Revisions[s.Model]
				for _, m := range inUse {
					if !slices.Contains(h.Loaded, m) {
						eh.Missing = append(eh.Missing, m)
					}
				}
				eh.ModelLoaded = slices.Contains(h.Loaded, s.Model)
			}
			out[i] = eh
		}()
	}
	wg.Wait()
	return out, nil
}

func (c *Classifier) modelsInUseLocked() []string {
	in := []string{c.settings.Model}
	for _, m := range c.sourceModels {
		if !slices.Contains(in, m) {
			in = append(in, m)
		}
	}
	sort.Strings(in[1:])
	return in
}

// Status returns counters and state for the UI.
func (c *Classifier) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		Enabled:       c.settings.Enabled,
		Configured:    c.client != nil,
		ConfigError:   c.configErr,
		URLs:          []string{},
		Model:         c.settings.Model,
		ModelsInUse:   c.modelsInUseLocked(),
		Capacity:      cap(c.sem),
		Calls:         c.Calls.Load(),
		CacheHits:     c.CacheHits.Load(),
		Errors:        c.Errors.Load(),
		TimedOut:      c.TimedOut.Load(),
		Applied:       c.Applied.Load(),
		LowConfidence: c.LowConfidence.Load(),
		Truncated:     c.Truncated.Load(),
		Skipped:       c.Skipped.Load(),
		AvgLatencyMs:  int64(c.avgLatency),
		AvgInferMs:    int64(c.avgInfer),
		InFlight:      int(c.inFlight.Load()),
		CachedResults: len(c.cache),
		LastError:     c.lastErr,
		LastErrorAt:   c.lastErrAt,
		LastSuccessAt: c.lastOK,
	}
	for _, u := range c.urls {
		st.URLs = append(st.URLs, redactURL(u))
	}
	if c.loaded != nil {
		st.Loaded = slices.Sorted(maps.Keys(c.loaded))
		for _, m := range st.ModelsInUse {
			if !c.loaded[m] {
				st.Warnings = append(st.Warnings, c.notLoadedErrLocked(m).Error()+": events that need it go on with the rules' result")
			}
		}
	}
	now := time.Now()
	for _, m := range slices.Sorted(maps.Keys(c.modelPause)) {
		if until := c.modelPause[m]; now.Before(until) {
			st.PausedCheckpoints = append(st.PausedCheckpoints, m)
			st.Warnings = append(st.Warnings, fmt.Sprintf("checkpoint %q is paused until %s: the engine answered with a different checkpoint", m, until.Format(time.TimeOnly)))
		}
	}
	if c.optionsWarn != "" {
		st.Warnings = append(st.Warnings, c.optionsWarn)
	}
	if c.client != nil {
		for _, u := range c.client.Hidden() {
			st.Warnings = append(st.Warnings, fmt.Sprintf("%s answers but doesn't list its checkpoints to Lissandra's key, as the other instances do: its key differs, so it isn't asked", redactURL(u)))
		}
	}
	if now.Before(c.pausedUntil) {
		st.PausedUntil = c.pausedUntil
	}
	return st
}

// notLoadedErrLocked explains that checkpoint isn't loaded. Caller holds mu.
func (c *Classifier) notLoadedErrLocked(checkpoint string) error {
	if c.loaded == nil {
		return fmt.Errorf("%w: %q (the engine hasn't reported its checkpoints yet)", ErrNotLoaded, checkpoint)
	}
	has := slices.Sorted(maps.Keys(c.loaded))
	return fmt.Errorf("%w: %q (it has %s): check the name, or add it to the engine's LAYA_MODELS", ErrNotLoaded, checkpoint, strings.Join(has, ", "))
}

// redactURL hides credentials embedded in a URL.
func redactURL(raw string) string { return config.MaskURLs(raw) }
