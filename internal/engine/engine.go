package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

const (
	cacheSize = 5000
	cacheTTL  = 24 * time.Hour

	// ByEngine is Event.Classifier when an engine answer was applied.
	ByEngine = "laya"

	// questions sent to the engine
	qCategory = "category"
	qSeverity = "severity"
)

// Result is the engine's classification of one event.
type Result struct {
	Model              string             `json:"model"`
	Category           string             `json:"category"` // model.Uncategorized when none fits
	CategoryConfidence float64            `json:"category_confidence"`
	Probabilities      map[string]float64 `json:"probabilities,omitempty"`
	Severity           model.Severity     `json:"severity,omitempty"` // only when asked
	SeverityConfidence float64            `json:"severity_confidence,omitempty"`
	Truncated          bool               `json:"truncated,omitempty"`
	LatencyMs          int64              `json:"latency_ms"`
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
	Calls         uint64    `json:"calls_total"`
	CacheHits     uint64    `json:"cache_hits"`
	Errors        uint64    `json:"errors"`
	Applied       uint64    `json:"applied"`        // answers that set a category or severity
	LowConfidence uint64    `json:"low_confidence"` // answers below min_confidence
	Truncated     uint64    `json:"truncated"`      // states cut to fit the model
	Skipped       uint64    `json:"skipped"`        // events not sent: engine busy or paused
	AvgLatencyMs  int64     `json:"avg_latency_ms"`
	InFlight      int       `json:"in_flight"`
	CachedResults int       `json:"cached_results"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
	PausedUntil   time.Time `json:"paused_until,omitzero"`
}

// Classifier classifies events with laya-serve. All methods are safe for
// concurrent use.
type Classifier struct {
	log *slog.Logger

	mu           sync.Mutex
	settings     config.EngineSettings
	categories   []config.Category
	sourceModels map[string]string
	client       *Client
	clientKey    string
	configErr    string
	sem          chan struct{}
	cache        map[string]cached
	flights      map[string]*flight
	lastErr      string
	lastErrAt    time.Time
	pausedUntil  time.Time
	failures     int

	Calls, CacheHits, Errors, Applied, LowConfidence, Truncated, Skipped atomic.Uint64
	latencyMs, latencyN                                                  atomic.Uint64
	inFlight                                                             atomic.Int64
}

// New returns an unconfigured classifier; call Configure.
func New(log *slog.Logger) *Classifier {
	return &Classifier{log: log, cache: map[string]cached{}, flights: map[string]*flight{}, sem: make(chan struct{}, 1)}
}

// Configure applies c. Changing the URL, key, checkpoints or categories
// clears cached answers.
func (c *Classifier) Configure(cfg *config.Config) {
	s := cfg.Settings.Engine
	urls, urlErr := config.ParseEngineURLs(config.ExpandEnv(s.URL))
	key := config.ExpandEnv(s.APIKey)
	models := map[string]string{}
	for _, src := range cfg.Sources {
		if src.EngineModel != "" {
			models[src.ID] = src.EngineModel
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Model != c.settings.Model || !maps.Equal(models, c.sourceModels) || !slices.Equal(cfg.Settings.Categories, c.categories) {
		c.cache = map[string]cached{}
	}
	clientKey := fmt.Sprint(urls, "\x00", key, "\x00", s.TimeoutSeconds, "\x00", s.MaxInFlight)
	if clientKey != c.clientKey {
		if c.client != nil {
			c.client.Close()
		}
		c.client, c.clientKey = nil, clientKey
		if len(urls) > 0 {
			c.client = NewClient(urls, key, time.Duration(s.TimeoutSeconds)*time.Second, s.MaxInFlight)
		}
		c.cache = map[string]cached{}
		c.pausedUntil, c.failures = time.Time{}, 0
	}
	if cap(c.sem) != s.MaxInFlight && s.MaxInFlight > 0 {
		c.sem = make(chan struct{}, s.MaxInFlight)
	}
	c.configErr = ""
	if urlErr != nil {
		c.configErr = "engine url: " + urlErr.Error()
	}
	c.settings = s
	c.categories = slices.Clone(cfg.Settings.Categories)
	c.sourceModels = models
}

// Eligible reports whether ev should be classified by the engine under the
// current settings.
func (c *Classifier) Eligible(ev *model.Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.settings
	if !s.Enabled || c.client == nil || !ev.Severity.AtLeast(s.MinSeverity) {
		return false
	}
	if s.OnlyUncategorized && ev.Category != model.Uncategorized {
		return false
	}
	return len(c.categories) > 0 || c.asksSeverity(ev)
}

// asksSeverity reports whether the engine should also judge ev's severity:
// only when neither the log record nor a rule set it. Caller holds mu.
func (c *Classifier) asksSeverity(ev *model.Event) bool {
	return c.settings.SetSeverity && ev.Level == "" && len(ev.MatchedRules) == 0
}

// MaxWait is how long an event may wait for an answer, queueing included,
// before it goes on with the rules' classification.
func (c *Classifier) MaxWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Duration(max(c.settings.TimeoutSeconds, 1)) * time.Second
}

// Available reports whether calls may be made now (not paused after
// errors).
func (c *Classifier) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().After(c.pausedUntil)
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

func (c *Classifier) applyCached(ev *model.Event, count bool) bool {
	c.mu.Lock()
	key := c.keyLocked(ev)
	r, ok := c.cache[key]
	s := c.settings
	c.mu.Unlock()
	if !ok || time.Since(r.at) > cacheTTL {
		return false
	}
	c.apply(ev, r.Result, s, count)
	return true
}

// keyLocked identifies the answer for ev: its pattern, the checkpoint and
// whether severity is asked. Caller holds mu.
func (c *Classifier) keyLocked(ev *model.Event) string {
	sev := "c"
	if c.asksSeverity(ev) {
		sev = "cs"
	}
	return c.modelLocked(ev) + "\x00" + sev + "\x00" + ev.Fingerprint
}

func (c *Classifier) modelLocked(ev *model.Event) string {
	if m := c.sourceModels[ev.SourceID]; m != "" {
		return m
	}
	return c.settings.Model
}

// Enrich classifies ev (from cache, or by calling the engine) and applies
// the answer. Failures leave ev as the rules classified it.
func (c *Classifier) Enrich(ctx context.Context, ev *model.Event) {
	if c.ApplyCached(ev) {
		return
	}
	if !c.Available() {
		c.Skipped.Add(1)
		return
	}
	r, err := c.classify(ctx, ev, false)
	if err != nil {
		c.log.Warn("laya engine classification failed", "err", err)
		return
	}
	c.mu.Lock()
	s := c.settings
	c.mu.Unlock()
	c.apply(ev, r, s, true)
}

// Classify asks the engine about ev now, bypassing the cache and any pause
// (for testing from the UI). ev is not modified.
func (c *Classifier) Classify(ctx context.Context, ev *model.Event) (Result, error) {
	return c.classify(ctx, ev, true)
}

func (c *Classifier) classify(ctx context.Context, ev *model.Event, force bool) (Result, error) {
	c.mu.Lock()
	if c.client == nil {
		err := c.configErr
		c.mu.Unlock()
		if err == "" {
			err = "no engine URL is configured"
		}
		return Result{}, errors.New(err)
	}
	key := c.keyLocked(ev)
	f, joined := c.flights[key]
	if !force && joined {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	f = &flight{done: make(chan struct{})}
	if !force {
		c.flights[key] = f
	}
	client, sem, s := c.client, c.sem, c.settings
	req := buildRequest(ev, c.categories, c.modelLocked(ev), c.asksSeverity(ev))
	c.mu.Unlock()

	f.res, f.err = c.call(ctx, client, sem, req, s)
	c.mu.Lock()
	if !force {
		delete(c.flights, key)
	}
	if f.err == nil {
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
	return f.res, f.err
}

func (c *Classifier) call(ctx context.Context, client *Client, sem chan struct{}, req Request, s config.EngineSettings) (Result, error) {
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-sem }()
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)

	c.Calls.Add(1)
	resp, err := client.Decide(ctx, req)
	if err != nil {
		c.Errors.Add(1)
		c.noteError(err)
		return Result{}, err
	}
	c.noteSuccess()
	ms := resp.Elapsed.Milliseconds()
	c.latencyMs.Add(uint64(max(ms, 0)))
	c.latencyN.Add(1)
	r, err := parseResult(resp, req)
	if err != nil {
		c.Errors.Add(1)
		c.noteError(err)
		return Result{}, err
	}
	r.LatencyMs = ms
	if r.Truncated {
		c.Truncated.Add(1)
	}
	return r, nil
}

// noteError records err and pauses calls on errors that the next event
// would hit too, so a down or misconfigured engine doesn't hold up events.
func (c *Classifier) noteError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr, c.lastErrAt = err.Error(), time.Now()
	var apiErr *APIError
	switch {
	case errors.Is(err, ErrWrongModel),
		errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound):
		c.pausedUntil = time.Now().Add(5 * time.Minute) // configuration problem
	case errors.As(err, &apiErr) && apiErr.Status < 500 && apiErr.Status != http.StatusServiceUnavailable:
		// This event's input; the next one may be fine.
	default: // unreachable, overloaded or failing: back off exponentially
		c.failures++
		backoff := min(5*time.Second<<min(c.failures-1, 5), 2*time.Minute)
		c.pausedUntil = time.Now().Add(backoff)
	}
}

func (c *Classifier) noteSuccess() {
	c.mu.Lock()
	c.failures = 0
	c.mu.Unlock()
}

// apply merges r into ev. Rules stay authoritative: the engine only fills
// in a missing category, and sets severity only when neither the log record
// nor a rule did; answers below the confidence threshold are recorded but
// not used.
func (c *Classifier) apply(ev *model.Event, r Result, s config.EngineSettings, count bool) {
	info := &model.EngineResult{
		Model:      r.Model,
		Category:   r.Category,
		Confidence: r.CategoryConfidence,
		Top:        top(r.Probabilities, 3),
		Severity:   r.Severity,
		Truncated:  r.Truncated,
	}
	confident := r.CategoryConfidence >= s.MinConfidence
	if ev.Category == model.Uncategorized && r.Category != "" && r.Category != model.Uncategorized && confident {
		ev.Category = r.Category
		info.Applied = true
	}
	if r.Severity.Valid() && ev.Level == "" && len(ev.MatchedRules) == 0 && s.SetSeverity && r.SeverityConfidence >= s.MinConfidence {
		ev.Severity = r.Severity
		info.Applied = true
	}
	switch {
	case info.Applied:
		ev.Classifier = ByEngine
		if count {
			c.Applied.Add(1)
		}
	case !confident && count:
		c.LowConfidence.Add(1)
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
	r := Result{Model: resp.Routing.Model, Truncated: resp.Usage.Truncated}
	if _, ok := req.Questions[qCategory]; ok {
		a, ok := resp.Answers[qCategory]
		if !ok {
			return Result{}, errors.New("laya engine: reply has no category answer")
		}
		label, _ := a.Choice.(string)
		if label == "" {
			return Result{}, fmt.Errorf("laya engine: unexpected category answer %v", a.Choice)
		}
		r.Category, r.CategoryConfidence, r.Probabilities = label, a.AnswerConfidence, a.Probabilities
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
			if err != nil || i < 0 || i >= len(model.Severities) {
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
	Device      string   `json:"device,omitempty"`
	Revision    string   `json:"revision,omitempty"`
	ModelLoaded bool     `json:"model_loaded"` // the configured checkpoint is resident
	LatencyMs   int64    `json:"latency_ms"`
}

// Probe checks every configured instance's /health.
func (c *Classifier) Probe(ctx context.Context) ([]EndpointHealth, error) {
	c.mu.Lock()
	client, s, cfgErr := c.client, c.settings, c.configErr
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
			if err != nil {
				eh.Error = err.Error()
			} else {
				eh.OK = true
				eh.Loaded, eh.Device = h.Loaded, h.Device
				eh.Revision = h.Revisions[s.Model]
				eh.ModelLoaded = slices.Contains(h.Loaded, s.Model)
			}
			out[i] = eh
		}()
	}
	wg.Wait()
	return out, nil
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
		Calls:         c.Calls.Load(),
		CacheHits:     c.CacheHits.Load(),
		Errors:        c.Errors.Load(),
		Applied:       c.Applied.Load(),
		LowConfidence: c.LowConfidence.Load(),
		Truncated:     c.Truncated.Load(),
		Skipped:       c.Skipped.Load(),
		InFlight:      int(c.inFlight.Load()),
		CachedResults: len(c.cache),
		LastError:     c.lastErr,
		LastErrorAt:   c.lastErrAt,
	}
	if c.client != nil {
		for _, u := range c.client.URLs() {
			st.URLs = append(st.URLs, redactURL(u))
		}
	}
	if time.Now().Before(c.pausedUntil) {
		st.PausedUntil = c.pausedUntil
	}
	if n := c.latencyN.Load(); n > 0 {
		st.AvgLatencyMs = int64(c.latencyMs.Load() / n)
	}
	return st
}

// redactURL hides credentials embedded in a URL.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
}
