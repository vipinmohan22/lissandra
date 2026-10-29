package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

type capture struct {
	mu   sync.Mutex
	reqs []captured
}

type captured struct {
	path    string
	headers http.Header
	body    map[string]any
	raw     string
}

func (c *capture) server(t *testing.T, status int) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		c.mu.Lock()
		c.reqs = append(c.reqs, captured{path: r.URL.Path, headers: r.Header, body: m, raw: string(b)})
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *capture) all() []captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]captured(nil), c.reqs...)
}

func testMessage() *Message {
	return &Message{
		Event: model.Event{
			ID: "e1", SourceName: "api", Message: "payment failed <b>bold</b> & ```code```", Severity: model.SeverityCritical,
			Category: "application", Tags: []string{"crash"}, Fingerprint: "abc", Timestamp: time.Now(),
		},
		RouteName:  "oncall",
		Suppressed: 3,
		Link:       "https://lissandra.example.com/#/events/e1",
	}
}

func TestChatPayloads(t *testing.T) {
	var c capture
	srv := c.server(t, http.StatusOK)
	ctx := context.Background()
	for _, n := range []config.Notifier{
		{Type: "slack", Settings: map[string]string{"webhook_url": srv.URL + "/slack", "mention": "<!here>"}},
		{Type: "teams", Settings: map[string]string{"webhook_url": srv.URL + "/teams"}},
		{Type: "discord", Settings: map[string]string{"webhook_url": srv.URL + "/discord"}},
		{Type: "googlechat", Settings: map[string]string{"webhook_url": srv.URL + "/gchat"}},
		{Type: "pagerduty", Settings: map[string]string{"routing_key": "rk", "api_url": srv.URL + "/pd"}},
	} {
		s, err := Build(n)
		if err != nil {
			t.Fatalf("%s: %v", n.Type, err)
		}
		if err := s.Send(ctx, testMessage()); err != nil {
			t.Fatalf("%s send: %v", n.Type, err)
		}
	}
	reqs := c.all()
	if len(reqs) != 5 {
		t.Fatalf("got %d requests", len(reqs))
	}
	blocks := reqs[0].body["attachments"].([]any)[0].(map[string]any)["blocks"].([]any)
	head := blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	code := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if !strings.HasPrefix(head, "<!here> ") {
		t.Errorf("slack mention missing: %q", head)
	}
	if code != "```payment failed &lt;b&gt;bold&lt;/b&gt; &amp; '''code'''```" {
		t.Errorf("slack message not escaped / backticks not neutralised: %q", code)
	}
	teams := reqs[1].body
	att := teams["attachments"].([]any)[0].(map[string]any)
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" || teams["type"] != "message" {
		t.Errorf("teams payload: %v", teams)
	}
	if !strings.Contains(reqs[1].raw, "Action.OpenUrl") || !strings.Contains(reqs[1].raw, "3 similar event(s)") {
		t.Error("teams card missing link or suppressed count")
	}
	if reqs[2].body["allowed_mentions"] == nil {
		t.Error("discord must disable mentions so log text can't ping @everyone")
	}
	pd := reqs[4].body
	if pd["routing_key"] != "rk" || pd["dedup_key"] != "lissandra-abc" || pd["payload"].(map[string]any)["severity"] != "critical" {
		t.Errorf("pagerduty payload: %v", pd)
	}
}

func TestWebhookTemplateAndHeaders(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "t0k")
	var c capture
	srv := c.server(t, http.StatusAccepted)
	s, err := Build(config.Notifier{Type: "webhook", Settings: map[string]string{
		"url":           srv.URL,
		"method":        "PUT",
		"headers":       "Authorization: Bearer ${env:HOOK_TOKEN}\nX-Team: core",
		"body_template": `{"title": {{ json .Title }}, "sev": {{ json .Event.Severity }}, "n": {{ .Suppressed }}, "short": {{ json (truncate 7 .Event.Message) }}}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	r := c.all()[0]
	if r.headers.Get("Authorization") != "Bearer t0k" || r.headers.Get("X-Team") != "core" {
		t.Errorf("headers: %v", r.headers)
	}
	if r.body["sev"] != "critical" || r.body["n"] != float64(3) || r.body["short"] != "payment…" {
		t.Errorf("body: %s", r.raw)
	}

	// Default body is the standard JSON payload.
	s, _ = Build(config.Notifier{Type: "webhook", Settings: map[string]string{"url": srv.URL}})
	if err := s.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	def := c.all()[1].body
	if def["title"] == nil || def["event"].(map[string]any)["fingerprint"] != "abc" {
		t.Errorf("default payload: %v", def)
	}

	if _, err := Build(config.Notifier{Type: "webhook", Settings: map[string]string{"url": srv.URL, "body_template": "{{ .Nope"}}); err == nil {
		t.Error("bad template should fail to build")
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(config.Notifier{Type: "slack", Settings: map[string]string{}}); err == nil {
		t.Error("missing required webhook should fail")
	}
	if err := Validate(config.Notifier{Type: "slack", Settings: map[string]string{"webhook_url": "${env:SLACK_URL}"}}); err != nil {
		t.Errorf("env references should pass validation: %v", err)
	}
	if err := Validate(config.Notifier{Type: "email", Settings: map[string]string{"smtp_host": "h", "from": "a@b.c", "to": "x@y.z", "security": "ssl"}}); err == nil {
		t.Error("invalid select value should fail")
	}
	if err := Validate(config.Notifier{Type: "nope"}); err == nil {
		t.Error("unknown type should fail")
	}
	masked := Mask(config.Notifier{Type: "slack", Settings: map[string]string{"webhook_url": "https://hooks/x", "username": "lissandra"}})
	if masked.Settings["webhook_url"] != config.Mask || masked.Settings["username"] != "lissandra" {
		t.Errorf("mask: %v", masked.Settings)
	}
	Restore(&masked, config.Notifier{Type: "slack", Settings: map[string]string{"webhook_url": "https://hooks/x"}})
	if masked.Settings["webhook_url"] != "https://hooks/x" {
		t.Error("restore failed")
	}
}

// fakeSMTP is a minimal SMTP server that records one message.
func fakeSMTP(t *testing.T) (addr string, got chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := func(s string) { conn.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					w("250 queued")
					got <- data.String()
					continue
				}
				data.WriteString(line)
				continue
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250-fake\r\n250 AUTH PLAIN")
			case strings.HasPrefix(cmd, "AUTH"):
				w("235 ok")
			case strings.HasPrefix(cmd, "DATA"):
				inData = true
				w("354 go ahead")
			case strings.HasPrefix(cmd, "QUIT"):
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestEmailSend(t *testing.T) {
	addr, got := fakeSMTP(t)
	host, port, _ := net.SplitHostPort(addr)
	s, err := Build(config.Notifier{Type: "email", Settings: map[string]string{
		"smtp_host": host, "smtp_port": port, "security": "none", "username": "u", "password": "p",
		"from": "Lissandra <lissandra@example.com>", "to": "a@example.com, b@example.com",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	for _, want := range []string{"Subject: ", "multipart/alternative", "text/html", "X-Lissandra-Severity: critical", "To: <a@example.com>, <b@example.com>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("email missing %q:\n%s", want, msg)
		}
	}
	_, htmlPart, ok := strings.Cut(msg, "Content-Type: text/html")
	if !ok {
		t.Fatal("no html part")
	}
	html, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(htmlPart)))
	if strings.Contains(string(html), "<b>bold</b>") || !strings.Contains(string(html), "&lt;b&gt;bold&lt;/b&gt;") {
		t.Errorf("HTML body must escape log content:\n%s", html)
	}
}

type fakeSender struct {
	mu    sync.Mutex
	msgs  []*Message
	fails int
}

func (f *fakeSender) Send(ctx context.Context, m *Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return &HTTPError{Status: 503}
	}
	f.msgs = append(f.msgs, m)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

var fake = &fakeSender{}

func init() {
	Register(&Plugin{Type: "fake-test", Label: "Fake", New: func(map[string]string) (Sender, error) { return fake, nil }})
}

func TestDispatcherRoutingAndThrottle(t *testing.T) {
	fake.mu.Lock()
	fake.msgs, fake.fails = nil, 1 // first attempt fails, retry succeeds
	fake.mu.Unlock()

	clock := newFakeClock()
	d := NewDispatcherWith(slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Now: clock.Now})
	d.Configure(&config.Config{
		Notifiers: []config.Notifier{{ID: "n1", Name: "fake", Type: "fake-test", Enabled: true}},
		Routes: []config.Route{
			{ID: "r1", Name: "db errors", Enabled: true, MinSeverity: model.SeverityError, Categories: []string{"database"}, NotifierIDs: []string{"n1"}, ThrottleSeconds: 3600},
			{ID: "r2", Name: "disabled", Enabled: false, MinSeverity: model.SeverityDebug, NotifierIDs: []string{"n1"}},
		},
		Settings: config.Settings{PublicURL: "https://lissandra.test"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, 1)

	ev := model.Event{ID: "1", Severity: model.SeverityError, Category: "database", Fingerprint: "fp1"}
	d.Handle(ev)                                                                                            // sent
	d.Handle(ev)                                                                                            // throttled
	d.Handle(model.Event{ID: "2", Severity: model.SeverityWarning, Category: "database", Fingerprint: "x"}) // below min severity
	d.Handle(model.Event{ID: "3", Severity: model.SeverityCritical, Category: "network", Fingerprint: "y"}) // wrong category
	d.Handle(model.Event{ID: "4", Severity: model.SeverityCritical, Category: "database", Fingerprint: "fp2"})

	deadline := time.Now().Add(10 * time.Second)
	for fake.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if fake.count() != 2 {
		t.Fatalf("delivered %d, want 2", fake.count())
	}
	if d.Throttled.Load() != 1 {
		t.Errorf("throttled = %d", d.Throttled.Load())
	}
	if got := fake.msgs[0].Link; got != "https://lissandra.test/#/events/1" && got != "https://lissandra.test/#/events/4" {
		t.Errorf("link = %q", got)
	}
	dels := d.Deliveries(10)
	if len(dels) != 2 || dels[0].Status != "sent" {
		t.Errorf("deliveries: %+v", dels)
	}
	var retried bool
	for _, del := range dels {
		retried = retried || del.Attempts == 2
	}
	if !retried {
		t.Error("expected one delivery to need a retry")
	}

	// After the window, the next alert reports suppressed repeats.
	clock.Add(2 * time.Hour)
	d.Handle(ev)
	for fake.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.count() != 3 {
		t.Fatalf("delivered %d, want 3", fake.count())
	}
	fake.mu.Lock()
	third := fake.msgs[2]
	fake.mu.Unlock()
	if third.Suppressed != 1 || third.Reason.Code != ReasonRepeat {
		t.Errorf("after the window: suppressed %d, reason %+v", third.Suppressed, third.Reason)
	}
}

func TestDispatcherDrain(t *testing.T) {
	fake.mu.Lock()
	fake.msgs, fake.fails = nil, 0
	fake.mu.Unlock()
	d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Configure(&config.Config{
		Notifiers: []config.Notifier{{ID: "n1", Name: "fake", Type: "fake-test", Enabled: true}},
		Routes:    []config.Route{{ID: "r1", Name: "all", Enabled: true, MinSeverity: model.SeverityDebug, NotifierIDs: []string{"n1"}}},
	})
	for i := 0; i < 5; i++ {
		d.Handle(model.Event{ID: fmt.Sprint(i), Severity: model.SeverityError, Fingerprint: fmt.Sprint("fp", i)})
	}
	// Nothing delivers until Run starts: Drain waits for it.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if err := d.Drain(ctx); err == nil {
		t.Error("drain returned with notifications still queued")
	}
	cancel()
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go d.Run(runCtx, 2)
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Drain(ctx); err != nil || fake.count() != 5 {
		t.Errorf("drain: %v delivered=%d", err, fake.count())
	}
}

// senderFunc adapts a function to Sender.
type senderFunc func(ctx context.Context, m *Message) error

func (f senderFunc) Send(ctx context.Context, m *Message) error { return f(ctx, m) }

var (
	hang     = make(chan struct{}) // closed to release the hung notifier
	failures atomic.Int64
)

func init() {
	Register(&Plugin{Type: "hang-test", Label: "Hang", New: func(map[string]string) (Sender, error) {
		return senderFunc(func(ctx context.Context, m *Message) error {
			select {
			case <-hang:
			case <-ctx.Done():
			}
			return nil
		}), nil
	}})
	Register(&Plugin{Type: "fail-test", Label: "Fail", New: func(map[string]string) (Sender, error) {
		return senderFunc(func(ctx context.Context, m *Message) error {
			failures.Add(1)
			return &HTTPError{Status: 503}
		}), nil
	}})
}

func TestHungNotifierOnlyDelaysItself(t *testing.T) {
	fake.mu.Lock()
	fake.msgs, fake.fails = nil, 0
	fake.mu.Unlock()
	d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Configure(&config.Config{
		Notifiers: []config.Notifier{
			{ID: "slow", Name: "slow", Type: "hang-test", Enabled: true},
			{ID: "ok", Name: "ok", Type: "fake-test", Enabled: true},
		},
		Routes: []config.Route{{ID: "r1", Name: "all", Enabled: true, MinSeverity: model.SeverityDebug, NotifierIDs: []string{"slow", "ok"}}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, 2)
	defer close(hang)
	for i := 0; i < 6; i++ { // more than the workers per notifier
		d.Handle(model.Event{ID: fmt.Sprint(i), Severity: model.SeverityError, Fingerprint: fmt.Sprint("fp", i)})
	}
	deadline := time.Now().Add(5 * time.Second)
	for fake.count() < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.count() != 6 {
		t.Errorf("a hung notifier held up another: %d of 6 delivered", fake.count())
	}
}

func TestDrainStopsRetrying(t *testing.T) {
	d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Configure(&config.Config{
		Notifiers: []config.Notifier{{ID: "n1", Name: "down", Type: "fail-test", Enabled: true}},
		Routes:    []config.Route{{ID: "r1", Name: "all", Enabled: true, MinSeverity: model.SeverityDebug, NotifierIDs: []string{"n1"}}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, 1)
	failures.Store(0)
	for i := 0; i < 3; i++ {
		d.Handle(model.Event{ID: fmt.Sprint(i), Severity: model.SeverityError, Fingerprint: fmt.Sprint("fp", i)})
	}
	for failures.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	if err := d.Drain(dctx); err != nil || time.Since(start) > time.Second {
		t.Errorf("drain behind a failing notifier: %v after %v", err, time.Since(start))
	}
	if d.Failed.Load() != 3 {
		t.Errorf("failed = %d, want 3", d.Failed.Load())
	}
}

func TestLanesOfRemovedNotifiersStop(t *testing.T) {
	d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := &config.Config{
		Notifiers: []config.Notifier{{ID: "n1", Name: "fake", Type: "fake-test", Enabled: true}},
		Routes:    []config.Route{{ID: "r1", Name: "all", Enabled: true, MinSeverity: model.SeverityDebug, NotifierIDs: []string{"n1"}}},
	}
	d.Configure(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, 1)
	d.Handle(model.Event{ID: "1", Severity: model.SeverityError, Fingerprint: "fp"})
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	if err := d.Drain(dctx); err != nil {
		t.Fatal(err)
	}
	d.pruneLanes()
	if d.lanes["n1"] == nil {
		t.Fatal("a configured notifier's lane was stopped")
	}
	cfg.Notifiers = nil
	d.Configure(cfg)
	d.pruneLanes()
	if len(d.lanes) != 0 {
		t.Errorf("lanes left after the notifier was removed: %d", len(d.lanes))
	}
}

// A notifier's first event while Run is stopping must not start workers
// on the wait group Run is waiting on.
func TestLaneMadeWhileRunStops(t *testing.T) {
	for i := 0; i < 200; i++ {
		d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
		d.Configure(&config.Config{
			Notifiers: []config.Notifier{{ID: "n1", Name: "fake", Type: "fake-test", Enabled: true}},
			Routes:    []config.Route{{ID: "r1", Name: "all", Enabled: true, MinSeverity: model.SeverityDebug, NotifierIDs: []string{"n1"}}},
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { d.Run(ctx, 1); close(done) }()
		cancel()
		d.Handle(model.Event{ID: "1", Severity: model.SeverityError, Fingerprint: fmt.Sprint(i)})
		<-done
	}
}

// Errors don't show a webhook's path (often its secret) or an expanded
// header line.
func TestNotifierErrorsHideSecrets(t *testing.T) {
	_, err := newWebhook(map[string]string{"url": "https://hooks.example/services/T0/B0/s3cr3t", "headers": "Authorization: Bearer x\nhvs.SUPER-SECRET"})
	if err == nil || strings.Contains(err.Error(), "SUPER-SECRET") || !strings.Contains(err.Error(), "header 2 ") {
		t.Errorf("header error: %v", err)
	}
	err = doRequest(context.Background(), http.MethodPost, "http://127.0.0.1:1/services/T0/B0/s3cr3t", "application/json", nil, nil)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("request error: %v", err)
	}
}

func TestRecipientAndAudience(t *testing.T) {
	recipients := map[string]string{"email": "to", "slack": "mention"}
	for _, typ := range []string{"email", "slack", "teams", "discord", "googlechat", "pagerduty", "webhook"} {
		p, ok := PluginFor(typ)
		if !ok {
			t.Fatalf("no plugin %s", typ)
		}
		if p.Audience == "" {
			t.Errorf("%s has no audience", p.Type)
		}
		var keys []string
		for _, f := range p.Fields {
			if f.Recipient {
				keys = append(keys, f.Key)
			}
		}
		if want := recipients[p.Type]; strings.Join(keys, ",") != want {
			t.Errorf("%s recipient fields = %v, want %q", p.Type, keys, want)
		}
	}
	data, _ := json.Marshal(Plugins())
	if !strings.Contains(string(data), `"recipient":true`) || !strings.Contains(string(data), `"audience":"The addresses in To."`) {
		t.Errorf("JSON = %s", data)
	}
}

// An alert group becomes a route limited to its sources, to its channels
// and to its members through the email notifier's server; a group without
// sources, or disabled, matches nothing.
func TestAlertGroupRoutes(t *testing.T) {
	c := config.Defaults()
	c.Notifiers = []config.Notifier{
		{ID: "mail", Name: "SMTP", Type: "email", Enabled: true, Settings: map[string]string{"smtp_host": "smtp.example.com", "from": "lissandra@example.com", "to": "everyone@example.com"}},
		{ID: "chat", Name: "chat", Type: "slack", Enabled: true, Settings: map[string]string{"webhook_url": "https://hooks.example.com/x"}},
	}
	c.Sources = append(c.Sources, config.Source{ID: "pay", Name: "payments", Type: config.SourcePush, Enabled: true, AlertGroupIDs: []string{"team"}})
	c.AlertGroups = []config.AlertGroup{
		{ID: "team", Name: "Payments", Enabled: true, NotifierIDs: []string{"chat"}, EmailNotifierID: "mail", Emails: []string{"a@example.com", "b@example.com"}, MinSeverity: model.SeverityError},
		{ID: "idle", Name: "Nobody's", Enabled: true, NotifierIDs: []string{"chat"}},
		{ID: "off", Name: "Off", NotifierIDs: []string{"chat"}},
	}
	c.Sources[0].AlertGroupIDs = []string{"off"}
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	routes, channels := alertGroups(c)
	if len(routes) != 1 || routes[0].Name != "Alert group: Payments" || strings.Join(routes[0].SourceIDs, ",") != "pay" ||
		strings.Join(routes[0].NotifierIDs, ",") != "chat,"+groupEmailID("team") {
		t.Fatalf("routes: %+v", routes)
	}
	n := channels[groupEmailID("team")]
	if n.Settings["to"] != "a@example.com, b@example.com" || n.Settings["smtp_host"] != "smtp.example.com" || c.Notifiers[0].Settings["to"] != "everyone@example.com" {
		t.Errorf("email channel: %+v (the notifier's own: %v)", n.Settings, c.Notifiers[0].Settings["to"])
	}
	match := RouteMatcher(c)
	for _, tc := range []struct {
		src  string
		sev  model.Severity
		want int
	}{{"pay", model.SeverityError, 1}, {"pay", model.SeverityWarning, 0}, {c.Sources[0].ID, model.SeverityCritical, 0}} {
		if got := match(&model.Event{SourceID: tc.src, Severity: tc.sev}); len(got) != tc.want {
			t.Errorf("%s %s: %v", tc.src, tc.sev, got)
		}
	}
}

// Alerts lead with why they were sent; chat headers and subjects say what
// made them go right away.
func TestAlertWhyAndHeadline(t *testing.T) {
	m := testMessage()
	m.Reason = Reason{ReasonNew, "New problem: first seen just now; nothing like it in the last 7 days."}
	if f := m.Facts(); f[0].Name != "Why" || f[0].Value != m.Reason.Text || f[1].Name != "Severity" {
		t.Errorf("facts = %+v", f)
	}
	for code, suffix := range map[string]string{ReasonNew: " · new problem", ReasonSpike: " · spiking", ReasonWorse: " · got worse", ReasonRepeat: " · still happening", ReasonSeverity: ""} {
		m.Reason.Code = code
		if got := m.Headline(); got != m.Title()+suffix {
			t.Errorf("%s: %q", code, got)
		}
	}
	m.Reason.Code = ReasonNew
	m.Since, m.at = time.Date(2026, 10, 19, 10, 2, 0, 0, time.UTC), time.Date(2026, 10, 19, 11, 0, 0, 0, time.UTC)
	var supp string
	for _, f := range m.Facts() {
		if f.Name == "Suppressed" {
			supp = f.Value
		}
	}
	if supp != "3 more since the alert at 10:02 UTC" {
		t.Errorf("suppressed = %q", supp)
	}

	var c capture
	srv := c.server(t, http.StatusOK)
	for _, n := range []config.Notifier{
		{Type: "slack", Settings: map[string]string{"webhook_url": srv.URL + "/slack"}},
		{Type: "pagerduty", Settings: map[string]string{"routing_key": "rk", "api_url": srv.URL + "/pd"}},
		{Type: "webhook", Settings: map[string]string{"url": srv.URL + "/hook"}},
	} {
		s, _ := Build(n)
		if err := s.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	reqs := c.all()
	head := reqs[0].body["attachments"].([]any)[0].(map[string]any)["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	if head != "*[CRITICAL] application · api · new problem*" {
		t.Errorf("slack head = %q", head)
	}
	pd := reqs[1].body["payload"].(map[string]any)
	if pd["custom_details"].(map[string]any)["why"] != m.Reason.Text || !strings.HasPrefix(pd["summary"].(string), "[CRITICAL] application · api: ") {
		t.Errorf("pagerduty = %v", pd)
	}
	if hook := reqs[2].body; hook["kind"] != "alert" || hook["reason"].(map[string]any)["code"] != ReasonNew || hook["routes"] == nil || hook["digest"] != nil {
		t.Errorf("webhook = %v", hook)
	}
	// A template that can't render is not retried.
	s, _ := Build(config.Notifier{Type: "webhook", Settings: map[string]string{"url": srv.URL, "body_template": "{{ .Event.Nope.X }}"}})
	if err := s.Send(context.Background(), m); err == nil || retryable(err) {
		t.Errorf("template error: %v (retryable %v)", err, retryable(err))
	}
	if !retryable(&HTTPError{Status: 503}) || retryable(Permanent(&HTTPError{Status: 503})) || Permanent(nil) != nil {
		t.Error("Permanent")
	}
	if p, _ := PluginFor("pagerduty"); !p.NoDigest {
		t.Error("PagerDuty takes digests")
	}
	if data, _ := json.Marshal(Plugins()); !strings.Contains(string(data), `"no_digest":true`) {
		t.Error("no_digest isn't in the notifier types")
	}
}

func TestDeliveriesRing(t *testing.T) {
	d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ch := &channel{n: config.Notifier{ID: "n", Name: "n", Type: "fake-test"}}
	for i := 0; i < maxDeliveries+20; i++ {
		d.record(ch, &Message{ID: fmt.Sprint(i), Event: model.Event{ID: fmt.Sprint(i)}}, "sent", "", 1, 0)
	}
	all := d.Deliveries(0)
	if len(all) != maxDeliveries || all[0].ID != fmt.Sprint(maxDeliveries+19) || all[len(all)-1].ID != "20" {
		t.Errorf("ring: %d, newest %s, oldest %s", len(all), all[0].ID, all[len(all)-1].ID)
	}
	if got := d.Deliveries(3); len(got) != 3 || got[2].ID != fmt.Sprint(maxDeliveries+17) {
		t.Errorf("limit: %+v", got)
	}
	if _, _, ok := d.DeliveryDetail("19"); ok {
		t.Error("an evicted delivery was found")
	}
	if del, _, ok := d.DeliveryDetail("20"); !ok || del.ID != "20" {
		t.Error("the oldest kept delivery wasn't found")
	}
}
