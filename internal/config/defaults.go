package config

import "github.com/vipinmohan22/lissandra/internal/model"

// DefaultEngineModel is the Laya engine checkpoint used unless the operator
// picks another one.
const DefaultEngineModel = "english"

// DefaultLearnMinConfidence is the confidence an engine answer needs to
// become a learned rule unless the operator sets another.
const DefaultLearnMinConfidence = 0.8

// MaxReviewRules bounds the learned rules awaiting review: past it, Lissandra
// learns nothing new until some are approved or rejected.
const MaxReviewRules = 500

// DefaultCategories seeds the category list shown in the UI. They have no
// descriptions: the engine tells plain names apart better than descriptions
// it can only read about 16 tokens of. Add a description only to settle a
// name that is ambiguous.
var DefaultCategories = []Category{
	{Name: "security"}, {Name: "auth"}, {Name: "application"}, {Name: "database"}, {Name: "network"},
	{Name: "resource"}, {Name: "http"}, {Name: "performance"}, {Name: "deployment"}, {Name: "audit"},
}

func cond(field, op, value string) Condition {
	return Condition{Field: field, Op: op, Value: value}
}

// Defaults returns the configuration a fresh install starts with: one push
// source, a set of editable classification rules, and no notifiers.
func Defaults() *Config {
	return &Config{
		Sources: []Source{{
			Name:        "default",
			Type:        SourcePush,
			Enabled:     true,
			Description: "Generic HTTP ingest endpoint. POST JSON, NDJSON or plain-text lines.",
			Parse:       Parse{Format: FormatAuto},
		}},
		Rules: []Rule{
			{
				Name:        "Drop health checks",
				Description: "Ignore load balancer / orchestrator health probes. Enable to cut noise.",
				Enabled:     false,
				Match:       MatchAny,
				Conditions:  []Condition{cond("message", OpRegex, `(?i)\b(GET|HEAD) /(healthz?|readyz?|livez?|ping|status)\b`)},
				Drop:        true,
				Stop:        true,
			},
			{
				Name:         "Crash / panic",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?i)(panic:|fatal error:|segmentation fault|sigsegv|core dumped|unhandled exception|uncaught exception|process exited with code [1-9])`)},
				Category:     "application",
				Severity:     model.SeverityCritical,
				SeverityMode: SeverityAtLeast,
				Tags:         []string{"crash"},
			},
			{
				Name:         "Resource exhaustion",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?i)(out ?of ?memory|oom[- ]?kill|no space left on device|disk (is )?full|too many open files|resource exhausted|quota exceeded|memory limit)`)},
				Category:     "resource",
				Severity:     model.SeverityCritical,
				SeverityMode: SeverityAtLeast,
			},
			{
				Name:         "Security: attacks",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?i)(sql injection|xss|csrf|brute[- ]?force|malicious|intrusion|blocked by waf|path traversal|\.\./\.\./)`)},
				Category:     "security",
				Severity:     model.SeverityError,
				SeverityMode: SeverityAtLeast,
				Tags:         []string{"attack"},
			},
			{
				Name:         "Auth failures",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?i)(unauthori[sz]ed|forbidden|authentication (failed|failure)|failed password|invalid (user|password|token|credentials|api key)|permission denied|access denied|login failed|jwt (expired|invalid))`)},
				Category:     "auth",
				Severity:     model.SeverityWarning,
				SeverityMode: SeverityAtLeast,
				Tags:         []string{"auth"},
			},
			{
				Name:    "HTTP 5xx",
				Enabled: true,
				Match:   MatchAny,
				Conditions: []Condition{
					cond("status", OpRegex, `^5\d\d$`),
					cond("status_code", OpRegex, `^5\d\d$`),
					cond("http.status_code", OpRegex, `^5\d\d$`),
					cond("message", OpRegex, `"\s5\d\d\s`),
				},
				Category:     "http",
				Severity:     model.SeverityError,
				SeverityMode: SeverityAtLeast,
			},
			{
				Name:       "Database",
				Enabled:    true,
				Match:      MatchAny,
				Conditions: []Condition{cond("message", OpRegex, `(?i)(deadlock|sqlstate|postgres|mysql|mariadb|mongo(db)?|redis|connection pool|duplicate key|constraint violation|ORA-\d{4,5}|too many connections)`)},
				Category:   "database",
			},
			{
				Name:       "Network",
				Enabled:    true,
				Match:      MatchAny,
				Conditions: []Condition{cond("message", OpRegex, `(?i)(connection refused|connection reset|timed out|timeout|no route to host|unreachable|broken pipe|tls handshake|econnrefused|etimedout|dns|name resolution)`)},
				Category:   "network",
			},
			{
				Name:       "Slow operations",
				Enabled:    true,
				Match:      MatchAny,
				Conditions: []Condition{cond("message", OpRegex, `(?i)(slow (query|request|response)|took \d{4,}\s?ms|latency (high|exceeded)|deadline exceeded)`)},
				Category:   "performance",
			},
			{
				Name:         "Exceptions / stack traces",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?i)(exception|traceback \(most recent call last\)|stack ?trace|\bat [\w.$]+\([\w]+\.(java|kt|scala):\d+\))`)},
				Category:     "application",
				Severity:     model.SeverityError,
				SeverityMode: SeverityAtLeast,
			},
			{
				Name:       "Deployments",
				Enabled:    true,
				Match:      MatchAny,
				Conditions: []Condition{cond("message", OpRegex, `(?i)(deploy(ment|ed|ing)|rolling update|rollback|release v?\d+\.\d+)`)},
				Category:   "deployment",
			},
		},
		Notifiers:   []Notifier{},
		Routes:      []Route{},
		AlertGroups: []AlertGroup{},
		Settings: Settings{
			Categories: append([]Category{}, DefaultCategories...),
			Engine: EngineSettings{
				Enabled:            true, // idle until a URL is set (LISSANDRA_ENGINE_URL in the Nomad job)
				URL:                "${env:LISSANDRA_ENGINE_URL}",
				APIKey:             "${env:LISSANDRA_ENGINE_API_KEY}",
				Model:              DefaultEngineModel,
				OnlyUncategorized:  true,
				MinSeverity:        model.SeverityWarning,
				MinConfidence:      0.6,
				SetSeverity:        false, // doubles engine time; rarely changes a keyword guess
				MaxInFlight:        1,
				TimeoutSeconds:     30,
				LearnRules:         true,
				LearnMinConfidence: DefaultLearnMinConfidence,
			},
		},
	}
}
