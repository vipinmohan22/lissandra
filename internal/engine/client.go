// Package engine classifies log events with the classification engine:
// laya-serve from github.com/NandhaKishorM/laya, a decision model served
// over HTTP (POST /v1/systemone). Rules run first; the engine classifies
// what they leave, once per message pattern, and the rules' result stands
// whenever the engine is unavailable or unsure.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Question types understood by laya-serve.
const (
	TypeChoice = "choice" // pick one label; criteria maps label -> description
	TypeScore  = "score"  // place on an ordered scale; criteria lists the levels
	TypeNoul   = "noul"   // yes/no; the answer is P(true)
)

// Question is one typed question about the state.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Request is the body of POST /v1/systemone.
type Request struct {
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
	// Model pins the checkpoint. It is always sent: without it laya-serve
	// routes each text by detected language and can load a second model.
	Model string `json:"model"`
	// Strict sends only to instances known to have Model loaded (asking
	// one that hasn't would make it load the checkpoint). Otherwise an
	// instance that hasn't said what it has may be asked too.
	Strict bool `json:"-"`
}

// Answer is one question's answer. Probabilities are keyed by label for
// choice questions and by level index ("0", "1", …) for score questions.
type Answer struct {
	Type             string             `json:"type"`
	Choice           any                `json:"choice,omitempty"`
	Score            *float64           `json:"score,omitempty"` // expected level index
	Noul             *float64           `json:"noul,omitempty"`  // P(true)
	Probabilities    map[string]float64 `json:"probabilities,omitempty"`
	AnswerConfidence float64            `json:"answer_confidence"` // max probability; gate on this
}

// Usage reports tokens read and whether the state was cut to fit.
type Usage struct {
	InputTokens        int      `json:"input_tokens"`
	StateTokens        int      `json:"state_tokens"`
	StateTokensDropped int      `json:"state_tokens_dropped"`
	Truncated          bool     `json:"truncated"`
	TruncatedQuestions []string `json:"truncated_questions,omitempty"`
	// Options is reported for questions whose options had to be cut to fit
	// the model's option budget; Distinct < Total means some options became
	// indistinguishable.
	Options map[string]OptionUsage `json:"options,omitempty"`
}

// OptionUsage describes how a question's options were cut.
type OptionUsage struct {
	Total           int `json:"total"`
	Distinct        int `json:"distinct"`
	TokensPerOption int `json:"tokens_per_option"`
}

// Routing names the checkpoint that answered.
type Routing struct {
	Model  string `json:"model"`
	Repo   string `json:"repo,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Response is laya-serve's answer to a Request.
type Response struct {
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Routing Routing           `json:"routing"`
	// Measured by the client (round trip) and reported by the engine
	// (X-Inference-Time-Ms, the forward pass alone).
	Elapsed   time.Duration `json:"-"`
	Attempt   time.Duration `json:"-"` // the answering request alone, without retries or waits
	Inference time.Duration `json:"-"`
	Endpoint  string        `json:"-"`
}

// Health is laya-serve's GET /health. Loaded and Device are only reported
// to callers with the API key when one is set.
type Health struct {
	Status    string            `json:"status"`
	Loaded    []string          `json:"loaded"`
	Revisions map[string]string `json:"revisions,omitempty"`
	Device    string            `json:"device,omitempty"`
}

// APIError is a non-2xx reply from laya-serve; Detail is its
// {"detail": "..."} message.
type APIError struct {
	Status     int
	Detail     string
	RetryAfter time.Duration // for 503: when to try again
}

func (e *APIError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("engine: HTTP %d", e.Status)
	}
	return fmt.Sprintf("engine: HTTP %d: %s", e.Status, e.Detail)
}

// ErrWrongModel is returned when the engine answered with a checkpoint other
// than the one requested; laya-serve does this silently for names it does
// not know.
var ErrWrongModel = errors.New("the engine answered with a different checkpoint")

// Client calls one or more laya-serve instances, round-robin with failover.
// It is safe for concurrent use.
type Client struct {
	urls   []string
	key    string
	http   *http.Client
	next   atomic.Uint32
	health atomic.Pointer[instances]
}

// instances is what the last health check learned: which instances answered
// and, for those that said, which checkpoints each has loaded.
type instances struct {
	ready  map[string]bool
	models map[string]map[string]bool // nil entry: the instance didn't say
}

// maxResponseBytes bounds a reply; real ones are a few KB.
const maxResponseBytes = 4 << 20

// NewClient returns a client for the given base URLs.
func NewClient(urls []string, apiKey string, timeout time.Duration, maxConns int) *Client {
	tr := &http.Transport{
		// No proxy: the engine runs next to Lissandra, and HTTP(S)_PROXY settings
		// meant for outbound traffic (notifications) would capture it.
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: maxConns,
		// uvicorn closes idle keep-alive connections after 5s; reusing one
		// it is closing fails a POST that is not retried automatically.
		IdleConnTimeout:       4 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &Client{urls: urls, key: apiKey, http: &http.Client{Transport: tr, Timeout: timeout}}
}

// URLs returns the configured base URLs.
func (c *Client) URLs() []string { return c.urls }

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Decide sends req and returns the answers. Each instance that could
// answer is tried at most once, ready ones that have the checkpoint first:
// connection errors, 5xx and an answer from another checkpoint move on to
// the next one. A 503 (engine busy) is retried after its Retry-After for as
// long as ctx allows. A response timeout or a 4xx is returned at once:
// retrying would only add to the engine's queue or fail again.
func (c *Client) Decide(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	order := c.order(req.Model, req.Strict)
	if len(order) == 0 {
		return Response{}, fmt.Errorf("%w: %q on any instance", ErrNotLoaded, req.Model)
	}
	start := time.Now()
	var last error
	for i, busy := 0, 0; i < len(order) && busy < 30; {
		base := order[i]
		attempt := time.Now()
		resp, err := c.post(ctx, base, body)
		if err == nil {
			resp.Elapsed, resp.Attempt, resp.Endpoint = time.Since(start), time.Since(attempt), base
			if want := req.Model; want != "" && resp.Routing.Model != want {
				c.forget(base, want)
				last = fmt.Errorf("%w: asked %s for %q, got %q (is the checkpoint configured on the engine?)", ErrWrongModel, redactURL(base), want, resp.Routing.Model)
				i++
				continue
			}
			return resp, nil
		}
		last = err
		var apiErr *APIError
		var netErr net.Error
		var opErr *net.OpError
		switch {
		case ctx.Err() != nil:
			return Response{}, err
		case errors.As(err, &opErr) && opErr.Op == "dial":
			i++ // unreachable (refused, or no answer to connect): next instance
		case errors.As(err, &netErr) && netErr.Timeout():
			return Response{}, err
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable:
			busy++
			if !sleep(ctx, apiErr.RetryAfter) {
				return Response{}, err
			}
		case errors.As(err, &apiErr) && apiErr.Status < 500:
			return Response{}, err
		default:
			i++
		}
	}
	return Response{}, last
}

// SetHealth records what the last health check learned.
func (c *Client) SetHealth(ready map[string]bool, models map[string]map[string]bool) {
	c.health.Store(&instances{ready: ready, models: models})
}

// SetReady records which instances passed their last health check.
func (c *Client) SetReady(ready map[string]bool) { c.SetHealth(ready, nil) }

// order lists the instances to try for checkpoint, best first: ready ones
// that have it loaded (or didn't say, unless strict), round-robin; then
// the others that may have it, in case one came up since the last check.
func (c *Client) order(checkpoint string, strict bool) []string {
	return c.candidates(checkpoint, strict, int(c.next.Add(1)-1))
}

// Has reports whether some instance may answer for checkpoint.
func (c *Client) Has(checkpoint string, strict bool) bool {
	return len(c.candidates(checkpoint, strict, 0)) > 0
}

func (c *Client) candidates(checkpoint string, strict bool, start int) []string {
	n := len(c.urls)
	h := c.health.Load()
	may := func(u string) bool {
		if h == nil || h.models == nil {
			return !strict
		}
		m, said := h.models[u]
		if !said || m == nil {
			return !strict
		}
		return m[checkpoint]
	}
	// An instance that answers but hides its checkpoints while another
	// lists them has a different key (one still on the old key while the
	// key rotates): asking it would only get a 401.
	listed := false
	if h != nil {
		for u := range h.ready {
			listed = listed || h.models[u] != nil
		}
	}
	var ready, rest []string
	for i := 0; i < n; i++ {
		u := c.urls[(start+i)%n]
		switch {
		case !may(u):
		case h != nil && h.ready[u] && listed && h.models[u] == nil:
		case h != nil && h.ready[u]:
			ready = append(ready, u)
		default:
			rest = append(rest, u)
		}
	}
	return append(ready, rest...)
}

// Hidden lists the instances that answer their health check but hide their
// checkpoints while another lists them (a different key): candidates skips
// them.
func (c *Client) Hidden() []string {
	h := c.health.Load()
	if h == nil {
		return nil
	}
	listed := false
	for u := range h.ready {
		listed = listed || h.models[u] != nil
	}
	var out []string
	for _, u := range c.urls {
		if listed && h.ready[u] && h.models[u] == nil {
			out = append(out, u)
		}
	}
	return out
}

// forget records that base answered checkpoint with another one.
func (c *Client) forget(base, checkpoint string) {
	h := c.health.Load()
	if h == nil {
		return
	}
	models := maps.Clone(h.models)
	if models == nil {
		models = map[string]map[string]bool{}
	}
	m := maps.Clone(models[base])
	if m == nil {
		m = map[string]bool{}
	}
	delete(m, checkpoint)
	models[base] = m
	c.health.Store(&instances{ready: h.ready, models: models})
}

func (c *Client) post(ctx context.Context, base string, body []byte) (Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Response{}, hideCreds(err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.key)
	}
	res, err := c.http.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("engine at %s: %w", redactURL(base), hideCreds(err))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return Response{}, fmt.Errorf("engine at %s: reading reply: %w", redactURL(base), err)
	}
	if res.StatusCode != http.StatusOK {
		return Response{}, apiError(res, data)
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return Response{}, fmt.Errorf("engine at %s: invalid reply: %w", redactURL(base), err)
	}
	if ms, err := strconv.ParseFloat(res.Header.Get("X-Inference-Time-Ms"), 64); err == nil {
		out.Inference = time.Duration(ms * float64(time.Millisecond))
	}
	return out, nil
}

func apiError(res *http.Response, data []byte) error {
	var body struct {
		Detail any `json:"detail"`
	}
	detail := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &body) == nil && body.Detail != nil {
		if s, ok := body.Detail.(string); ok {
			detail = s
		} else if b, err := json.Marshal(body.Detail); err == nil {
			detail = string(b) // FastAPI's own validation errors are lists
		}
	}
	if len(detail) > 500 {
		detail = detail[:500] + "…"
	}
	e := &APIError{Status: res.StatusCode, Detail: detail}
	if res.StatusCode == http.StatusServiceUnavailable {
		secs, _ := strconv.Atoi(res.Header.Get("Retry-After"))
		e.RetryAfter = time.Duration(min(max(secs, 1), 5)) * time.Second
	}
	return e
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Health calls GET /health on base.
func (c *Client) Health(ctx context.Context, base string) (Health, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return Health{}, hideCreds(err)
	}
	if c.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.key)
	}
	res, err := c.http.Do(hreq)
	if err != nil {
		return Health{}, fmt.Errorf("engine at %s: %w", redactURL(base), hideCreds(err))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return Health{}, err
	}
	if res.StatusCode != http.StatusOK {
		return Health{}, apiError(res, data)
	}
	var h Health
	if err := json.Unmarshal(data, &h); err != nil {
		return Health{}, fmt.Errorf("engine at %s: invalid health reply: %w", redactURL(base), err)
	}
	return h, nil
}

// hideCreds hides credentials in the URL of an HTTP client error: net/http
// hides a password, but not a user alone, which is often a token.
func hideCreds(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactURL(ue.URL)
	}
	return err
}
