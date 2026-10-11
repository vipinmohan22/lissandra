package ai

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

type fakeAPI struct {
	calls   atomic.Int32
	lastReq map[string]any
	lastHdr http.Header
	reply   string
	status  int
}

func (f *fakeAPI) start(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &f.lastReq)
		f.lastHdr = r.Header
		w.Header().Set("Content-Type", "application/json")
		if f.status != 0 {
			w.WriteHeader(f.status)
			w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
			return
		}
		resp := map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
			"content":     []map[string]any{{"type": "text", "text": f.reply}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
}

func settings(enabled bool) config.Settings {
	return config.Settings{
		Categories: []string{"database", "network"},
		AI: config.AISettings{
			Enabled: enabled, APIKey: "${env:LAYA_TEST_KEY}", Model: "claude-opus-5-5", Effort: "low",
			OnlyUncategorized: true, MinSeverity: model.SeverityWarning, MaxCallsPerHour: 2,
		},
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestClassifyRequestShapeAndCache(t *testing.T) {
	t.Setenv("LAYA_TEST_KEY", "sk-test")
	api := &fakeAPI{reply: `{"category":"database","severity":"critical","summary":"Primary database is refusing connections."}`}
	api.start(t)

	c := New(quiet())
	c.Configure(settings(true))

	ev := &model.Event{Message: "pg: connection refused", Severity: model.SeverityError, Category: model.Uncategorized, Fingerprint: "fp1", SourceName: "api"}
	if !c.Eligible(ev) {
		t.Fatal("event should be eligible")
	}
	c.Enrich(context.Background(), ev)
	if ev.Category != "database" || ev.Severity != model.SeverityCritical || ev.Classifier != "ai" || ev.Summary == "" {
		t.Fatalf("enrichment not applied: %+v", ev)
	}

	req := api.lastReq
	if req["model"] != "claude-opus-5-5" || req["fallbacks"] != "default" {
		t.Errorf("model/fallbacks: %v / %v", req["model"], req["fallbacks"])
	}
	oc := req["output_config"].(map[string]any)
	format := oc["format"].(map[string]any)
	if oc["effort"] != "low" || format["type"] != "json_schema" {
		t.Errorf("output_config = %v", oc)
	}
	enum := format["schema"].(map[string]any)["properties"].(map[string]any)["category"].(map[string]any)["enum"].([]any)
	if len(enum) != 3 || enum[2] != model.Uncategorized {
		t.Errorf("category enum = %v", enum)
	}
	if !strings.Contains(api.lastHdr.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("beta header = %q", api.lastHdr.Get("Anthropic-Beta"))
	}
	if api.lastHdr.Get("X-Api-Key") != "sk-test" {
		t.Errorf("api key header = %q", api.lastHdr.Get("X-Api-Key"))
	}

	// Same fingerprint: served from cache, no new call.
	ev2 := &model.Event{Message: "pg: connection refused", Severity: model.SeverityError, Category: model.Uncategorized, Fingerprint: "fp1", Level: "error"}
	if !c.ApplyCached(ev2) {
		t.Fatal("expected cache hit")
	}
	if api.calls.Load() != 1 {
		t.Errorf("calls = %d", api.calls.Load())
	}
	if ev2.Severity != model.SeverityError {
		t.Error("explicit log level must not be overridden by AI")
	}

	// File analysis reads the cache without counting hits in live metrics.
	hits := c.CacheHits.Load()
	ev3 := &model.Event{Message: "pg: connection refused", Severity: model.SeverityError, Category: model.Uncategorized, Fingerprint: "fp1"}
	if !c.ApplyCachedQuiet(ev3) || ev3.Category != "database" || c.CacheHits.Load() != hits {
		t.Errorf("quiet lookup: %+v, hits %d -> %d", ev3, hits, c.CacheHits.Load())
	}
}

func TestBudgetAndEligibility(t *testing.T) {
	t.Setenv("LAYA_TEST_KEY", "sk-test")
	api := &fakeAPI{reply: `{"category":"bogus","severity":"loud","summary":"x"}`}
	api.start(t)
	c := New(quiet())
	c.Configure(settings(true))

	if c.Eligible(&model.Event{Severity: model.SeverityInfo, Category: model.Uncategorized}) {
		t.Error("below min severity should not be eligible")
	}
	if c.Eligible(&model.Event{Severity: model.SeverityError, Category: "network"}) {
		t.Error("categorized events should not be eligible when only_uncategorized")
	}
	for i := 0; i < 2; i++ {
		r, err := c.Classify(context.Background(), &model.Event{Message: "m", Fingerprint: string(rune('a' + i))})
		if err != nil {
			t.Fatal(err)
		}
		if r.Category != model.Uncategorized || r.Severity != "" {
			t.Errorf("out-of-enum answers should be discarded: %+v", r)
		}
	}
	if _, err := c.Classify(context.Background(), &model.Event{Message: "m", Fingerprint: "c"}); err != ErrNoBudget {
		t.Errorf("third call should hit the hourly budget, got %v", err)
	}

	c.Configure(settings(false))
	if c.Eligible(&model.Event{Severity: model.SeverityCritical, Category: model.Uncategorized}) {
		t.Error("disabled classifier should not be eligible")
	}
}

func TestAuthErrorPauses(t *testing.T) {
	t.Setenv("LAYA_TEST_KEY", "sk-bad")
	api := &fakeAPI{status: http.StatusUnauthorized}
	api.start(t)
	c := New(quiet())
	c.Configure(settings(true))
	if _, err := c.Classify(context.Background(), &model.Event{Message: "m", Fingerprint: "a"}); err == nil {
		t.Fatal("expected error")
	}
	if c.HasBudget() {
		t.Error("auth failure should pause further calls")
	}
	if st := c.Status(); st.Errors != 1 || st.LastError == "" || st.PausedUntil.IsZero() {
		t.Errorf("status = %+v", st)
	}
}
