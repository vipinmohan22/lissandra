package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if strings.Join(c.Settings.Categories, ",") != "a,b" {
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
