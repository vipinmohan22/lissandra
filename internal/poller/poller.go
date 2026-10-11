// Package poller periodically fetches logs from HTTP APIs configured as pull
// sources.
package poller

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/ingest"
	"github.com/vipinmohan22/layalogs/internal/model"
)

const maxResponseBytes = 20 << 20

// Status describes a pull source's recent activity.
type Status struct {
	Running     bool      `json:"running"`
	LastPoll    time.Time `json:"last_poll,omitzero"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	LastCount   int       `json:"last_count"`
	Total       uint64    `json:"total"`
}

// SubmitFunc hands fetched events to the pipeline.
type SubmitFunc func(ctx context.Context, evs []model.Event) error

// Manager runs one polling loop per enabled pull source.
type Manager struct {
	log    *slog.Logger
	submit SubmitFunc
	ctx    context.Context

	mu      sync.Mutex
	workers map[string]*worker
	status  map[string]*Status
}

type worker struct {
	hash   string
	cancel context.CancelFunc
}

// NewManager returns a manager whose loops live until ctx is cancelled.
func NewManager(ctx context.Context, log *slog.Logger, submit SubmitFunc) *Manager {
	return &Manager{log: log, submit: submit, ctx: ctx, workers: map[string]*worker{}, status: map[string]*Status{}}
}

// Configure starts, restarts or stops loops to match c.
func (m *Manager) Configure(c *config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]bool{}
	for _, src := range c.Sources {
		if src.Type != config.SourcePull || !src.Enabled || src.Pull == nil {
			continue
		}
		want[src.ID] = true
		h := hashSource(src)
		if w, ok := m.workers[src.ID]; ok {
			if w.hash == h {
				continue
			}
			w.cancel()
		}
		ctx, cancel := context.WithCancel(m.ctx)
		m.workers[src.ID] = &worker{hash: h, cancel: cancel}
		if m.status[src.ID] == nil {
			m.status[src.ID] = &Status{}
		}
		m.status[src.ID].Running = true
		go m.loop(ctx, src)
	}
	for id, w := range m.workers {
		if !want[id] {
			w.cancel()
			delete(m.workers, id)
			if st := m.status[id]; st != nil {
				st.Running = false
			}
		}
	}
}

// Status returns a snapshot of every pull source's status.
func (m *Manager) Status() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Status, len(m.status))
	for id, st := range m.status {
		out[id] = *st
	}
	return out
}

func (m *Manager) update(id string, fn func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.status[id]; st != nil {
		fn(st)
	}
}

func (m *Manager) loop(ctx context.Context, src config.Source) {
	interval := time.Duration(src.Pull.IntervalSeconds) * time.Second
	since := time.Now().Add(-interval)
	seen := newSeenSet(50000)
	m.log.Info("pull source started", "source", src.Name, "interval", interval)

	// Small initial delay so a burst of config changes doesn't hammer the API.
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		var dedupe *seenSet
		if src.Pull.Dedupe {
			dedupe = seen
		}
		evs, err := Fetch(ctx, &src, since, now, dedupe)
		if err == nil && len(evs) > 0 {
			err = m.submit(ctx, evs)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.log.Warn("pull failed", "source", src.Name, "err", err)
			m.update(src.ID, func(st *Status) { st.LastPoll, st.LastError = now, err.Error() })
		} else {
			since = now
			m.update(src.ID, func(st *Status) {
				st.LastPoll, st.LastSuccess, st.LastError, st.LastCount = now, now, "", len(evs)
				st.Total += uint64(len(evs))
			})
		}
		t.Reset(interval)
	}
}

func hashSource(s config.Source) string {
	b, _ := json.Marshal(struct {
		P  *config.Pull
		Pa config.Parse
		N  string
		T  []string
	}{s.Pull, s.Parse, s.Name, s.Tags})
	sum := sha1.Sum(b)
	return string(sum[:])
}

// Fetch performs one poll of src and returns the parsed events. dedupe may
// be nil.
func Fetch(ctx context.Context, src *config.Source, since, now time.Time, dedupe *seenSet) ([]model.Event, error) {
	p := src.Pull
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()

	target := expandPlaceholders(p.URL, since, now, url.QueryEscape)
	var body io.Reader
	if p.Method == http.MethodPost && p.Body != "" {
		body = strings.NewReader(config.ExpandEnv(expandPlaceholders(p.Body, since, now, nil)))
	}
	req, err := http.NewRequestWithContext(ctx, p.Method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "laya-poller/1")
	req.Header.Set("Accept", "application/json, application/x-ndjson, text/plain;q=0.9, */*;q=0.5")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range p.Headers {
		req.Header.Set(k, config.ExpandEnv(v))
	}
	switch p.Auth.Type {
	case config.AuthBearer:
		req.Header.Set("Authorization", "Bearer "+config.ExpandEnv(p.Auth.Token))
	case config.AuthBasic:
		req.SetBasicAuth(config.ExpandEnv(p.Auth.Username), config.ExpandEnv(p.Auth.Password))
	case config.AuthHeader:
		req.Header.Set(p.Auth.HeaderName, config.ExpandEnv(p.Auth.HeaderValue))
	}

	client := http.DefaultClient
	if p.InsecureSkipVerify {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit per-source opt-in
		client = &http.Client{Transport: tr}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, model.Truncate(strings.TrimSpace(string(data)), 300))
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("response larger than %d bytes", maxResponseBytes)
	}
	evs, err := ingest.Parse(data, resp.Header.Get("Content-Type"), src, now.UTC())
	if err != nil {
		return nil, err
	}
	if dedupe == nil {
		return evs, nil
	}
	out := evs[:0]
	for _, ev := range evs {
		if dedupe.add(eventKey(&ev)) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func expandPlaceholders(s string, since, now time.Time, esc func(string) string) string {
	if esc == nil {
		esc = func(v string) string { return v }
	}
	return strings.NewReplacer(
		"{since_rfc3339}", esc(since.UTC().Format(time.RFC3339)),
		"{since_unix_ms}", strconv.FormatInt(since.UnixMilli(), 10),
		"{since_unix}", strconv.FormatInt(since.Unix(), 10),
		"{now_rfc3339}", esc(now.UTC().Format(time.RFC3339)),
		"{now_unix_ms}", strconv.FormatInt(now.UnixMilli(), 10),
		"{now_unix}", strconv.FormatInt(now.Unix(), 10),
	).Replace(s)
}

func eventKey(ev *model.Event) string {
	h := sha1.New()
	fmt.Fprintf(h, "%d\x00%s\x00%s", ev.Timestamp.UnixNano(), ev.Level, ev.Message)
	if len(ev.Fields) > 0 {
		b, _ := json.Marshal(ev.Fields)
		h.Write(b)
	}
	return string(h.Sum(nil))
}

// seenSet is a bounded FIFO set of record hashes. It holds between max and
// 2*max entries.
type seenSet struct {
	max   int
	set   map[string]struct{}
	order []string
}

func newSeenSet(max int) *seenSet { return &seenSet{max: max, set: map[string]struct{}{}} }

// add returns false if key was already present.
func (s *seenSet) add(key string) bool {
	if _, ok := s.set[key]; ok {
		return false
	}
	s.set[key] = struct{}{}
	s.order = append(s.order, key)
	if len(s.order) > 2*s.max { // compact occasionally rather than on every add
		drop := len(s.order) - s.max
		for _, k := range s.order[:drop] {
			delete(s.set, k)
		}
		s.order = append([]string(nil), s.order[drop:]...)
	}
	return true
}
