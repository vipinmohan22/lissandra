package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenSeedsDefaultsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if len(c.Sources) != 1 || c.Sources[0].IngestToken == "" || len(c.Rules) == 0 {
		t.Fatalf("defaults not seeded: %+v", c.Sources)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %v, want 0600", info.Mode().Perm())
	}

	var notified int
	s.Subscribe(func(*Config) { notified++ })
	if err := s.Update(func(c *Config) error {
		c.Settings.PublicURL = "https://laya.example.com/"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if notified != 2 { // once on subscribe, once on update
		t.Errorf("subscriber called %d times", notified)
	}

	s2, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get().Settings.PublicURL; got != "https://laya.example.com" {
		t.Errorf("public url not persisted/normalized: %q", got)
	}
	if s2.Get().Version != 1 {
		t.Errorf("version = %d", s2.Get().Version)
	}
}

func TestUpdateRejectsInvalidAndKeepsOld(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "c.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(c *Config) error {
		c.Sources = append(c.Sources, Source{Name: "bad", Type: SourcePull, Pull: &Pull{URL: "ftp://x"}})
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("want url error, got %v", err)
	}
	if len(s.Get().Sources) != 1 {
		t.Error("failed update must not change config")
	}
}

func TestSeedFile(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.json")
	os.WriteFile(seed, []byte(`{"sources":[{"name":"from-seed","type":"push","enabled":true}],"settings":{"categories":["A","b","a"]}}`), 0o600)
	s, err := Open(filepath.Join(dir, "config.json"), seed)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if len(c.Sources) != 1 || c.Sources[0].Name != "from-seed" || c.Sources[0].IngestToken == "" {
		t.Errorf("seed not applied: %+v", c.Sources)
	}
	var names []string
	for _, cat := range c.Settings.Categories {
		names = append(names, cat.Name)
	}
	if strings.Join(names, ",") != "a,b" {
		t.Errorf("categories not normalized: %v", c.Settings.Categories)
	}
}

func TestNormalizePullDefaultsAndRouteRefs(t *testing.T) {
	c := &Config{
		Sources:   []Source{{Name: "p", Type: SourcePull, Pull: &Pull{URL: "https://x/logs?since={since_rfc3339}"}}},
		Notifiers: []Notifier{{ID: "n1", Name: "n", Type: "slack"}},
		Routes:    []Route{{Name: "r", NotifierIDs: []string{"n1", "gone"}}},
	}
	if err := Normalize(c); err != nil {
		t.Fatal(err)
	}
	p := c.Sources[0].Pull
	if p.Method != "GET" || p.IntervalSeconds != 60 || p.TimeoutSeconds != 15 || p.Auth.Type != AuthNone {
		t.Errorf("pull defaults: %+v", p)
	}
	if len(c.Routes[0].NotifierIDs) != 1 || c.Routes[0].MinSeverity != "error" {
		t.Errorf("route = %+v", c.Routes[0])
	}
	bad := &Config{Sources: []Source{{Name: "p", Type: SourcePull, Pull: &Pull{URL: "https://x", IntervalSeconds: 1}}}}
	if err := Normalize(bad); err == nil {
		t.Error("interval below 5s should be rejected")
	}
	dup := &Config{Sources: []Source{{Name: "a", Type: SourcePush}, {Name: "a", Type: SourcePush}}}
	if err := Normalize(dup); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate source names should be rejected, got %v", err)
	}
}

func TestSecrets(t *testing.T) {
	t.Setenv("LAYA_TEST_SECRET", "s3cret")
	if got := ExpandEnv("Bearer ${env:LAYA_TEST_SECRET}"); got != "Bearer s3cret" {
		t.Errorf("ExpandEnv = %q", got)
	}
	if MaskValue("${env:X}") != "${env:X}" || MaskValue("plain") != Mask || MaskValue("") != "" {
		t.Error("MaskValue wrong")
	}
	old := Source{Pull: &Pull{Auth: Auth{Token: "tok"}, Headers: map[string]string{"X-Api-Key": "k", "Accept": "json"}}}
	masked := MaskSource(old)
	if masked.Pull.Auth.Token != Mask || masked.Pull.Headers["X-Api-Key"] != Mask || masked.Pull.Headers["Accept"] != "json" {
		t.Errorf("masked = %+v", masked.Pull)
	}
	if old.Pull.Auth.Token != "tok" {
		t.Error("MaskSource must not modify its input")
	}
	RestoreSource(&masked, old)
	if masked.Pull.Auth.Token != "tok" || masked.Pull.Headers["X-Api-Key"] != "k" {
		t.Errorf("restore failed: %+v", masked.Pull)
	}
}

// allowRefs lets ${file:} references read dir for the test.
func allowRefs(t *testing.T, dirs ...string) {
	old := FileRefDirs()
	SetFileRefDirs(dirs)
	t.Cleanup(func() { SetFileRefDirs(old) })
}

func TestFileRefsAndURLMasking(t *testing.T) {
	dir := t.TempDir()
	allowRefs(t, dir)
	path := filepath.Join(dir, "urls")
	os.WriteFile(path, []byte("http://10.0.0.1:8000,http://10.0.0.2:8000\n"), 0o600)
	if got := ExpandEnv("${file:" + path + "}"); got != "http://10.0.0.1:8000,http://10.0.0.2:8000" {
		t.Errorf("file ref: %q", got)
	}
	if got := ExpandEnv("${file:/does/not/exist}"); got != "" {
		t.Errorf("missing file: %q", got)
	}
	t.Setenv("LAYA_TEST_URLS_FILE", path)
	if got := ExpandEnv("${env:LAYA_TEST_URLS}"); got != "http://10.0.0.1:8000,http://10.0.0.2:8000" {
		t.Errorf("NAME_FILE fallback: %q", got)
	}
	t.Setenv("LAYA_TEST_URLS", "")
	if got := ExpandEnv("${env:LAYA_TEST_URLS}"); got != "http://10.0.0.1:8000,http://10.0.0.2:8000" {
		t.Errorf("an empty NAME should fall back to NAME_FILE: %q", got)
	}
	t.Setenv("LAYA_TEST_URLS", "http://set")
	if got := ExpandEnv("${env:LAYA_TEST_URLS}"); got != "http://set" {
		t.Errorf("NAME wins over NAME_FILE: %q", got)
	}
	if MaskValue("${file:/run/secrets/key}") != "${file:/run/secrets/key}" {
		t.Error("a file reference is not itself secret")
	}
	raw := "http://user:s3cret@a:8000, http://b:8000"
	masked := MaskURLs(raw)
	if strings.Contains(masked, "s3cret") || !strings.Contains(masked, "http://b:8000") {
		t.Errorf("masked %q", masked)
	}
	if got, err := RestoreURLs(masked, raw); got != raw || err != nil {
		t.Errorf("restore: %q %v", got, err)
	}
	if got, _ := RestoreURLs("http://c:1", raw); got != "http://c:1" {
		t.Errorf("restore of a new list: %q", got)
	}
}

func TestFileRefsOnlyReadAllowedRegularFiles(t *testing.T) {
	allowed, other := t.TempDir(), t.TempDir()
	allowRefs(t, allowed)
	in := filepath.Join(allowed, "key")
	out := filepath.Join(other, "key")
	os.WriteFile(in, []byte("inside"), 0o600)
	os.WriteFile(out, []byte("outside"), 0o600)
	if ExpandEnv("${file:"+in+"}") != "inside" || ExpandEnv("${file:"+out+"}") != "" {
		t.Error("only files in the allowed directories can be read")
	}
	if ExpandEnv("${file:"+allowed+"/../"+filepath.Base(other)+"/key}") != "" {
		t.Error("a path must not climb out of the allowed directory")
	}
	link := filepath.Join(allowed, "link")
	if os.Symlink(out, link) == nil && ExpandEnv("${file:"+link+"}") != "" {
		t.Error("a symlink must not lead out of the allowed directory")
	}
	if CheckRefs("http://x,${file:"+out+"}") == nil || CheckRefs("${file:"+in+"}") != nil || CheckRefs("${env:X}") != nil {
		t.Error("CheckRefs")
	}
	big := filepath.Join(allowed, "big")
	os.WriteFile(big, make([]byte, maxRefSize+1), 0o600)
	if ExpandEnv("${file:"+big+"}") != "" || ExpandEnv("${file:"+allowed+"}") != "" {
		t.Error("files over 64 KiB and directories read as empty")
	}
	fifo := filepath.Join(allowed, "fifo")
	if syscall.Mkfifo(fifo, 0o600) == nil {
		done := make(chan string)
		go func() { done <- ExpandEnv("${file:" + fifo + "}") }()
		select {
		case got := <-done:
			if got != "" {
				t.Errorf("fifo: %q", got)
			}
		case <-time.After(2 * time.Second):
			t.Error("reading a FIFO reference blocked")
		}
	}
}

func TestURLCredentialsMaskAndRestorePerEntry(t *testing.T) {
	old := "http://laya:s3cret@10.0.0.1:8000"
	masked := MaskURLs(old)
	if masked != "http://laya:xxxxx@10.0.0.1:8000" {
		t.Fatalf("masked %q", masked)
	}
	for _, tc := range []struct{ sent, want string }{
		{masked + ",http://laya:other@10.0.0.2:8000", old + ",http://laya:other@10.0.0.2:8000"}, // an instance added
		{"http://laya:xxxxx@10.0.0.1:9000", "http://laya:s3cret@10.0.0.1:9000"},                 // the port changed
		{"http://laya:xxxxx@10.0.0.3:8000", "http://laya:s3cret@10.0.0.3:8000"},                 // moved: the only credentials
	} {
		if got, err := RestoreURLs(tc.sent, old); got != tc.want || err != nil {
			t.Errorf("RestoreURLs(%q) = %q, %v; want %q", tc.sent, got, err, tc.want)
		}
	}
	two := "http://a:p1@h1:1,http://a:p2@h2:1"
	if _, err := RestoreURLs("http://a:xxxxx@h3:1", two); err == nil {
		t.Error("a masked entry matching two credentials must be refused")
	}
	if _, err := RestoreURLs("http://b:xxxxx@h1:1", two); err == nil {
		t.Error("a masked entry for another user must be refused")
	}
	tok := "https://tok_9f8e7d6c5b4a@engine.example:8443"
	if m := MaskURLs(tok); strings.Contains(m, "tok_") {
		t.Errorf("a user-only credential is shown: %q", m)
	} else if got, _ := RestoreURLs(m, tok); got != tok {
		t.Errorf("restore of a token: %q", got)
	}
	pull := "https://svc:pw@logs.example/api/{since_unix}?from={since_rfc3339}"
	if m := MaskURLs(pull); m != "https://svc:xxxxx@logs.example/api/{since_unix}?from={since_rfc3339}" {
		t.Errorf("placeholders must survive masking: %q", m)
	} else if got, _ := RestoreURLs(m, pull); got != pull {
		t.Errorf("restore of a pull URL: %q", got)
	}
}

func TestCategoryLimitsApplyToDuplicates(t *testing.T) {
	c := Defaults()
	c.Settings.Categories = []Category{{Name: "x"}, {Name: "X", Description: strings.Repeat("d", 1000)}}
	if err := Normalize(c); err == nil {
		t.Error("a long description must be refused on a repeated name too")
	}
}

func TestEngineCheckpointNamesAreLowercase(t *testing.T) {
	if m, err := NormalizeEngineModel(" English "); m != "english" || err != nil {
		t.Errorf("%q %v", m, err)
	}
	if m, _ := NormalizeEngineModel("My-Logs"); m != "my-logs" {
		t.Errorf("%q", m)
	}
}

func TestCategoryLimitsCountCharacters(t *testing.T) {
	c := Defaults()
	c.Settings.Categories = []Category{{Name: strings.Repeat("я", 40), Description: strings.Repeat("д", 200)}}
	if err := Normalize(c); err != nil {
		t.Errorf("40 and 200 characters are within the limits: %v", err)
	}
	c = Defaults()
	c.Settings.Categories = []Category{{Name: "x", Description: strings.Repeat("д", 301)}}
	if err := Normalize(c); err == nil || !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("301 characters: %v", err)
	}
}

// A config written before the engine existed (settings.ai, no
// settings.engine) gets the engine's defaults, not zero values.
func TestOldConfigGetsEngineDefaults(t *testing.T) {
	old := `{"version": 3, "settings": {"categories": ["database", "network"], "ai": {"enabled": false, "model": "x"}}, "sources": [], "rules": [], "notifiers": [], "routes": []}`
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(old), 0o600)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e, d := c.Settings.Engine, Defaults().Settings.Engine
	if !e.Enabled || e.URL != d.URL || e.APIKey != d.APIKey || e.MinConfidence != 0.6 || !e.OnlyUncategorized || e.MaxInFlight != 1 {
		t.Errorf("engine settings of an old config: %+v", e)
	}
	if len(c.Settings.Categories) != 2 {
		t.Errorf("its categories are kept: %+v", c.Settings.Categories)
	}
}

// A symlink inside an allowed directory followed by ".." can't lead out:
// paths with ".." are refused, and the resolved path is what is read.
func TestFileRefSymlinkEscape(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	allowRefs(t, allowed)
	os.MkdirAll(filepath.Join(outside, "deep"), 0o700)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("TOP-SECRET"), 0o600)
	os.Symlink(filepath.Join(outside, "deep"), filepath.Join(allowed, "lnk"))
	ref := "${file:" + allowed + "/lnk/../secret}"
	if CheckRefs(ref) == nil || ExpandEnv(ref) != "" {
		t.Errorf("escaped: CheckRefs=%v ExpandEnv=%q", CheckRefs(ref), ExpandEnv(ref))
	}
	os.WriteFile(filepath.Join(outside, "deep", "f"), []byte("deep"), 0o600)
	if ExpandEnv("${file:"+allowed+"/lnk/f}") != "" {
		t.Error("a symlinked directory leading out must not be read")
	}
}

// A config saved with a reference outside the allowed directories (before
// the check existed) still loads; the reference reads as empty and is
// reported. Only newly added references are refused.
func TestOldFileRefsLoadButNewOnesAreRefused(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	allowRefs(t, allowed)
	c := Defaults()
	c.Settings.Engine.APIKey = "${file:" + outside + "/key}"
	if err := Normalize(c); err != nil {
		t.Fatalf("a reference is not refused when loading: %v", err)
	}
	if w := FileRefWarnings(c); len(w) != 1 || !strings.Contains(w[0], "reads as empty") {
		t.Errorf("warnings: %v", w)
	}
	next := clone(c)
	if err := CheckNewRefs(c, next); err != nil {
		t.Errorf("an existing reference is kept: %v", err)
	}
	next.Settings.Engine.URL = "${file:" + outside + "/urls}"
	if err := CheckNewRefs(c, next); err == nil {
		t.Error("a new reference outside the allowed directories must be refused")
	}
	next.Settings.Engine.URL = "${file:" + allowed + "/urls}"
	if err := CheckNewRefs(c, next); err != nil {
		t.Errorf("a new allowed reference: %v", err)
	}
}

func TestSingleURLMasking(t *testing.T) {
	raw := "https://svc:pa,ss-secret@logs.example/api?since={since_unix}"
	m := MaskURL(raw)
	if strings.Contains(m, "ss-secret") || m != "https://svc:xxxxx@logs.example/api?since={since_unix}" {
		t.Errorf("masked %q", m)
	}
	if got, err := RestoreURL(m, raw); got != raw || err != nil {
		t.Errorf("restored %q %v", got, err)
	}
}

// A category decoded over an existing one doesn't keep fields the object
// leaves out (a partial settings update).
func TestCategoryDecodesFresh(t *testing.T) {
	cats := []Category{{Name: "security", Description: "attacks and intrusions"}, {Name: "database"}}
	if err := json.Unmarshal([]byte(`[{"name":"database"}]`), &cats); err != nil {
		t.Fatal(err)
	}
	if len(cats) != 1 || cats[0] != (Category{Name: "database"}) {
		t.Errorf("%+v", cats)
	}
}

// Engine settings a config has (an api_key the operator cleared) are kept;
// only a config without engine settings gets the defaults.
func TestDecodeKeepsEngineSettingsItHas(t *testing.T) {
	var c Config
	if err := Decode([]byte(`{"settings": {"engine": {"enabled": true, "url": "http://e:8000", "min_confidence": 0.7}}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Settings.Engine.APIKey != "" || c.Settings.Engine.MinConfidence != 0.7 {
		t.Errorf("%+v", c.Settings.Engine)
	}
}

// References are looked for only where they are expanded: a rule matching
// the literal text "${file:…}" is fine, and so is a directory with "&".
func TestFileRefsOnlyInExpandedFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a&b")
	os.MkdirAll(dir, 0o700)
	allowRefs(t, dir)
	c := Defaults()
	next := clone(c)
	next.Rules = append(next.Rules, Rule{Name: "literal", Enabled: true, Conditions: []Condition{{Field: "message", Op: "contains", Value: "${file:/etc/passwd}"}}})
	next.Settings.Engine.APIKey = "${file:" + dir + "/key}"
	if err := CheckNewRefs(c, next); err != nil {
		t.Errorf("%v", err)
	}
	var d Config
	if err := Decode([]byte(`{"settings": {"engine": null}}`), &d); err != nil || d.Settings.Engine.MinConfidence != 0.6 {
		t.Errorf("engine null: %+v %v", d.Settings.Engine, err)
	}
}
