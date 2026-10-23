package poller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

func TestFetchPlaceholdersAuthAndDedupe(t *testing.T) {
	t.Setenv("LOGS_TOKEN", "abc")
	var gotQuery, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"message":"a","ts":"2026-10-11T10:00:00Z"},{"message":"b","ts":"2026-10-11T10:00:01Z"}]}`))
	}))
	defer srv.Close()

	src := &config.Source{ID: "p", Name: "p", Type: config.SourcePull, Parse: config.Parse{Format: config.FormatAuto}, Pull: &config.Pull{
		URL: srv.URL + "/logs?from={since_rfc3339}&to={now_unix}", Method: "POST", Body: `{"since":"{since_unix_ms}","key":"${env:LOGS_TOKEN}"}`,
		TimeoutSeconds: 5, Auth: config.Auth{Type: config.AuthBearer, Token: "${env:LOGS_TOKEN}"},
	}}
	since := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	now := since.Add(time.Minute)
	seen := newSeenSet(10)

	evs, err := Fetch(context.Background(), src, since, now, seen)
	if err != nil || len(evs) != 2 {
		t.Fatalf("got %d events, err %v", len(evs), err)
	}
	if gotQuery != "from=2026-10-11T09%3A00%3A00Z&to=1791709260" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotAuth != "Bearer abc" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody != `{"since":"1791709200000","key":"abc"}` {
		t.Errorf("body = %q", gotBody)
	}
	evs, _ = Fetch(context.Background(), src, since, now, seen)
	if len(evs) != 0 {
		t.Errorf("dedupe should drop repeats, got %d", len(evs))
	}
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	src := &config.Source{ID: "p", Name: "p", Type: config.SourcePull, Pull: &config.Pull{URL: srv.URL, Method: "GET", TimeoutSeconds: 5}}
	if _, err := Fetch(context.Background(), src, time.Now(), time.Now(), nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v", err)
	}
}

func TestManagerStartsAndStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("line one\nline two\n"))
	}))
	defer srv.Close()

	var mu sync.Mutex
	var got []model.Event
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), func(_ context.Context, evs []model.Event) error {
		mu.Lock()
		got = append(got, evs...)
		mu.Unlock()
		return nil
	})
	cfg := &config.Config{Sources: []config.Source{{ID: "p", Name: "p", Type: config.SourcePull, Enabled: true,
		Pull: &config.Pull{URL: srv.URL, Method: "GET", IntervalSeconds: 60, TimeoutSeconds: 5}}}}
	m.Configure(cfg)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := m.Status()["p"]
	if !st.Running || st.LastCount != 2 || st.Total != 2 || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}

	cfg.Sources[0].Enabled = false
	m.Configure(cfg)
	if m.Status()["p"].Running {
		t.Error("disabled source should stop")
	}
}

// A token in the pull URL (a user alone, which Go doesn't hide) doesn't
// show in fetch errors.
func TestFetchErrorHidesURLToken(t *testing.T) {
	src := &config.Source{ID: "s", Name: "s", Type: config.SourcePull, Pull: &config.Pull{URL: "http://tok_pull_secret@127.0.0.1:1/logs", Method: "GET", TimeoutSeconds: 1}}
	_, err := Fetch(context.Background(), src, time.Now(), time.Now(), nil)
	if err == nil || strings.Contains(err.Error(), "tok_pull_secret") {
		t.Errorf("fetch error: %v", err)
	}
}
