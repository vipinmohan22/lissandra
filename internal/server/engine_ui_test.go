package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/engine"
	"github.com/vipinmohan22/layalogs/internal/engine/enginetest"
	"github.com/vipinmohan22/layalogs/internal/model"
)

// The tester and "Ask the Laya engine" see the source's tags, as live
// events do, so rules on tags give the same result.
func TestEngineTesterAppliesSourceTags(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	e := newEnv(t)
	var st config.Settings
	e.admin("GET", "/api/settings", nil, &st)
	st.Engine.URL = fake.URL
	if code := e.admin("PUT", "/api/settings", st, nil); code != 200 {
		t.Fatalf("settings: %d", code)
	}
	var src config.Source
	e.admin("POST", "/api/sources", map[string]any{"name": "payments", "type": "push", "enabled": true, "tags": []string{"payments-team"}}, &src)
	e.admin("POST", "/api/rules", map[string]any{"name": "Payments team errors", "enabled": true, "match": "all", "category": "application",
		"conditions": []map[string]string{{"field": "tags", "op": "contains", "value": "payments-team"}, {"field": "message", "op": "contains", "value": "card"}}}, nil)
	var res struct {
		OK    bool `json:"ok"`
		Rules struct {
			Category     string   `json:"category"`
			MatchedRules []string `json:"matched_rules"`
		} `json:"rules"`
	}
	e.admin("POST", "/api/engine/classify", map[string]string{"message": "ERROR card declined by issuer", "source_id": src.ID}, &res)
	if res.Rules.Category != "application" {
		t.Errorf("rules alone, with the source's tags: %+v", res)
	}
}

func TestTrainingDataFileNameIsASCII(t *testing.T) {
	rec := httptest.NewRecorder()
	writeTrainingData(rec, nil, nil, strings.Repeat("a", 150)+"-0123456789abcdef")
	cd := rec.Header().Get("Content-Disposition")
	for _, r := range cd {
		if r > 127 {
			t.Fatalf("non-ASCII header: %q", cd)
		}
	}
}

// One line per text the engine reads, whatever the source; events cut for
// browsing are left out.
func TestTrainingDataDedupesAcrossSourcesAndSkipsCompacted(t *testing.T) {
	cl := engine.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(cl.Close)
	c := config.Defaults()
	c.Settings.Engine.URL = ""
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	cl.Configure(c)
	var evs []model.Event
	for i, src := range []string{"a", "b", "c"} {
		evs = append(evs, model.Event{SourceID: src, Fingerprint: src, Message: fmt.Sprintf("replica %d lost quorum", i), Category: "database", MatchedRules: []string{"Database"}})
	}
	evs = append(evs, model.Event{SourceID: "a", Fingerprint: "x", Message: "pool exhausted", Category: "database", MatchedRules: []string{"Database"}, Compacted: true})
	rec := httptest.NewRecorder()
	writeTrainingData(rec, cl, evs, "live")
	if n := strings.Count(rec.Body.String(), "\n"); n != 1 {
		t.Errorf("%d lines:\n%s", n, rec.Body.String())
	}
}
