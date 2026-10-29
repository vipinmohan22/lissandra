// Package config defines Lissandra's runtime configuration (sources, rules,
// notifiers, routes, settings) and persists it to a JSON file.
package config

import (
	"encoding/json"
	"time"

	"github.com/vipinmohan22/lissandra/internal/model"
)

// Source types.
const (
	SourcePush = "push" // Lissandra exposes /ingest/{id} and the sender POSTs logs to it.
	SourcePull = "pull" // Lissandra polls a remote HTTP API for logs.
)

// Source is a place logs come from.
type Source struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description,omitempty"`
	IngestToken string   `json:"ingest_token,omitempty"`
	Pull        *Pull    `json:"pull,omitempty"`
	Parse       Parse    `json:"parse"`
	Tags        []string `json:"tags,omitempty"`
	// EngineModel overrides the engine checkpoint for this source's
	// events (empty: Settings.Engine.Model).
	EngineModel string `json:"engine_model,omitempty"`
	// AlertGroupIDs are the alert groups notified about this source's
	// events, besides any alert routes that match them.
	AlertGroupIDs []string  `json:"alert_group_ids,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Pull describes how to fetch logs from a remote HTTP endpoint.
//
// URL and Body may contain the placeholders {since_rfc3339}, {since_unix},
// {since_unix_ms}, {now_rfc3339}, {now_unix} and {now_unix_ms}; "since" is
// the time of the previous successful poll.
type Pull struct {
	URL                string            `json:"url"`
	Method             string            `json:"method"`
	Headers            map[string]string `json:"headers,omitempty"`
	Body               string            `json:"body,omitempty"`
	Auth               Auth              `json:"auth"`
	IntervalSeconds    int               `json:"interval_seconds"`
	TimeoutSeconds     int               `json:"timeout_seconds"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify,omitempty"`
	// Dedupe drops records already seen (by content hash) in recent polls,
	// for APIs that return overlapping windows.
	Dedupe bool `json:"dedupe"`
}

// Auth types for pull sources.
const (
	AuthNone   = "none"
	AuthBearer = "bearer"
	AuthBasic  = "basic"
	AuthHeader = "header"
)

// Auth holds credentials for a pull source. Secret values may be written as
// ${env:NAME} to read them from the process environment.
type Auth struct {
	Type        string `json:"type"`
	Token       string `json:"token,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	HeaderName  string `json:"header_name,omitempty"`
	HeaderValue string `json:"header_value,omitempty"`
}

// Parse formats.
const (
	FormatAuto   = "auto"
	FormatJSON   = "json"
	FormatNDJSON = "ndjson"
	FormatText   = "text"
)

// Parse controls how raw payloads are turned into events. Empty field names
// fall back to auto-detection of common names (message/msg/log, level/severity,
// timestamp/@timestamp/time, ...).
type Parse struct {
	Format         string `json:"format"`
	RecordsPath    string `json:"records_path,omitempty"`
	MessageField   string `json:"message_field,omitempty"`
	LevelField     string `json:"level_field,omitempty"`
	TimestampField string `json:"timestamp_field,omitempty"`
}

// Rule match modes.
const (
	MatchAll = "all"
	MatchAny = "any"
)

// Condition operators.
const (
	OpContains    = "contains"
	OpNotContains = "not_contains"
	OpEquals      = "equals"
	OpNotEquals   = "not_equals"
	OpRegex       = "regex"
	OpNotRegex    = "not_regex"
	OpStartsWith  = "starts_with"
	OpEndsWith    = "ends_with"
	OpExists      = "exists"
	OpMissing     = "missing"
	OpGreater     = "gt"
	OpLess        = "lt"
)

// Operators lists every supported condition operator.
var Operators = []string{OpContains, OpNotContains, OpEquals, OpNotEquals, OpRegex, OpNotRegex,
	OpStartsWith, OpEndsWith, OpExists, OpMissing, OpGreater, OpLess}

// Severity modes for rule actions.
const (
	SeveritySet     = "set"      // overwrite the detected severity
	SeverityAtLeast = "at_least" // only raise it
)

// Rule review statuses.
const (
	RuleActive   = ""         // an ordinary rule
	RuleReview   = "review"   // learned from the engine: it applies, awaiting review
	RuleRejected = "rejected" // a learned rule the operator rejected
)

// Rule is a classification rule. Rules are evaluated in order; every
// matching rule contributes tags and severity changes, the first matching
// rule with a category decides the category, and Stop ends evaluation.
type Rule struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	Enabled      bool           `json:"enabled"`
	Match        string         `json:"match"`
	Conditions   []Condition    `json:"conditions"`
	SourceIDs    []string       `json:"source_ids,omitempty"`
	Category     string         `json:"category,omitempty"`
	Severity     model.Severity `json:"severity,omitempty"`
	SeverityMode string         `json:"severity_mode,omitempty"`
	// KeepStated applies Severity only to records that state no level of
	// their own (the detector guessed one): a record logged at WARN that
	// mentions an exception stays a warning.
	KeepStated bool     `json:"keep_stated,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Drop       bool     `json:"drop,omitempty"`
	Stop       bool     `json:"stop,omitempty"`
	// SkipEngine keeps matching events from the engine, so they keep
	// the rules' result. A rejected learned rule sets it, so the engine's
	// answer isn't applied (or learned) again.
	SkipEngine bool `json:"skip_engine,omitempty"`
	// Status is RuleReview for a rule learned from the engine until
	// it is approved (RuleActive) or rejected (RuleRejected).
	Status  string   `json:"status,omitempty"`
	Learned *Learned `json:"learned,omitempty"`
}

// Learned records the engine answer a learned rule was made from.
type Learned struct {
	At                 time.Time      `json:"at"`
	Model              string         `json:"model"`
	Category           string         `json:"category"`
	Confidence         float64        `json:"confidence"`
	Severity           model.Severity `json:"severity,omitempty"`
	SeverityConfidence float64        `json:"severity_confidence,omitempty"`
	SourceID           string         `json:"source_id"`
	EventID            string         `json:"event_id,omitempty"`
	Example            string         `json:"example"` // the message it was learned from, clipped
}

// Condition tests one field of an event. Field is one of message, level,
// severity, source, category, or a dot path into the event's structured
// fields (e.g. "http.status").
type Condition struct {
	Field         string `json:"field"`
	Op            string `json:"op"`
	Value         string `json:"value,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
}

// Notifier is a configured notification channel. Settings are interpreted
// by the notifier plugin registered for Type.
type Notifier struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Enabled  bool              `json:"enabled"`
	Settings map[string]string `json:"settings"`
}

// Route decides which classified events are sent to which notifiers.
// Empty filter lists match everything.
type Route struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Enabled         bool           `json:"enabled"`
	MinSeverity     model.Severity `json:"min_severity"`
	Categories      []string       `json:"categories,omitempty"`
	SourceIDs       []string       `json:"source_ids,omitempty"`
	Tags            []string       `json:"tags,omitempty"`
	MessageRegex    string         `json:"message_regex,omitempty"`
	NotifierIDs     []string       `json:"notifier_ids"`
	ThrottleSeconds int            `json:"throttle_seconds"`
}

// AlertGroup is a team, or a set of people, alerted about the events of
// the sources assigned to it (Source.AlertGroupIDs): through its channels
// (notifiers), and by email to its members through an email notifier's
// server.
type AlertGroup struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	// NotifierIDs are the group's channels: Slack, Teams, PagerDuty, …
	NotifierIDs []string `json:"notifier_ids"`
	// Emails are the group's members. They are emailed through
	// EmailNotifierID, an email notifier whose server settings are used
	// with these recipients instead of its own.
	Emails          []string       `json:"emails,omitempty"`
	EmailNotifierID string         `json:"email_notifier_id,omitempty"`
	MinSeverity     model.Severity `json:"min_severity"`
	Categories      []string       `json:"categories,omitempty"` // empty: any category
	ThrottleSeconds int            `json:"throttle_seconds"`
}

// Category is a label events are classified into. Rules set it directly;
// the engine chooses among categories by their descriptions.
type Category struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// UnmarshalJSON also accepts a plain name, as older configs stored them.
func (c *Category) UnmarshalJSON(b []byte) error {
	var name string
	if json.Unmarshal(b, &name) == nil {
		*c = Category{Name: name}
		return nil
	}
	// A fresh value: decoding into an existing element (a partial settings
	// update) must not keep fields the object leaves out.
	type plain Category
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = Category(p)
	return nil
}

// EngineSettings configures classification by the engine
// (laya-serve, github.com/NandhaKishorM/laya). Rules run first; the engine
// classifies what they leave, once per source and message pattern.
type EngineSettings struct {
	Enabled bool `json:"enabled"`
	// URL of laya-serve, e.g. http://127.0.0.1:8000. Several URLs may be
	// given, comma-separated; requests go round-robin and fail over.
	// ${env:NAME} and ${file:/path} references are expanded; a file is
	// re-read while Lissandra runs (Nomad renders engine addresses into one).
	URL    string `json:"url"`
	APIKey string `json:"api_key,omitempty"` // bearer key (LAYA_API_KEY on the engine)
	// Model is the checkpoint pinned on every request: english,
	// multilingual, typed-decisions, or a name from the engine's
	// LAYA_EXTRA_MODELS. Sources may override it.
	Model string `json:"model"`
	// OnlyUncategorized sends only events no rule categorized.
	OnlyUncategorized bool           `json:"only_uncategorized"`
	MinSeverity       model.Severity `json:"min_severity"`
	// MinConfidence is the answer confidence (0-1) an engine answer needs
	// before it is applied; below it the rules' result stands.
	MinConfidence float64 `json:"min_confidence"`
	// SetSeverity asks the engine for a severity when neither the log
	// record nor a rule set one.
	SetSeverity bool `json:"set_severity"`
	// MaxInFlight is concurrent requests per engine instance. laya-serve
	// computes one answer at a time, so more only queue inside it.
	MaxInFlight int `json:"max_in_flight"`
	// TimeoutSeconds bounds how long an event waits for the engine,
	// queueing included; then it goes on with the rules' result, so a slow
	// engine never holds alerts back longer than this.
	TimeoutSeconds int `json:"timeout_seconds"`
	// LearnRules turns an applied category answer for a live event into a
	// rule for its message pattern and source, marked for review, so the
	// pattern is classified by the rules from then on.
	LearnRules bool `json:"learn_rules"`
	// LearnMinConfidence is the confidence an answer needs to become a
	// rule; it is never below MinConfidence.
	LearnMinConfidence float64 `json:"learn_min_confidence"`
}

// Settings holds global options.
type Settings struct {
	// PublicURL is the externally reachable base URL of Lissandra, used for ingest
	// URLs shown in the UI and for links in notifications.
	PublicURL  string         `json:"public_url,omitempty"`
	Categories []Category     `json:"categories"`
	Engine     EngineSettings `json:"engine"`
}

// Setup wizard statuses.
const (
	OnboardingPending   = "pending"   // offered: the UI opens it, the dashboard shows a banner
	OnboardingDeferred  = "deferred"  // "Finish later": the banner only
	OnboardingCompleted = "completed" // finished
	OnboardingDismissed = "dismissed" // skipped as a whole
)

// Setup wizard steps, in the order the wizard shows them.
const (
	StepAccess   = "access"
	StepSources  = "sources"
	StepClassify = "classify"
	StepChannels = "channels"
	StepNotify   = "notify"
)

// OnboardingSteps lists the setup steps in order.
var OnboardingSteps = []string{StepAccess, StepSources, StepClassify, StepChannels, StepNotify}

// Outcomes of a setup step.
const (
	OutcomeDone     = "done"     // saved what the user chose
	OutcomeDefaults = "defaults" // applied Lissandra's defaults
	OutcomeSkipped  = "skipped"  // changed nothing; the step still counts as left to do
)

// Onboarding is the setup wizard's progress on this instance. nil (a config
// written before the wizard, or a seed that doesn't ask for it) means it is
// never offered on its own; Settings can still start it. Export leaves it
// out and import keeps the current one.
type Onboarding struct {
	Status     string                    `json:"status"`
	Steps      map[string]OnboardingStep `json:"steps,omitempty"`
	StartedAt  *time.Time                `json:"started_at,omitempty"`
	FinishedAt *time.Time                `json:"finished_at,omitempty"`
}

// OnboardingStep records how a setup step ended.
type OnboardingStep struct {
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
}

// Config is the complete persisted configuration.
type Config struct {
	Version     int          `json:"version"`
	Sources     []Source     `json:"sources"`
	Rules       []Rule       `json:"rules"`
	Notifiers   []Notifier   `json:"notifiers"`
	Routes      []Route      `json:"routes"`
	AlertGroups []AlertGroup `json:"alert_groups"`
	Settings    Settings     `json:"settings"`
	// Onboarding is top level, not in Settings, so a stale Settings page
	// saving its copy can't roll setup progress back.
	Onboarding *Onboarding `json:"onboarding,omitempty"`
}
