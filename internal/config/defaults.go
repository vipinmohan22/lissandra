package config

import "github.com/vipinmohan22/layalogs/internal/model"

// DefaultEngineModel is the Laya engine checkpoint used unless the operator
// picks another one.
const DefaultEngineModel = "english"

// DefaultCategories seeds the category list shown in the UI. The
// descriptions tell the Laya engine what belongs in each category.
var DefaultCategories = []Category{
	{"security", "attacks, intrusion attempts, malware, suspicious access, policy violations"},
	{"auth", "login failures, invalid or expired credentials and tokens, permission denied"},
	{"application", "unhandled exceptions, crashes, panics, null pointers, application bugs"},
	{"database", "database connections, query errors and timeouts, deadlocks, replication"},
	{"network", "DNS failures, connection refused or reset, TLS errors, upstream timeouts"},
	{"resource", "out of memory, disk full, CPU throttling, file descriptor or quota limits"},
	{"http", "HTTP request errors and unusual status codes from web servers and APIs"},
	{"performance", "slow requests, high latency, timeouts caused by load, queue backlogs"},
	{"deployment", "deploys, releases, rollbacks, configuration changes, restarts"},
	{"audit", "records of who did what: account, permission and configuration changes"},
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
				Name:         "Security: auth failures",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?i)(unauthori[sz]ed|forbidden|authentication (failed|failure)|failed password|invalid (user|password|token|credentials|api key)|permission denied|access denied|login failed|jwt (expired|invalid))`)},
				Category:     "security",
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
		Notifiers: []Notifier{},
		Routes:    []Route{},
		Settings: Settings{
			Categories: append([]Category{}, DefaultCategories...),
			Engine: EngineSettings{
				Enabled:           true, // idle until a URL is set (LAYA_ENGINE_URL in the Nomad job)
				URL:               "${env:LAYA_ENGINE_URL}",
				APIKey:            "${env:LAYA_ENGINE_API_KEY}",
				Model:             DefaultEngineModel,
				OnlyUncategorized: true,
				MinSeverity:       model.SeverityWarning,
				MinConfidence:     0.6,
				SetSeverity:       true,
				MaxInFlight:       4,
				TimeoutSeconds:    30,
			},
		},
	}
}
