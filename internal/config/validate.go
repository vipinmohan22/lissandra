package config

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

// Normalize fills defaults, assigns missing IDs and validates c in place.
func Normalize(c *Config) error {
	now := time.Now().UTC()
	seen := map[string]bool{}
	uniq := func(kind, id string) error {
		key := kind + "/" + id
		if seen[key] {
			return fmt.Errorf("duplicate %s id %q", kind, id)
		}
		seen[key] = true
		return nil
	}

	if c.Sources == nil {
		c.Sources = []Source{}
	}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.ID == "" {
			s.ID = model.NewID()
		}
		if err := uniq("source", s.ID); err != nil {
			return err
		}
		if err := normalizeSource(s, now); err != nil {
			return fmt.Errorf("source %q: %w", s.Name, err)
		}
		// Names must be unique because /ingest/{name} resolves by name.
		if err := uniq("source name", s.Name); err != nil {
			return fmt.Errorf("a source named %q already exists", s.Name)
		}
	}

	if c.Rules == nil {
		c.Rules = []Rule{}
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.ID == "" {
			r.ID = model.NewID()
		}
		if err := uniq("rule", r.ID); err != nil {
			return err
		}
		if err := normalizeRule(r); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
	}

	if c.Notifiers == nil {
		c.Notifiers = []Notifier{}
	}
	notifierIDs := map[string]bool{}
	for i := range c.Notifiers {
		n := &c.Notifiers[i]
		if n.ID == "" {
			n.ID = model.NewID()
		}
		if err := uniq("notifier", n.ID); err != nil {
			return err
		}
		n.Name = strings.TrimSpace(n.Name)
		if n.Name == "" {
			return fmt.Errorf("notifier name is required")
		}
		if n.Type == "" {
			return fmt.Errorf("notifier %q: type is required", n.Name)
		}
		if n.Settings == nil {
			n.Settings = map[string]string{}
		}
		notifierIDs[n.ID] = true
	}

	if c.Routes == nil {
		c.Routes = []Route{}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.ID == "" {
			r.ID = model.NewID()
		}
		if err := uniq("route", r.ID); err != nil {
			return err
		}
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			return fmt.Errorf("route name is required")
		}
		if r.MinSeverity == "" {
			r.MinSeverity = model.SeverityError
		}
		if !r.MinSeverity.Valid() {
			return fmt.Errorf("route %q: invalid min_severity %q", r.Name, r.MinSeverity)
		}
		if r.MessageRegex != "" {
			if _, err := regexp.Compile(r.MessageRegex); err != nil {
				return fmt.Errorf("route %q: message_regex: %w", r.Name, err)
			}
		}
		if r.ThrottleSeconds < 0 {
			r.ThrottleSeconds = 0
		}
		// Drop references to notifiers that no longer exist.
		kept := r.NotifierIDs[:0]
		for _, id := range r.NotifierIDs {
			if notifierIDs[id] {
				kept = append(kept, id)
			}
		}
		r.NotifierIDs = kept
		if r.NotifierIDs == nil {
			r.NotifierIDs = []string{}
		}
	}

	st := &c.Settings
	st.PublicURL = strings.TrimRight(strings.TrimSpace(st.PublicURL), "/")
	if st.PublicURL != "" {
		if u, err := url.Parse(st.PublicURL); err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("settings: public_url must be an absolute URL")
		}
	}
	if st.Categories == nil {
		st.Categories = []string{}
	}
	cats := st.Categories[:0]
	for _, cat := range st.Categories {
		cat = strings.ToLower(strings.TrimSpace(cat))
		if cat != "" && !slices.Contains(cats, cat) {
			cats = append(cats, cat)
		}
	}
	st.Categories = cats
	ai := &st.AI
	if ai.Model == "" {
		ai.Model = DefaultAIModel
	}
	if ai.Effort == "" {
		ai.Effort = "low"
	}
	if !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, ai.Effort) {
		return fmt.Errorf("settings: invalid AI effort %q", ai.Effort)
	}
	if ai.MinSeverity == "" {
		ai.MinSeverity = model.SeverityWarning
	}
	if !ai.MinSeverity.Valid() {
		return fmt.Errorf("settings: invalid AI min_severity %q", ai.MinSeverity)
	}
	if ai.MaxCallsPerHour <= 0 {
		ai.MaxCallsPerHour = 120
	}
	return nil
}

func normalizeSource(s *Source, now time.Time) error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}
	switch s.Parse.Format {
	case "":
		s.Parse.Format = FormatAuto
	case FormatAuto, FormatJSON, FormatNDJSON, FormatText:
	default:
		return fmt.Errorf("unknown parse format %q", s.Parse.Format)
	}
	switch s.Type {
	case SourcePush:
		s.Pull = nil
		if s.IngestToken == "" {
			s.IngestToken = model.NewToken()
		}
	case SourcePull:
		s.IngestToken = ""
		if s.Pull == nil {
			return fmt.Errorf("pull settings are required")
		}
		return normalizePull(s.Pull)
	default:
		return fmt.Errorf("type must be %q or %q", SourcePush, SourcePull)
	}
	return nil
}

func normalizePull(p *Pull) error {
	p.URL = strings.TrimSpace(p.URL)
	u, err := url.Parse(strings.NewReplacer(
		"{since_rfc3339}", "x", "{since_unix}", "0", "{since_unix_ms}", "0",
		"{now_rfc3339}", "x", "{now_unix}", "0", "{now_unix_ms}", "0").Replace(p.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("pull url must be an absolute http(s) URL")
	}
	p.Method = strings.ToUpper(strings.TrimSpace(p.Method))
	if p.Method == "" {
		p.Method = "GET"
	}
	if p.Method != "GET" && p.Method != "POST" {
		return fmt.Errorf("pull method must be GET or POST")
	}
	if p.IntervalSeconds == 0 {
		p.IntervalSeconds = 60
	}
	if p.IntervalSeconds < 5 {
		return fmt.Errorf("pull interval must be at least 5 seconds")
	}
	if p.TimeoutSeconds <= 0 {
		p.TimeoutSeconds = 15
	}
	switch p.Auth.Type {
	case "":
		p.Auth.Type = AuthNone
	case AuthNone, AuthBearer, AuthBasic:
	case AuthHeader:
		if strings.TrimSpace(p.Auth.HeaderName) == "" {
			return fmt.Errorf("header auth requires a header name")
		}
	default:
		return fmt.Errorf("unknown auth type %q", p.Auth.Type)
	}
	return nil
}

func normalizeRule(r *Rule) error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch r.Match {
	case "":
		r.Match = MatchAll
	case MatchAll, MatchAny:
	default:
		return fmt.Errorf("match must be %q or %q", MatchAll, MatchAny)
	}
	if len(r.Conditions) == 0 {
		return fmt.Errorf("at least one condition is required")
	}
	for i := range r.Conditions {
		cd := &r.Conditions[i]
		cd.Field = strings.TrimSpace(cd.Field)
		if cd.Field == "" {
			return fmt.Errorf("condition %d: field is required", i+1)
		}
		if !slices.Contains(Operators, cd.Op) {
			return fmt.Errorf("condition %d: unknown operator %q", i+1, cd.Op)
		}
		if cd.Op == OpRegex || cd.Op == OpNotRegex {
			if _, err := regexp.Compile(cd.Value); err != nil {
				return fmt.Errorf("condition %d: %w", i+1, err)
			}
		}
	}
	r.Category = strings.ToLower(strings.TrimSpace(r.Category))
	if r.Severity != "" && !r.Severity.Valid() {
		return fmt.Errorf("invalid severity %q", r.Severity)
	}
	switch r.SeverityMode {
	case "":
		r.SeverityMode = SeveritySet
	case SeveritySet, SeverityAtLeast:
	default:
		return fmt.Errorf("severity_mode must be %q or %q", SeveritySet, SeverityAtLeast)
	}
	return nil
}
