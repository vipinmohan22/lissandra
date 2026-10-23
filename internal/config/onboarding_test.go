package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/model"
)

func TestFreshConfigStartsOnboarding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	o := s.Get().Onboarding
	if o == nil || o.Status != OnboardingPending || o.StartedAt == nil || o.Steps != nil {
		t.Fatalf("onboarding = %+v", o)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Onboarding == nil || c.Onboarding.Status != OnboardingPending || !c.Onboarding.StartedAt.Equal(*o.StartedAt) {
		t.Errorf("not persisted: %+v", c.Onboarding)
	}
	if Defaults().Onboarding != nil {
		t.Error("Defaults() must not offer setup: lissandra analyze and the engine-defaults probe use it")
	}
}

func TestExistingConfigHasNoOnboarding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(withoutOnboarding(Defaults()))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if o := s.Get().Onboarding; o != nil {
		t.Fatalf("an existing config was offered setup: %+v", o)
	}
	if err := s.Update(func(c *Config) error {
		c.Settings.PublicURL = "https://lissandra.example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().Onboarding != nil || c.Onboarding != nil {
		t.Errorf("an update added setup state: %+v", c.Onboarding)
	}
}

func TestSeedOnboarding(t *testing.T) {
	open := func(t *testing.T, seed string) *Onboarding {
		t.Helper()
		dir := t.TempDir()
		seedPath := filepath.Join(dir, "seed.json")
		if seed != "" {
			if err := os.WriteFile(seedPath, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		s, err := Open(filepath.Join(dir, "config.json"), seedPath)
		if err != nil {
			t.Fatal(err)
		}
		return s.Get().Onboarding
	}
	if o := open(t, `{"sources":[]}`); o != nil {
		t.Errorf("a seed without the key: %+v", o)
	}
	if o := open(t, `{"onboarding":{}}`); o == nil || o.Status != OnboardingPending || o.StartedAt == nil {
		t.Errorf(`a seed with "onboarding": {}: %+v`, o)
	}
	o := open(t, `{"onboarding":{"steps":{"sources":{"outcome":"done"},"classify":{"outcome":"defaults","at":"2026-01-02T03:04:05Z"}}}}`)
	if o == nil || o.Status != OnboardingPending || len(o.Steps) != 2 || o.Steps[StepSources].Outcome != OutcomeDone ||
		o.Steps[StepSources].At.IsZero() || o.Steps[StepClassify].At.Year() != 2026 {
		t.Errorf("a seed with steps: %+v", o)
	}
	if o := open(t, ""); o == nil || o.Status != OnboardingPending {
		t.Errorf("a missing seed falls back to the defaults, which offer setup: %+v", o)
	}
}

func TestNormalizeOnboardingTolerant(t *testing.T) {
	c := Defaults()
	c.Onboarding = &Onboarding{Status: "from-the-future", Steps: map[string]OnboardingStep{
		StepAccess: {Outcome: OutcomeDone}, "telemetry": {Outcome: OutcomeDone}, StepNotify: {Outcome: "maybe"},
	}}
	if err := Normalize(c); err != nil {
		t.Fatalf("setup state must never fail validation: %v", err)
	}
	o := clone(c).Onboarding
	if o.Status != OnboardingDismissed || len(o.Steps) != 1 || o.Steps[StepAccess].Outcome != OutcomeDone {
		t.Errorf("onboarding = %+v", o)
	}
	c.Onboarding = &Onboarding{Status: OnboardingDeferred, Steps: map[string]OnboardingStep{"x": {Outcome: OutcomeDone}}}
	if err := Normalize(c); err != nil || c.Onboarding.Status != OnboardingDeferred || c.Onboarding.Steps != nil {
		t.Errorf("onboarding = %+v, %v", c.Onboarding, err)
	}
}

func TestOnboardingOnlyUpdateIsQuiet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	var notified int
	s.Subscribe(func(*Config) { notified++ })
	notified = 0

	if err := s.Update(func(c *Config) error {
		RecordStep(c, StepSources, OutcomeSkipped, time.Now().UTC())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v := s.Get().Version; v != 0 || notified != 0 {
		t.Errorf("a setup-only update: version %d, %d notifications", v, notified)
	}
	if c, err := Load(path); err != nil || c.Onboarding.Steps[StepSources].Outcome != OutcomeSkipped {
		t.Errorf("not saved: %v", err)
	}

	if err := s.Update(func(c *Config) error {
		RecordStep(c, StepClassify, OutcomeDone, time.Now().UTC())
		c.Rules[0].Enabled = !c.Rules[0].Enabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v := s.Get().Version; v != 1 || notified != 1 {
		t.Errorf("a rule edit with setup state: version %d, %d notifications", v, notified)
	}

	if err := s.Update(func(c *Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if v := s.Get().Version; v != 2 || notified != 2 {
		t.Errorf("a no-op update: version %d, %d notifications", v, notified)
	}
}

func names[T any](items []T, name func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = name(it)
	}
	return out
}

func catName(c Category) string { return c.Name }

func TestApplyStepDefaults(t *testing.T) {
	apply := func(t *testing.T, c *Config, step string) []Change {
		t.Helper()
		changes, err := ApplyStepDefaults(c, step)
		if err != nil {
			t.Fatal(err)
		}
		if changes == nil {
			t.Fatal("changes must be [] rather than nil")
		}
		if err := Normalize(c); err != nil {
			t.Fatal(err)
		}
		return changes
	}

	t.Run("sources", func(t *testing.T) {
		c := Defaults()
		if ch := apply(t, c, StepSources); len(ch) != 0 {
			t.Errorf("defaults already have an enabled source: %+v", ch)
		}
		c.Sources = nil
		if ch := apply(t, c, StepSources); len(ch) != 1 || ch[0] != (Change{Kind: "source", Name: "default"}) || len(c.Sources) != 1 {
			t.Errorf("no sources: %+v", ch)
		}
		if c.Sources[0].ID == "" || c.Sources[0].IngestToken == "" || !c.Sources[0].Enabled {
			t.Errorf("source = %+v", c.Sources[0])
		}
		if ch := apply(t, c, StepSources); len(ch) != 0 {
			t.Errorf("a second call: %+v", ch)
		}

		c.Sources[0].Enabled = false
		if ch := apply(t, c, StepSources); len(ch) != 1 || ch[0].Name != "default-2" {
			t.Errorf("a disabled source named default: %+v", ch)
		}
		c.Sources[1].Enabled = false
		if ch := apply(t, c, StepSources); len(ch) != 1 || ch[0].Name != "default-3" {
			t.Errorf("default and default-2 taken: %+v", ch)
		}

		pull := &Config{Sources: []Source{{Name: "api", Type: SourcePull, Enabled: true, Pull: &Pull{URL: "https://logs.example.com"}}}}
		if ch := apply(t, pull, StepSources); len(ch) != 0 || len(pull.Sources) != 1 {
			t.Errorf("an enabled pull source: %+v", ch)
		}
	})

	t.Run("classify", func(t *testing.T) {
		c := Defaults()
		Normalize(c)
		rules, _ := json.Marshal(c.Rules)
		if ch := apply(t, c, StepClassify); len(ch) != 0 {
			t.Errorf("defaults: %+v", ch)
		}
		c.Settings.Categories = slices.DeleteFunc(c.Settings.Categories, func(x Category) bool { return x.Name == "audit" || x.Name == "http" })
		c.Settings.Categories[0].Description = "attacks and abuse"
		c.Settings.Categories = append([]Category{{Name: "billing"}}, c.Settings.Categories...)
		before := names(c.Settings.Categories, catName)
		ch := apply(t, c, StepClassify)
		if len(ch) != 2 || ch[0] != (Change{Kind: "category", Name: "http"}) || ch[1] != (Change{Kind: "category", Name: "audit"}) {
			t.Errorf("changes = %+v", ch)
		}
		if got := names(c.Settings.Categories, catName); !slices.Equal(got, append(before, "http", "audit")) {
			t.Errorf("categories = %v", got)
		}
		if c.Settings.Categories[1] != (Category{Name: "security", Description: "attacks and abuse"}) {
			t.Errorf("a description was lost: %+v", c.Settings.Categories[1])
		}
		if ch := apply(t, c, StepClassify); len(ch) != 0 {
			t.Errorf("a second call: %+v", ch)
		}
		if after, _ := json.Marshal(c.Rules); string(after) != string(rules) {
			t.Error("the classify defaults changed rules")
		}

		full := Defaults()
		full.Settings.Categories = nil
		for i := 0; i < MaxCategories-1; i++ {
			full.Settings.Categories = append(full.Settings.Categories, Category{Name: "c" + strconv.Itoa(i)})
		}
		if ch := apply(t, full, StepClassify); len(ch) != 1 || len(full.Settings.Categories) != MaxCategories {
			t.Errorf("past MaxCategories: %+v, %d categories", ch, len(full.Settings.Categories))
		}
	})

	t.Run("notify", func(t *testing.T) {
		c := Defaults()
		if _, err := ApplyStepDefaults(c, StepNotify); !errors.Is(err, ErrNeedsChannel) {
			t.Errorf("no notifiers: %v", err)
		}
		c.Notifiers = []Notifier{
			{ID: "routed", Name: "Email ops", Type: "email", Enabled: true},
			{ID: "email", Name: "Email DBAs", Type: "email", Enabled: true},
			{ID: "pd", Name: "PagerDuty", Type: "pagerduty", Enabled: true},
			{ID: "off", Name: "Old Slack", Type: "slack"},
		}
		c.Routes = []Route{
			{Name: "ops", Enabled: true, NotifierIDs: []string{"routed"}},
			{Name: "unused", Enabled: false, NotifierIDs: []string{"email"}},
		}
		ch := apply(t, c, StepNotify)
		if len(ch) != 2 || ch[0].Name != "Email DBAs: error and above" || ch[1].Name != "PagerDuty: critical and above" {
			t.Fatalf("changes = %+v", ch)
		}
		if len(c.Routes) != 4 {
			t.Fatalf("routes = %+v", c.Routes)
		}
		email, pd := c.Routes[2], c.Routes[3]
		if email.MinSeverity != model.SeverityError || !slices.Equal(email.NotifierIDs, []string{"email"}) || email.ThrottleSeconds != 900 || !email.Enabled || email.ID == "" {
			t.Errorf("email route = %+v", email)
		}
		if pd.MinSeverity != model.SeverityCritical || !slices.Equal(pd.NotifierIDs, []string{"pd"}) || pd.ThrottleSeconds != 900 {
			t.Errorf("pagerduty route = %+v", pd)
		}
		if ch := apply(t, c, StepNotify); len(ch) != 0 {
			t.Errorf("a second call: %+v", ch)
		}
		c.Notifiers[0].Enabled, c.Notifiers[1].Enabled, c.Notifiers[2].Enabled = false, false, false
		if _, err := ApplyStepDefaults(c, StepNotify); !errors.Is(err, ErrNeedsChannel) {
			t.Errorf("only disabled notifiers: %v", err)
		}
	})

	t.Run("notify with alert groups", func(t *testing.T) {
		c := Defaults()
		c.Sources[0].ID = "src"
		c.Notifiers = []Notifier{
			{ID: "team", Name: "Team Slack", Type: "slack", Enabled: true},
			{ID: "idle", Name: "Idle team Slack", Type: "slack", Enabled: true},
			{ID: "off", Name: "Off team Slack", Type: "slack", Enabled: true},
		}
		c.AlertGroups = []AlertGroup{
			{ID: "g1", Name: "Payments", Enabled: true, NotifierIDs: []string{"team"}},
			{ID: "g2", Name: "No sources", Enabled: true, NotifierIDs: []string{"idle"}},
			{ID: "g3", Name: "Disabled", NotifierIDs: []string{"off"}},
		}
		c.Sources[0].AlertGroupIDs = []string{"g1", "g3"}
		// Only a channel an enabled group with sources alerts through is
		// left alone: the others get nothing yet.
		ch := apply(t, c, StepNotify)
		if len(ch) != 2 || ch[0].Name != "Idle team Slack: error and above" || ch[1].Name != "Off team Slack: error and above" {
			t.Fatalf("changes = %+v", ch)
		}
	})

	t.Run("access and channels", func(t *testing.T) {
		c := Defaults()
		Normalize(c)
		before, _ := json.Marshal(c)
		for _, step := range []string{StepAccess, StepChannels} {
			if ch := apply(t, c, step); len(ch) != 0 {
				t.Errorf("%s: %+v", step, ch)
			}
		}
		if after, _ := json.Marshal(c); string(after) != string(before) {
			t.Error("the config changed")
		}
		if _, err := ApplyStepDefaults(c, "telemetry"); err == nil {
			t.Error("an unknown step should fail")
		}
	})
}

func TestRecordStep(t *testing.T) {
	c := Defaults()
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	RecordStep(c, StepSources, OutcomeDone, now)
	RecordStep(c, StepClassify, OutcomeSkipped, now)
	RecordStep(c, StepSources, OutcomeDefaults, now.Add(time.Minute))
	o := c.Onboarding
	if o.Status != OnboardingPending || len(o.Steps) != 2 || o.Steps[StepSources] != (OnboardingStep{Outcome: OutcomeDefaults, At: now.Add(time.Minute)}) {
		t.Errorf("onboarding = %+v", o)
	}
}
