package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine"
	"github.com/vipinmohan22/lissandra/internal/events"
	"github.com/vipinmohan22/lissandra/internal/importer"
	"github.com/vipinmohan22/lissandra/internal/notify"
	"github.com/vipinmohan22/lissandra/internal/pipeline"
	"github.com/vipinmohan22/lissandra/internal/poller"
)

func TestFileReferencesAreLimitedToAllowedDirs(t *testing.T) {
	allowed, other := t.TempDir(), t.TempDir()
	dirs := config.FileRefDirs()
	config.SetFileRefDirs([]string{allowed})
	t.Cleanup(func() { config.SetFileRefDirs(dirs) })
	secret := filepath.Join(other, "token")
	os.WriteFile(secret, []byte("hvs.super-secret-token\n"), 0o600)
	inside := filepath.Join(allowed, "urls")
	os.WriteFile(inside, []byte("not a url, hvs.another-secret\n"), 0o600)

	e := newEnv(t)
	var st config.Settings
	e.admin("GET", "/api/settings", nil, &st)
	for _, ref := range []string{"${file:" + secret + "}", "${file:/proc/self/environ}"} {
		st.Engine.URL = ref
		if code := e.admin("PUT", "/api/settings", st, nil); code != 400 {
			t.Errorf("%s: saved (%d)", ref, code)
		}
	}
	st.Engine.URL = "${file:" + inside + "}"
	if code := e.admin("PUT", "/api/settings", st, nil); code != 200 {
		t.Fatalf("an allowed reference: %d", code)
	}
	var stats struct {
		Engine struct {
			ConfigError string `json:"config_error"`
		} `json:"engine"`
	}
	e.admin("GET", "/api/stats", nil, &stats)
	_, health := e.do("GET", "/api/engine/health", nil, authHeaders(nil))
	if stats.Engine.ConfigError == "" || strings.Contains(stats.Engine.ConfigError+string(health), "secret") {
		t.Errorf("what a reference expands to must not be shown: %q %s", stats.Engine.ConfigError, health)
	}
	n := map[string]any{"name": "hook", "type": "webhook", "enabled": true, "settings": map[string]string{"url": "${file:" + secret + "}"}}
	if code := e.admin("POST", "/api/notifiers", n, nil); code != 400 {
		t.Errorf("a notifier referencing a file outside the allowed dirs was saved: %d", code)
	}
}

func TestMaskedURLsSurviveEdits(t *testing.T) {
	e := newEnv(t)
	var st config.Settings
	e.admin("GET", "/api/settings", nil, &st)
	st.Engine.URL = "http://laya:s3cret@10.0.0.1:8000"
	e.admin("PUT", "/api/settings", st, &st)
	if strings.Contains(st.Engine.URL, "s3cret") {
		t.Fatalf("shown: %q", st.Engine.URL)
	}
	st.Engine.URL += ",http://laya:s3cret@10.0.0.2:8000" // an instance added in the UI
	if code := e.admin("PUT", "/api/settings", st, nil); code != 200 {
		t.Fatalf("put %d", code)
	}
	if got := e.cfg.Get().Settings.Engine.URL; got != "http://laya:s3cret@10.0.0.1:8000,http://laya:s3cret@10.0.0.2:8000" {
		t.Errorf("stored %q", got)
	}

	var src config.Source
	e.admin("POST", "/api/sources", map[string]any{"name": "remote", "type": "pull", "enabled": false,
		"pull": map[string]any{"url": "https://svc:pullpass123@logs.example/api?since={since_unix}", "method": "GET", "interval_seconds": 60, "auth": map[string]any{"type": "none"}}}, &src)
	_, list := e.do("GET", "/api/sources", nil, authHeaders(nil))
	_, exp := e.do("GET", "/api/config/export", nil, authHeaders(nil))
	if strings.Contains(string(list)+string(exp), "pullpass123") {
		t.Error("a pull URL's password is shown")
	}
	src.Pull.IntervalSeconds = 120
	if code := e.admin("PUT", "/api/sources/"+src.ID, src, nil); code != 200 {
		t.Fatalf("update %d", code)
	}
	if got := e.cfg.Get().Sources[1].Pull.URL; got != "https://svc:pullpass123@logs.example/api?since={since_unix}" {
		t.Errorf("pull url after an edit: %q", got)
	}
}

// An export from before the engine, and a settings update that leaves the
// engine out, keep sane engine settings.
func TestOldImportsAndPartialSettingsKeepEngineSettings(t *testing.T) {
	e := newEnv(t)
	old := map[string]any{"settings": map[string]any{"categories": []string{"database"}, "ai": map[string]any{"enabled": true}},
		"sources": []any{}, "rules": []any{}, "notifiers": []any{}, "routes": []any{}}
	if code := e.admin("POST", "/api/config/import", old, nil); code != 200 {
		t.Fatalf("import %d", code)
	}
	if eng := e.cfg.Get().Settings.Engine; eng.MinConfidence != 0.6 || !eng.Enabled || eng.URL == "" {
		t.Errorf("engine after importing an old export: %+v", eng)
	}
	if code := e.admin("PUT", "/api/settings", map[string]any{"public_url": "https://lissandra.example"}, nil); code != 200 {
		t.Fatalf("put %d", code)
	}
	if st := e.cfg.Get().Settings; st.Engine.MinConfidence != 0.6 || len(st.Categories) != 1 || st.PublicURL != "https://lissandra.example" {
		t.Errorf("settings after a partial update: %+v", st)
	}
}

// Concurrent partial settings updates don't undo each other.
func TestConcurrentPartialSettingsUpdates(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 20; i++ {
		var wg sync.WaitGroup
		url, cat := fmt.Sprintf("https://lissandra-%d.example", i), fmt.Sprintf("cat%d", i)
		wg.Add(2)
		go func() { defer wg.Done(); e.admin("PUT", "/api/settings", map[string]any{"public_url": url}, nil) }()
		go func() {
			defer wg.Done()
			e.admin("PUT", "/api/settings", map[string]any{"categories": []string{cat}}, nil)
		}()
		wg.Wait()
		st := e.cfg.Get().Settings
		if st.PublicURL != url || len(st.Categories) != 1 || st.Categories[0].Name != cat {
			t.Fatalf("round %d lost an update: %q %+v", i, st.PublicURL, st.Categories)
		}
	}
}

// A config that can't be saved is a server fault (500), and the reply
// doesn't show the server's paths.
func TestSaveFailureIsAServerError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg, err := config.Open(filepath.Join(dir, "config.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := events.NewStore(100)
	d := notify.NewDispatcher(log)
	a := engine.New(log)
	defer a.Close()
	p := pipeline.New(100, store, d, a)
	pm := poller.NewManager(ctx, log, p.SubmitWait)
	reg := importer.NewRegistry(importer.RegistryConfig{Keep: 3, Limits: importer.Limits{Events: 1000, Bytes: 8 << 20}})
	srv := httptest.NewServer(New(Options{Version: "test", AdminToken: adminToken, UI: fstest.MapFS{}}, log, cfg, store, p, d, a, nil, pm, reg).Handler())
	defer srv.Close()
	e := &env{t: t, srv: srv, cfg: cfg, imports: reg}
	os.RemoveAll(dir)
	os.WriteFile(dir, []byte("x"), 0o600) // the data dir is now a file: saving fails
	code, body := e.do("PUT", "/api/settings", map[string]any{"public_url": "https://x.example"}, authHeaders(nil))
	if code != 500 || strings.Contains(string(body), dir) || !strings.Contains(string(body), `"code":"save_failed"`) {
		t.Errorf("%d %s", code, body)
	}
	if code, c := e.apiErr(adminToken, "POST", "/api/onboarding/steps/sources", map[string]string{"outcome": "done"}); code != 500 || c != "save_failed" {
		t.Errorf("a setup step: %d %s", code, c)
	}
}
