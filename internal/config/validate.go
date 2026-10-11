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
		st.Categories = []Category{}
	}
	cats := st.Categories[:0]
	for _, cat := range st.Categories {
		cat.Name = strings.ToLower(strings.TrimSpace(cat.Name))
		cat.Description = strings.TrimSpace(cat.Description)
		switch {
		case cat.Name == "" && cat.Description == "":
			continue // a blank row
		case cat.Name == "":
			return fmt.Errorf("settings: a category with a description needs a name")
		case cat.Name == model.Uncategorized:
			return fmt.Errorf("settings: %q is reserved for events no category fits", model.Uncategorized)
		}
		if i := slices.IndexFunc(cats, func(c Category) bool { return c.Name == cat.Name }); i >= 0 {
			if cat.Description != "" && cats[i].Description != "" && cat.Description != cats[i].Description {
				return fmt.Errorf("settings: category %q is listed twice with different descriptions", cat.Name)
			}
			if cats[i].Description == "" {
				cats[i].Description = cat.Description
			}
			continue // the same category again (names are case-insensitive)
		}
		if len(cat.Name) > 64 || len(cat.Description) > 300 {
			return fmt.Errorf("settings: category %q: names are limited to 64 characters and descriptions to 300", model.Truncate(cat.Name, 64))
		}
		cats = append(cats, cat)
	}
	st.Categories = cats
	if len(cats) > MaxCategories {
		return fmt.Errorf("settings: at most %d categories", MaxCategories)
	}
	return normalizeEngine(&st.Engine)
}

// MaxCategories bounds the category list: the engine answers a choice over
// all of them (plus "other") and accepts at most 100 options.
const MaxCategories = 50

var engineModelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// engineModelAliases maps laya-serve's aliases to the checkpoint names it
// reports in routing.model, so answers can be checked against the request.
var engineModelAliases = map[string]string{
	"en":                                     "english",
	"laya":                                   "english",
	"default":                                "english",
	"multi":                                  "multilingual",
	"ml":                                     "multilingual",
	"laya-multilingual":                      "multilingual",
	"convaiinnovations/laya-multilingual":    "multilingual",
	"typed":                                  "typed-decisions",
	"typed_decisions":                        "typed-decisions",
	"decisions":                              "typed-decisions",
	"laya-typed-decisions":                   "typed-decisions",
	"convaiinnovations/laya-typed-decisions": "typed-decisions",
}

// NormalizeEngineModel trims name, resolves laya-serve aliases and checks
// it is a plain checkpoint name ("" stays "").
func NormalizeEngineModel(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if a, ok := engineModelAliases[strings.ToLower(name)]; ok {
		return a, nil
	}
	if !engineModelName.MatchString(name) {
		return "", fmt.Errorf("invalid engine checkpoint %q: use english, multilingual, typed-decisions or a name from the engine's LAYA_EXTRA_MODELS", name)
	}
	return name, nil
}

func normalizeEngine(e *EngineSettings) error {
	e.URL = strings.TrimSpace(e.URL)
	if !HasRef(e.URL) {
		if _, err := ParseEngineURLs(e.URL); err != nil {
			return fmt.Errorf("settings: engine url: %w", err)
		}
	}
	m, err := NormalizeEngineModel(e.Model)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if m == "" {
		m = DefaultEngineModel
	}
	e.Model = m
	if e.MinSeverity == "" {
		e.MinSeverity = model.SeverityWarning
	}
	if !e.MinSeverity.Valid() {
		return fmt.Errorf("settings: invalid engine min_severity %q", e.MinSeverity)
	}
	if e.MinConfidence < 0 || e.MinConfidence > 1 {
		return fmt.Errorf("settings: engine min_confidence must be between 0 and 1")
	}
	if e.MaxInFlight <= 0 {
		e.MaxInFlight = 1
	}
	if e.MaxInFlight > 16 { // laya-serve answers 503 past 16
		return fmt.Errorf("settings: engine max_in_flight is at most 16 per instance")
	}
	if e.TimeoutSeconds <= 0 {
		e.TimeoutSeconds = 30
	}
	if e.TimeoutSeconds > 300 {
		return fmt.Errorf("settings: engine timeout_seconds is at most 300")
	}
	return nil
}

// ParseEngineURLs splits a comma-separated list of laya-serve base URLs
// (after ${env:...} expansion). An empty list is valid: the engine is then
// not configured.
func ParseEngineURLs(list string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%q is not an http(s) base URL", raw)
		}
		out = append(out, raw)
	}
	return out, nil
}

func normalizeSource(s *Source, now time.Time) error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	m, err := NormalizeEngineModel(s.EngineModel)
	if err != nil {
		return err
	}
	s.EngineModel = m
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
