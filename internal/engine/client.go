// Package engine classifies log events with the Laya engine: laya-serve
// from github.com/NandhaKishorM/laya, a decision model served over HTTP
// (POST /v1/systemone). Rules run first; the engine classifies what they
// leave, once per message pattern, and the rules' result stands whenever
// the engine is unavailable or unsure.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
		return fmt.Sprintf("laya engine: HTTP %d", e.Status)
	}
	return fmt.Sprintf("laya engine: HTTP %d: %s", e.Status, e.Detail)
}

// ErrWrongModel is returned when the engine answered with a checkpoint other
// than the one requested; laya-serve does this silently for names it does
// not know.
var ErrWrongModel = errors.New("laya engine answered with a different checkpoint")

// Client calls one or more laya-serve instances, round-robin with failover.
// It is safe for concurrent use.
type Client struct {
	urls  []string
	key   string
	http  *http.Client
	next  atomic.Uint32
	ready atomic.Pointer[map[string]bool]
}

// maxResponseBytes bounds a reply; real ones are a few KB.
const maxResponseBytes = 4 << 20

// NewClient returns a client for the given base URLs.
func NewClient(urls []string, apiKey string, timeout time.Duration, maxConns int) *Client {
	tr := &http.Transport{
		// No proxy: the engine runs next to Laya, and HTTP(S)_PROXY settings
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

// Decide sends req and returns the answers. A 503 (engine busy) is retried
// after its Retry-After for as long as ctx allows; connection errors and
// 5xx move on to the next instance; a timeout or a 4xx is returned at once,
// since retrying would only add to the engine's queue or fail again.
func (c *Client) Decide(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	start := time.Now()
	failovers := len(c.urls)
	var last error
	for attempt := 0; attempt < 30; attempt++ {
		base := c.pick()
		attempt := time.Now()
		resp, err := c.post(ctx, base, body)
		if err == nil {
			resp.Elapsed, resp.Attempt, resp.Endpoint = time.Since(start), time.Since(attempt), base
			if want := req.Model; want != "" && resp.Routing.Model != want {
				return resp, fmt.Errorf("%w: asked for %q, got %q (is the checkpoint configured on the engine?)", ErrWrongModel, want, resp.Routing.Model)
			}
			return resp, nil
		}
		last = err
		var apiErr *APIError
		var netErr net.Error
		switch {
		case ctx.Err() != nil:
			return Response{}, err
		case errors.As(err, &netErr) && netErr.Timeout():
			return Response{}, err
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable:
			if !sleep(ctx, apiErr.RetryAfter) {
				return Response{}, err
			}
			continue
		case errors.As(err, &apiErr) && apiErr.Status < 500:
			return Response{}, err
		}
		if failovers--; failovers < 0 {
			break
		}
	}
	return Response{}, last
}

// SetReady records which instances passed their last health check.
// Requests go to those, or to all of them when none did.
func (c *Client) SetReady(ready map[string]bool) { c.ready.Store(&ready) }

// pick returns the next base URL, round-robin over ready instances.
func (c *Client) pick() string {
	n := len(c.urls)
	start := int(c.next.Add(1) - 1)
	if ready := c.ready.Load(); ready != nil {
		for i := 0; i < n; i++ {
			if u := c.urls[(start+i)%n]; (*ready)[u] {
				return u
			}
		}
	}
	return c.urls[start%n]
}

func (c *Client) post(ctx context.Context, base string, body []byte) (Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.key)
	}
	res, err := c.http.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("laya engine at %s: %w", base, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return Response{}, fmt.Errorf("laya engine at %s: reading reply: %w", base, err)
	}
	if res.StatusCode != http.StatusOK {
		return Response{}, apiError(res, data)
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return Response{}, fmt.Errorf("laya engine at %s: invalid reply: %w", base, err)
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
		return Health{}, err
	}
	if c.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.key)
	}
	res, err := c.http.Do(hreq)
	if err != nil {
		return Health{}, fmt.Errorf("laya engine at %s: %w", base, err)
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
		return Health{}, fmt.Errorf("laya engine at %s: invalid health reply: %w", base, err)
	}
	return h, nil
}
