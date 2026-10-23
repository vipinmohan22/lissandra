package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vipinmohan22/layalogs/internal/auth"
	"github.com/vipinmohan22/layalogs/internal/config"
)

const permanentToken = "9b2e7f41c08a3d65e1f4a7c2b90d3e58f6a1c4b7d2e09f38"

// syncBuffer collects log output written from the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// authEnv is an env whose admin token comes from admin-token.json, as
// main sets it up.
type authEnv struct {
	*env
	file  string
	store *auth.Store
	token string // the token to sign in with
	logs  *syncBuffer
}

// withAuth opens the admin token as main does (envToken "" generates one)
// and starts a server with it.
func withAuth(t *testing.T, envToken string) *authEnv {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	file := filepath.Join(t.TempDir(), "admin-token.json")
	if envToken != "" {
		// A token set in Laya earlier, which the env token overrides.
		s, gen, err := auth.Open(file, "", log)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Rotate(gen, permanentToken); err != nil {
			t.Fatal(err)
		}
	}
	store, gen, err := auth.Open(file, envToken, log)
	if err != nil {
		t.Fatal(err)
	}
	token := gen
	if envToken != "" {
		token = envToken
	}
	e := newEnvFrom(t, envConfig{log: log, mutate: func(o *Options) { o.Auth = store }})
	return &authEnv{env: e, file: file, store: store, token: token, logs: logs}
}

// as sends a request with token and decodes the response into out.
func (e *env) as(token, method, path string, body, out any) (int, []byte) {
	e.t.Helper()
	code, data := e.do(method, path, body, map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return code, data
}

// apiErr sends a request with token and returns the status and the error code.
func (e *env) apiErr(token, method, path string, body any) (int, string) {
	e.t.Helper()
	var resp struct {
		Code string `json:"code"`
	}
	code, data := e.do(method, path, body, map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
	json.Unmarshal(data, &resp)
	return code, resp.Code
}

type stepResponse struct {
	State   onboardingView  `json:"state"`
	Changes []config.Change `json:"changes"`
}

func (e *env) step(step string, body any) (int, stepResponse) {
	e.t.Helper()
	var r stepResponse
	code := e.admin("POST", "/api/onboarding/steps/"+step, body, &r)
	return code, r
}

func (e *env) onboarding() onboardingView {
	e.t.Helper()
	var v onboardingView
	if code := e.admin("GET", "/api/onboarding", nil, &v); code != 200 {
		e.t.Fatalf("GET /api/onboarding: %d", code)
	}
	return v
}

var setupEndpoints = []struct{ method, path string }{
	{"GET", "/api/auth"}, {"PUT", "/api/auth/token"}, {"DELETE", "/api/auth/token"},
	{"GET", "/api/onboarding"}, {"POST", "/api/onboarding/start"}, {"POST", "/api/onboarding/steps/sources"},
	{"POST", "/api/onboarding/defaults"}, {"POST", "/api/onboarding/defer"}, {"POST", "/api/onboarding/complete"},
	{"POST", "/api/onboarding/dismiss"},
}

func TestSetupEndpointsNeedTheAdminToken(t *testing.T) {
	e := withAuth(t, "")
	for _, ep := range setupEndpoints {
		for _, h := range []map[string]string{nil, {"Authorization": "Bearer wrong"}, {"Authorization": e.token}} {
			if code, _ := e.do(ep.method, ep.path, `{"token":"`+permanentToken+`"}`, h); code != http.StatusUnauthorized {
				t.Errorf("%s %s with %v: %d", ep.method, ep.path, h, code)
			}
		}
	}
	if e.store.Status().Source != auth.SourceGenerated {
		t.Error("an unauthenticated request changed the token")
	}
}

func TestAdminTokenChange(t *testing.T) {
	e := withAuth(t, "")
	var st authView
	if code, _ := e.as(e.token, "GET", "/api/auth", nil, &st); code != 200 || st.Source != auth.SourceGenerated || st.Persistent || st.File != e.file {
		t.Fatalf("GET /api/auth: %d %+v", code, st)
	}

	if code, c := e.apiErr(e.token, "PUT", "/api/auth/token", map[string]string{"token": "too-short"}); code != 400 || c != "weak_token" {
		t.Errorf("weak: %d %s", code, c)
	}
	if code, c := e.apiErr(e.token, "PUT", "/api/auth/token", map[string]any{"generate": true}); code != 400 || c != "invalid_json" {
		t.Errorf("an unknown field: %d %s", code, c)
	}
	if code, c := e.apiErr(e.token, "PUT", "/api/auth/token", map[string]string{"token": e.token}); code != 400 || c != "same_token" {
		t.Errorf("the logged token: %d %s", code, c)
	}

	code, body := e.as(e.token, "PUT", "/api/auth/token", map[string]string{"token": permanentToken}, &st)
	if code != 200 || st.Source != auth.SourceStored || !st.Persistent || st.SetAt == nil {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if code, _ := e.as(e.token, "GET", "/api/info", nil, nil); code != 401 {
		t.Errorf("the old token still works: %d", code)
	}
	if code, _ := e.as(permanentToken, "GET", "/api/info", nil, nil); code != 200 {
		t.Errorf("the new token doesn't work: %d", code)
	}

	// Nothing shows the token or what the file keeps of it.
	var file struct{ Key, Digest []byte }
	data, _ := os.ReadFile(e.file)
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	secrets := []string{permanentToken, b64(file.Key), b64(file.Digest)}
	_, exported := e.as(permanentToken, "GET", "/api/config/export?include_secrets=true", nil, nil)
	shown := [][]byte{body, exported, []byte(e.logs.String())}
	for _, path := range []string{"/api/auth", "/api/onboarding", "/api/info"} {
		_, b := e.as(permanentToken, "GET", path, nil, nil)
		shown = append(shown, b)
	}
	for i, b := range shown {
		for _, sec := range secrets {
			if bytes.Contains(b, []byte(sec)) {
				t.Errorf("response %d shows %.12s…: %s", i, sec, b)
			}
		}
	}
	if !strings.Contains(e.logs.String(), "admin token changed in Laya") {
		t.Error("the change was not logged")
	}

	// A restart keeps it.
	again, gen, err := auth.Open(e.file, "", slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil || gen != "" || !again.Check(permanentToken) || again.Status().Source != auth.SourceStored {
		t.Errorf("after a restart: %v", err)
	}

	if code, c := newEnv(t).apiErr(adminToken, "PUT", "/api/auth/token", map[string]string{"token": permanentToken}); code != 409 || c != "env_managed" {
		t.Errorf("with LAYA_ADMIN_TOKEN: %d %s", code, c)
	}
}

func b64(b []byte) string {
	s, _ := json.Marshal(b)
	return strings.Trim(string(s), `"`)
}

func TestConcurrentAdminTokenChanges(t *testing.T) {
	e := withAuth(t, "")
	next := []string{permanentToken, strings.ToUpper(permanentToken)}
	codes := make([]int, len(next))
	var wg sync.WaitGroup
	for i := range next {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = e.as(e.token, "PUT", "/api/auth/token", map[string]string{"token": next[i]}, nil)
		}()
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{200, 401}) {
		t.Fatalf("codes = %v", codes)
	}
	if n := btoi(e.store.Check(next[0])) + btoi(e.store.Check(next[1])); n != 1 {
		t.Errorf("%d tokens work", n)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestDeleteStoredAdminToken(t *testing.T) {
	gen := withAuth(t, "")
	if code, c := gen.apiErr(gen.token, "DELETE", "/api/auth/token", nil); code != 409 || c != "not_env_managed" {
		t.Errorf("without LAYA_ADMIN_TOKEN: %d %s", code, c)
	}

	e := withAuth(t, "env-token-0123456789abcdef")
	var st authView
	if e.as(e.token, "GET", "/api/auth", nil, &st); st.Source != auth.SourceEnv || !st.StoredIgnored || st.StoredSetAt == nil || st.Changeable {
		t.Fatalf("status = %+v", st)
	}
	if code, _ := e.as(permanentToken, "GET", "/api/info", nil, nil); code != 401 {
		t.Errorf("the stored token works while LAYA_ADMIN_TOKEN is set: %d", code)
	}
	if code, _ := e.as(e.token, "DELETE", "/api/auth/token", nil, nil); code != 204 {
		t.Fatalf("DELETE: %d", code)
	}
	if _, err := os.Stat(e.file); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}
	if e.as(e.token, "GET", "/api/auth", nil, &st); st.StoredIgnored {
		t.Errorf("status = %+v", st)
	}
	if code, c := e.apiErr(e.token, "DELETE", "/api/auth/token", nil); code != 404 || c != "no_stored_token" {
		t.Errorf("a second DELETE: %d %s", code, c)
	}
}

func TestOnboardingLifecycle(t *testing.T) {
	e := newEnv(t)
	v := e.onboarding()
	if v.Status != config.OnboardingPending || v.Resume != pageWelcome || !slices.Equal(v.NotNeeded, []string{"notify"}) ||
		!slices.Equal(v.Order, config.OnboardingSteps) || len(v.Steps) != 0 || v.StartedAt == nil || v.FinishedAt != nil {
		t.Fatalf("fresh: %+v", v)
	}
	// LAYA_ADMIN_TOKEN is permanent: access isn't left to do.
	if !slices.Equal(v.Remaining, []string{"sources", "classify", "channels"}) || v.Auth.Source != auth.SourceEnv {
		t.Errorf("remaining = %v", v.Remaining)
	}
	version := e.cfg.Get().Version

	if code, r := e.step("access", map[string]string{"outcome": "done"}); code != 200 || r.State.Resume != "sources" || r.Changes == nil {
		t.Errorf("access: %d %+v", code, r)
	}
	if code, r := e.step("sources", map[string]string{"outcome": "skipped"}); code != 200 || r.State.Resume != "classify" ||
		!slices.Contains(r.State.Remaining, "sources") || r.State.Steps["sources"].Outcome != "skipped" {
		t.Errorf("sources skipped: %d %+v", code, r)
	}
	if code, r := e.step("sources", map[string]string{"outcome": "done"}); code != 200 || slices.Contains(r.State.Remaining, "sources") {
		t.Errorf("sources done: %d %+v", code, r)
	}
	if v := e.cfg.Get().Version; v != version {
		t.Errorf("recording steps bumped the version: %d → %d", version, v)
	}

	for body, want := range map[string]string{`{"outcome":"maybe"}`: "invalid_outcome", `{"outcome":"done","dry_run":true}`: "invalid_outcome", `{}`: "invalid_outcome", ``: "invalid_json", `{"outcome":"done","x":1}`: "invalid_json"} {
		if code, c := e.apiErr(adminToken, "POST", "/api/onboarding/steps/classify", body); code != 400 || c != want {
			t.Errorf("%s: %d %s", body, code, c)
		}
	}
	if code, c := e.apiErr(adminToken, "POST", "/api/onboarding/steps/telemetry", map[string]string{"outcome": "done"}); code != 404 || c != "unknown_step" {
		t.Errorf("unknown step: %d %s", code, c)
	}

	// Defaults: a dry run first, then the change, then a repeat that is a no-op.
	e.cfg.Update(func(c *config.Config) error {
		c.Settings.Categories = slices.DeleteFunc(c.Settings.Categories, func(x config.Category) bool { return x.Name == "audit" })
		return nil
	})
	version = e.cfg.Get().Version
	var dry struct {
		State   *onboardingView `json:"state"`
		Changes []config.Change `json:"changes"`
	}
	if code := e.admin("POST", "/api/onboarding/steps/classify", map[string]any{"outcome": "defaults", "dry_run": true}, &dry); code != 200 ||
		dry.State != nil || len(dry.Changes) != 1 || dry.Changes[0] != (config.Change{Kind: "category", Name: "audit"}) {
		t.Errorf("dry run: %d %+v", code, dry)
	}
	if e.cfg.Get().Version != version || slices.ContainsFunc(e.cfg.Get().Settings.Categories, func(x config.Category) bool { return x.Name == "audit" }) {
		t.Error("a dry run changed the config")
	}
	if code, r := e.step("classify", map[string]string{"outcome": "defaults"}); code != 200 || len(r.Changes) != 1 || r.State.Steps["classify"].Outcome != "defaults" {
		t.Errorf("classify defaults: %d %+v", code, r)
	}
	if v := e.cfg.Get().Version; v != version+1 {
		t.Errorf("version %d, want %d", v, version+1)
	}
	code, r := e.step("classify", map[string]string{"outcome": "defaults"})
	if code != 200 || r.Changes == nil || len(r.Changes) != 0 || e.cfg.Get().Version != version+1 {
		t.Errorf("a second call: %d %+v, version %d", code, r, e.cfg.Get().Version)
	}
	if code, c := e.apiErr(adminToken, "POST", "/api/onboarding/steps/notify", map[string]string{"outcome": "defaults"}); code != 400 || c != "needs_channel" {
		t.Errorf("notify defaults without a channel: %d %s", code, c)
	}

	// A channel makes notify needed, and its defaults route to it.
	var n config.Notifier
	e.admin("POST", "/api/notifiers", map[string]any{"name": "Ops", "type": "webhook", "enabled": true, "settings": map[string]string{"url": "https://hooks.example.com/laya"}}, &n)
	if v := e.onboarding(); len(v.NotNeeded) != 0 || v.Resume != "channels" || !slices.Contains(v.Remaining, "notify") {
		t.Errorf("with a channel: %+v", v)
	}
	e.step("channels", map[string]string{"outcome": "done"})
	if code, r := e.step("notify", map[string]string{"outcome": "defaults"}); code != 200 || len(r.Changes) != 1 || r.Changes[0].Name != "Ops: error and above" ||
		r.State.Resume != pageReview || len(r.State.Remaining) != 0 {
		t.Errorf("notify defaults: %d %+v", code, r)
	}
	if rs := e.cfg.Get().Routes; len(rs) != 1 || !slices.Equal(rs[0].NotifierIDs, []string{n.ID}) || rs[0].ThrottleSeconds != 900 {
		t.Errorf("routes = %+v", rs)
	}
}

func TestAccessStepNeedsAPermanentToken(t *testing.T) {
	e := withAuth(t, "")
	if code, c := e.apiErr(e.token, "POST", "/api/onboarding/steps/access", map[string]string{"outcome": "done"}); code != 409 || c != "token_not_set" {
		t.Errorf("done with a generated token: %d %s", code, c)
	}
	if code, c := e.apiErr(e.token, "POST", "/api/onboarding/steps/access", map[string]string{"outcome": "defaults"}); code != 409 || c != "token_not_set" {
		t.Errorf("defaults with a generated token: %d %s", code, c)
	}
	var r stepResponse
	if code, _ := e.as(e.token, "POST", "/api/onboarding/steps/access", map[string]string{"outcome": "skipped"}, &r); code != 200 || !slices.Contains(r.State.Remaining, "access") {
		t.Errorf("skipped: %d %+v", code, r)
	}
	if code, _ := e.as(e.token, "PUT", "/api/auth/token", map[string]string{"token": permanentToken}, nil); code != 200 {
		t.Fatal(code)
	}
	if code, _ := e.as(permanentToken, "POST", "/api/onboarding/steps/access", map[string]string{"outcome": "done"}, &r); code != 200 ||
		r.State.Steps["access"].Outcome != "done" || r.State.Auth.Source != auth.SourceStored || slices.Contains(r.State.Remaining, "access") {
		t.Errorf("after setting a token: %d %+v", code, r)
	}
}

func TestOnboardingStatusMachine(t *testing.T) {
	e := newEnv(t)
	post := func(path string) (int, string, onboardingView) {
		t.Helper()
		var v onboardingView
		code, data := e.as(adminToken, "POST", "/api/onboarding/"+path, nil, nil)
		var errBody struct{ Code string }
		json.Unmarshal(data, &errBody)
		json.Unmarshal(data, &v)
		return code, errBody.Code, v
	}
	version := e.cfg.Get().Version
	if code, _, v := post("defer"); code != 200 || v.Status != config.OnboardingDeferred {
		t.Errorf("defer: %d %+v", code, v)
	}
	if code, _, v := post("defer"); code != 200 || v.Status != config.OnboardingDeferred {
		t.Errorf("defer again: %d %+v", code, v)
	}
	if code, _ := e.step("sources", map[string]string{"outcome": "done"}); code != 200 {
		t.Errorf("a step while deferred: %d", code)
	}
	code, _, v := post("complete")
	if code != 200 || v.Status != config.OnboardingCompleted || v.FinishedAt == nil {
		t.Errorf("complete: %d %+v", code, v)
	}
	if code, _, v := post("complete"); code != 200 || v.Status != config.OnboardingCompleted {
		t.Errorf("complete again: %d %+v", code, v)
	}
	for _, path := range []string{"defer", "dismiss", "defaults"} {
		if code, c, _ := post(path); code != 409 || c != "setup_not_running" {
			t.Errorf("%s after complete: %d %s", path, code, c)
		}
	}
	if code, c := e.apiErr(adminToken, "POST", "/api/onboarding/steps/sources", map[string]string{"outcome": "done"}); code != 409 || c != "setup_not_running" {
		t.Errorf("a step after complete: %d %s", code, c)
	}
	if code, _ := e.step("classify", map[string]any{"outcome": "defaults", "dry_run": true}); code != 200 {
		t.Errorf("a dry run is not refused: %d", code)
	}

	if code, _, v := post("start"); code != 200 || v.Status != config.OnboardingPending || len(v.Steps) != 0 || v.FinishedAt != nil || v.Resume != pageWelcome {
		t.Errorf("start after complete: %d %+v", code, v)
	}
	e.step("sources", map[string]string{"outcome": "done"})
	if code, _, v := post("start"); code != 200 || v.Steps["sources"].Outcome != "done" {
		t.Errorf("start while running keeps the steps: %d %+v", code, v)
	}
	if code, _, v := post("dismiss"); code != 200 || v.Status != config.OnboardingDismissed || v.FinishedAt == nil {
		t.Errorf("dismiss: %d %+v", code, v)
	}
	if code, _, _ := post("dismiss"); code != 200 {
		t.Errorf("dismiss again: %d", code)
	}
	if code, c, _ := post("complete"); code != 409 || c != "setup_not_running" {
		t.Errorf("complete after dismiss: %d %s", code, c)
	}
	if v := e.cfg.Get().Version; v != version {
		t.Errorf("status changes bumped the version: %d → %d", version, v)
	}
}

func TestOnboardingDefaultsForEverything(t *testing.T) {
	for _, envToken := range []string{"", "env-token-0123456789abcdef"} {
		e := withAuth(t, envToken)
		e.cfg.Update(func(c *config.Config) error {
			c.Sources = nil
			c.Settings.Categories = c.Settings.Categories[:3]
			return nil
		})
		version := e.cfg.Get().Version
		var dry stepResponse
		if code, _ := e.as(e.token, "POST", "/api/onboarding/defaults", map[string]bool{"dry_run": true}, &dry); code != 200 || len(dry.Changes) != 8 {
			t.Errorf("dry run: %d %+v", code, dry)
		}
		if e.cfg.Get().Version != version || len(e.cfg.Get().Sources) != 0 {
			t.Error("a dry run changed the config")
		}
		var r stepResponse
		if code, _ := e.as(e.token, "POST", "/api/onboarding/defaults", nil, &r); code != 200 || len(r.Changes) != 8 || r.Changes[0] != (config.Change{Kind: "source", Name: "default"}) {
			t.Fatalf("apply: %d %+v", code, r)
		}
		access := "done"
		if envToken == "" {
			access = "skipped"
		}
		st := r.State.Steps
		if st["access"].Outcome != access || st["sources"].Outcome != "defaults" || st["classify"].Outcome != "defaults" || st["channels"].Outcome != "defaults" {
			t.Errorf("env %q: steps = %+v", envToken, st)
		}
		if _, ok := st["notify"]; ok || r.State.Resume != pageReview {
			t.Errorf("notify without a channel: %+v", r.State)
		}
		c := e.cfg.Get()
		if c.Version != version+1 || len(c.Sources) != 1 || len(c.Settings.Categories) != len(config.DefaultCategories) {
			t.Errorf("config: version %d, %d sources, %d categories", c.Version, len(c.Sources), len(c.Settings.Categories))
		}
		if code, _ := e.as(e.token, "POST", "/api/onboarding/defaults", map[string]bool{"dry_run": false}, &r); code != 200 || len(r.Changes) != 0 || e.cfg.Get().Version != version+1 {
			t.Errorf("a repeat: %d %+v", code, r)
		}
	}
}

func TestInfoCarriesSetupSummary(t *testing.T) {
	e := withAuth(t, "")
	var info struct {
		Onboarding struct {
			Status, Resume   string
			Remaining, Total int
		} `json:"onboarding"`
		Auth struct {
			Source                 auth.Source
			Changeable, Persistent bool
		} `json:"auth"`
	}
	e.as(e.token, "GET", "/api/info", nil, &info)
	if o := info.Onboarding; o.Status != "pending" || o.Resume != pageWelcome || o.Remaining != 4 || o.Total != 4 {
		t.Errorf("onboarding = %+v", o)
	}
	if a := info.Auth; a.Source != auth.SourceGenerated || !a.Changeable || a.Persistent {
		t.Errorf("auth = %+v", a)
	}
	e.as(e.token, "POST", "/api/onboarding/steps/sources", map[string]string{"outcome": "done"}, nil)
	e.as(e.token, "GET", "/api/info", nil, &info)
	if o := info.Onboarding; o.Resume != "access" || o.Remaining != 3 {
		t.Errorf("after a step: %+v", o)
	}
}

func TestExistingInstallIsNotOffered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := config.Defaults()
	if err := config.Normalize(old); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnvFrom(t, envConfig{configPath: path})
	if v := e.onboarding(); v.Status != "none" || v.Resume != pageWelcome {
		t.Errorf("an existing config: %+v", v)
	}
	if code, c := e.apiErr(adminToken, "POST", "/api/onboarding/steps/sources", map[string]string{"outcome": "done"}); code != 409 || c != "setup_not_running" {
		t.Errorf("a step: %d %s", code, c)
	}
	for _, path := range []string{"defer", "complete", "dismiss"} {
		if code, c := e.apiErr(adminToken, "POST", "/api/onboarding/"+path, nil); code != 409 || c != "setup_not_running" {
			t.Errorf("%s: %d %s", path, code, c)
		}
	}
	var v onboardingView
	if code := e.admin("POST", "/api/onboarding/start", nil, &v); code != 200 || v.Status != config.OnboardingPending || v.StartedAt == nil {
		t.Errorf("start: %d %+v", code, v)
	}
}

func TestExportStripsAndImportKeepsSetupState(t *testing.T) {
	e := withAuth(t, "")
	if code, _ := e.as(e.token, "PUT", "/api/auth/token", map[string]string{"token": permanentToken}, nil); code != 200 {
		t.Fatal(code)
	}
	e.as(permanentToken, "POST", "/api/onboarding/steps/sources", map[string]string{"outcome": "done"}, nil)
	_, exported := e.as(permanentToken, "GET", "/api/config/export?include_secrets=true", nil, nil)
	var doc map[string]any
	if err := json.Unmarshal(exported, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["onboarding"]; ok {
		t.Errorf("the export has setup state: %v", doc["onboarding"])
	}
	doc["onboarding"] = map[string]any{"status": "completed", "steps": map[string]any{"classify": map[string]string{"outcome": "done"}}}
	if code, body := e.as(permanentToken, "POST", "/api/config/import", doc, nil); code != 200 {
		t.Fatalf("import: %d %s", code, body)
	}
	o := e.cfg.Get().Onboarding
	if o == nil || o.Status != config.OnboardingPending || len(o.Steps) != 1 || o.Steps["sources"].Outcome != "done" {
		t.Errorf("after an import: %+v", o)
	}
	if code, _ := e.as(permanentToken, "GET", "/api/info", nil, nil); code != 200 {
		t.Errorf("the stored token after an import: %d", code)
	}
}

func TestSettingsCannotTouchOnboarding(t *testing.T) {
	e := newEnv(t)
	before := e.onboarding()
	body := `{"public_url":"https://laya.example.com","onboarding":{"status":"completed"}}`
	if code := e.admin("PUT", "/api/settings", body, nil); code != 200 {
		t.Fatal(code)
	}
	if after := e.onboarding(); after.Status != before.Status || e.cfg.Get().Settings.PublicURL != "https://laya.example.com" {
		t.Errorf("setup state = %+v", after)
	}
}

func TestConcurrentStepRecords(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	codes := make([]int, len(config.OnboardingSteps))
	for i, step := range config.OnboardingSteps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = e.step(step, map[string]string{"outcome": "done"})
		}()
	}
	wg.Wait()
	steps := e.cfg.Get().Onboarding.Steps
	if len(steps) != len(config.OnboardingSteps) || slices.ContainsFunc(codes, func(c int) bool { return c != 200 }) {
		t.Errorf("codes %v, steps %+v", codes, steps)
	}
}

func TestNotifierTypesShowRecipients(t *testing.T) {
	e := newEnv(t)
	_, body := e.as(adminToken, "GET", "/api/notifier-types", nil, nil)
	if !bytes.Contains(body, []byte(`"recipient":true`)) || !bytes.Contains(body, []byte(`"audience":`)) {
		t.Errorf("notifier types: %.300s", body)
	}
}
