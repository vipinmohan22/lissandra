package config

import (
	"encoding/json"
	"slices"

	"github.com/vipinmohan22/lissandra/internal/model"
)

// DefaultEngineModel is the engine checkpoint used unless the operator
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
				Name:         "PostgreSQL client rejected",
				Description:  "A client's bad password, missing pg_hba.conf entry or unknown role: PostgreSQL logs it as FATAL for that session, not for the server.",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?-i:FATAL):  (?:password authentication failed for user|no pg_hba\.conf entry for|role "[^"]*" does not exist)|,FATAL,28(?:P01|000),`)},
				Category:     "auth",
				Severity:     model.SeverityError,
				SeverityMode: SeveritySet,
				Tags:         []string{"auth"},
			},
			{
				Name:        "Login failures",
				Description: "A wrong password or unknown user: routine one at a time, so a warning even when the words read like an error. Early, so the rules after it can still raise what it lowers (a brute force, a service down).",
				Enabled:     true,
				Match:       MatchAll,
				Conditions: []Condition{
					cond("message", OpRegex, `(?i)(failed password|invalid user|authentication (failed|failure)|login (failed|denied)|invalid (password|credentials))`),
					cond("message", OpNotRegex, `\bfailed to connect\b|\bbrute[- ]?force\b|\battack|\bintrusion\b|\bmalicious\b|\bsql injection\b|\bxss\b|\bblocked by waf\b|\bpath traversal\b`), // the attack rules' words: they say security
				},
				Category:     "auth",
				Severity:     model.SeverityWarning,
				SeverityMode: SeveritySet,
				KeepStated:   true,
				Tags:         []string{"auth"},
			},
			{
				Name:         "Resource exhaustion",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `\bout of memory: kill(?:ed)? process\b|\boom[- ]?kill(?:ed|er|ing)?\b|\bno space left on device\b|\b(?:disk|file ?system|volume) (?:is )?full\b|\btoo many open files\b`)},
				Category:     "resource",
				Severity:     model.SeverityCritical,
				SeverityMode: SeverityAtLeast,
			},
			{
				Name:         "Resource limits",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `\bout ?of ?memory\b|\bOutOfMemoryError\b|\bresource[_ ]exhausted\b|\bquota exceeded\b|\bmemory limit\b|\bmemory size of \d+ bytes exhausted\b|\bcannot allocate memory\b`)},
				Category:     "resource",
				Severity:     model.SeverityError,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
			},
			{
				Name:    "Crash / panic",
				Enabled: true,
				Match:   MatchAll,
				Conditions: []Condition{
					cond("message", OpRegex, `(?:^|\s)panic: |(?:^|\n)fatal error: |\bsegmentation (?:fault|violation)\b|\bsigsegv\b|\bsegfault at [0-9a-f]+ ip [0-9a-f]+ sp [0-9a-f]+ error\b|\btraps: \S+\[\d+\] (?:general protection|trap (?:divide error|invalid opcode|stack segment))|\bkernel panic\b|\bcore dumped\b`),
					cond("message", OpNotRegex, `\bchild\b[^\n]{0,40}\b(?:died|exit signal)\b`),
				},
				Category:     "application",
				Severity:     model.SeverityCritical,
				SeverityMode: SeverityAtLeast,
				Tags:         []string{"crash"},
			},
			{
				Name:    "Service down",
				Enabled: true,
				Match:   MatchAll,
				Conditions: []Condition{
					cond("message", OpRegex, `\b(?:service|site|website|api|database|db|cluster|backend|upstream)s? (?:is |are |went |seems |appears )?(?:completely |currently |now )?(?:down|offline|unreachable)\b|\bhealth ?checks? (?:are |is )?failing\b|\b(?:all|every) (?:replicas|instances|backends|upstreams|endpoints|targets|pods|nodes|servers)(?: are| is)? (?:unhealthy|down|unavailable|failing|unreachable)\b|\bhas no server available\b`),
					cond("message", OpNotRegex, `\b(?:shut(?:ting)?|bring(?:ing)?|brought|tak(?:e|ing)|took|turn(?:ing|ed)?|spin(?:ning)?|scal(?:e|ing|ed)|wind(?:ing)?|slow(?:ing|ed)?|count(?:ing)?)\b[^\n]{0,30}\bdown\b|\bfor maintenance\b|\bscheduled (?:downtime|maintenance)\b|\[(?:resolved|recovered)\]|\((?:resolved|recovered)\)|\bresolved:|\b(?:has|have) recovered\b|\b(?:is|are|was|were|now|came|comes|been|it's) back (?:up|online)\b|\bmaintenance mode\b|\boffline (?:backup|mode|migration)\b|\b(?:down|offline)-`),
				},
				Category:               "application",
				Severity:               model.SeverityCritical,
				SeverityMode:           SeverityAtLeast,
				KeepStatedBelowWarning: true,
				Tags:                   []string{"outage"},
			},
			{
				Name:    "Security: attacks",
				Enabled: true,
				Match:   MatchAny,
				Conditions: []Condition{
					cond("message", OpRegex, `(sql injection|xss|csrf|brute[- ]?force|malicious|intrusion|blocked by waf|path traversal|"(?:GET|POST|HEAD|PUT|DELETE|PATCH|OPTIONS) [^"]*(?:\.\./|\.\.%2f|%2e%2e%2f|%2e%2e/|\.\.\\){2})`),
					cond("message", OpRegex, `"(?:GET|POST|HEAD|PUT|DELETE|PATCH|OPTIONS) [^"]*(?:union(?:%20|\+|\s|/\*\*/)+(?:all(?:%20|\+|\s|/\*\*/)+)?select\b|(?:%27|')(?:%20|\+|\s)*(?:or|and)(?:%20|\+|\s)+(?:%27|')?\d+(?:%27|')?(?:%20|\+|\s)*(?:%3d|=)|\b(?:sleep|benchmark|pg_sleep)(?:%20|\+)*(?:\(|%28)\d|waitfor(?:%20|\+|\s)+delay|information_schema|(?:%3b|;)(?:%20|\+|\s)*(?:drop|delete|insert|update)(?:%20|\+|\s)+)`),
					cond("message", OpRegex, `\b(?:sqlmap|nikto|wpscan|havij)/\d|\(nuclei\b|projectdiscovery/nuclei|\bacunetix\b`),
				},
				Category:     "security",
				Severity:     model.SeverityError,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
				Tags:         []string{"attack"},
			},
			{
				Name:         "Auth failures",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?i)(unauthori[sz]ed|forbidden|permission denied|access denied|invalid (token|api key)|jwt (expired|invalid))`)},
				Category:     "auth",
				Severity:     model.SeverityWarning,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
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
					cond("message", OpRegex, `"[A-Z]{3,10} [^"]* HTTP/[\d.]+" 5\d\d\b|\[GIN\] [^|]*\| 5\d\d \|`),
				},
				// Not keep_stated: access loggers write every request at
				// INFO (uvicorn, werkzeug), but a 5xx is a server error.
				Category:     "http",
				Severity:     model.SeverityError,
				SeverityMode: SeverityAtLeast,
			},
			{
				Name:       "Database",
				Enabled:    true,
				Match:      MatchAny,
				Conditions: []Condition{cond("message", OpRegex, `(?i)(deadlock|sqlstate|postgres|mysql|mariadb|mongo(db)?|redis|connection pool|duplicate key|constraint violation|ORA-\d{4,5}|too many connections|\bjdbc\b|\bdb (?:query|connection|pool|error)\b|:(?:5432|3306|27017|6379|1433|1521)\b|\bthe database system is\b)`)},
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
				Name:    "TLS certificate expired",
				Enabled: true,
				Match:   MatchAll,
				Conditions: []Condition{
					cond("message", OpRegex, `\bcert(?:ificate)?s?\b[^\n]{0,80}?\b(?:has|have) expired\b|\bcert(?:ificate)? (?:is |has )?expired\b|\bexpired (?:tls |ssl |x509 )?cert(?:ificate)?\b|\bCERT_HAS_EXPIRED\b`),
					cond("message", OpNotRegex, `\bcert(?:ificate)?s? (?:was |were |has been |have been |successfully )?renewed\b|\brenewed (?:the )?(?:tls |ssl )?cert|\brenewals? succeeded\b|\bold cert`), // replaced, not "could not be renewed"
				},
				Category:     "network",
				Severity:     model.SeverityError,
				SeverityMode: SeverityAtLeast,
				Tags:         []string{"tls"},
			},
			{
				Name:         "TLS certificate expiring",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `\bcert(?:ificate)?s?\b[^\n]{0,80}?\b(?:expires?|expiring|will expire)(?: in| within)? (?:(?:[0-9]|[12][0-9]|30) ?(?:days?|d)\b|\d+ ?(?:hours?|h|minutes?|m)\b|today\b|tomorrow\b|soon\b)`)},
				Category:     "network",
				Severity:     model.SeverityWarning,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
				Tags:         []string{"tls"},
			},
			{
				Name:    "Replication lag",
				Enabled: true,
				Match:   MatchAll,
				Conditions: []Condition{
					cond("message", OpRegex, `\breplica(?:tion)?\b[^\n]{0,40}?\blag(?:ging)?\b|\bseconds_behind_(?:master|source)\b|\breplication delay\b|\bslave (?:lag|delay)\b`),
					cond("message", OpNotRegex, `\b(?:lag|delay)\b[^\n]{0,15}?(?:^|[^\d.])(?:0(?:\.\d+)? ?(?:s|secs?|seconds?)|\d{1,3}(?:\.\d+)? ?ms)\b`), // none, or under a second
				},
				Category:     "database",
				Severity:     model.SeverityWarning,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
			},
			{
				Name:    "Queue backlog",
				Enabled: true,
				Match:   MatchAll,
				Conditions: []Condition{
					cond("message", OpRegex, `\bqueue\b[^\n]{0,60}?\b(?:depth|backlog|length|size)\b[^\n]{0,20}?\b\d{4,}\b|\b(?:0|no) (?:active )?consumers\b|\bconsumer lag\b|\bbacklog (?:of|is|at|grew to) \d{3,}\b`),
					// a queue being set up: its capacity, no consumers yet
					cond("message", OpNotRegex, `\b(?:creat(?:ed|ing)|declar(?:ed|ing)|initiali[sz](?:ed|ing)|configur(?:ed|ing))\b[^\n]{0,40}?\bqueue\b|\bstart(?:ed|ing)\b[^\n]{0,40}?\bqueue (?:size|capacity)\b|\bqueue (?:capacity|max(?:imum)? size)\b`),
				},
				Category:     "performance",
				Severity:     model.SeverityWarning,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
			},
			{
				Name:    "Circuit breaker open",
				Enabled: true,
				Match:   MatchAll,
				Conditions: []Condition{
					cond("message", OpRegex, `\bcircuit[- ]?breaker\b[^\n]{0,60}?\b(?:open(?:ed)?|tripped)\b|\bcircuit (?:is )?(?:open|opened|tripped)\b`),
					cond("message", OpNotRegex, `\bto (?:closed|half[_ -]?open)\b|\bhalf[- ]open\b`), // into open, not "from OPEN to CLOSED"
				},
				Category:     "network",
				Severity:     model.SeverityWarning,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
			},
			{
				Name:         "Disk usage high",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `\b(?:disk|filesystem|file system|volume|partition|inode)s? (?:usage|space|utili[sz]ation|use|used)\b[^\n%]{0,30}?\b(?:8[5-9]|9\d|100)(?:\.\d+)? ?%|\b(?:8[5-9]|9\d|100)(?:\.\d+)? ?% (?:full|used)\b|\blow (?:disk|free) space\b|\bdisk space (?:is )?(?:low|running out)\b|\bdisk watermark\b`)},
				Category:     "resource",
				Severity:     model.SeverityWarning,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
			},
			{
				Name:       "Slow operations",
				Enabled:    true,
				Match:      MatchAny,
				Conditions: []Condition{cond("message", OpRegex, `(?i)(slow (query|request|response)|took \d{4,}\s?ms|latency (high|exceeded)|deadline exceeded)`)},
				Category:   "performance",
			},
			{
				Name:         "PostgreSQL slow statements",
				Description:  "log_min_duration_statement lines of a second or more. PostgreSQL logs them as LOG.",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `\bLOG:\s+duration: \d{4,}(?:\.\d+)? ms\s+(?:statement|execute|parse|bind)\b`)},
				Category:     "performance",
				Severity:     model.SeverityWarning,
				SeverityMode: SeverityAtLeast,
			},
			{
				Name:         "Exceptions / stack traces",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `(?:^|[\s:(\[{"'=])[\w$.]*exception\b|traceback \(most recent call last\)|\bstack ?trace\b|\n\s*at [\w$.<>]+\([\w$]+\.(?:java|kt|scala|groovy):\d+\)`)},
				Category:     "application",
				Severity:     model.SeverityError,
				SeverityMode: SeverityAtLeast,
				KeepStated:   true,
			},
			{
				Name:         "Webpack critical dependency",
				Description:  "webpack's \"Critical dependency:\" is a build warning, not a critical error.",
				Enabled:      true,
				Match:        MatchAny,
				Conditions:   []Condition{cond("message", OpRegex, `^Critical dependency: `)},
				Severity:     model.SeverityWarning,
				SeverityMode: SeveritySet,
				KeepStated:   true,
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

// DefaultRuleChanges compares c's rules with this version's default rules,
// by name: missing are defaults c has no rule of that name for, changed
// those whose rule of that name differs in what it does (its ID and whether
// it is enabled aside), or sits out of this version's order (the first
// rule with a category sets it). A default an operator renamed is theirs.
func DefaultRuleChanges(c *Config) (missing, changed []string) {
	d := Defaults()
	Normalize(d)
	moved := outOfOrder(c, d)
	for _, def := range d.Rules {
		i := ruleIndex(c.Rules, def.Name)
		switch {
		case i < 0:
			missing = append(missing, def.Name)
		case !sameRule(c.Rules[i], def) || moved[def.Name]:
			changed = append(changed, def.Name)
		}
	}
	return missing, changed
}

// outOfOrder names the defaults c has out of d's order: those outside the
// longest run of them c has in that order (fewest to move).
func outOfOrder(c, d *Config) map[string]bool {
	var names []string
	var pos []int
	for _, def := range d.Rules {
		if i := ruleIndex(c.Rules, def.Name); i >= 0 {
			names, pos = append(names, def.Name), append(pos, i)
		}
	}
	best, prev, end := make([]int, len(pos)), make([]int, len(pos)), -1
	for i := range pos {
		best[i], prev[i] = 1, -1
		for j := 0; j < i; j++ {
			if pos[j] < pos[i] && best[j]+1 > best[i] {
				best[i], prev[i] = best[j]+1, j
			}
		}
		if end < 0 || best[i] > best[end] {
			end = i
		}
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	for i := end; i >= 0; i = prev[i] {
		delete(out, names[i])
	}
	return out
}

func ruleIndex(rules []Rule, name string) int {
	return slices.IndexFunc(rules, func(r Rule) bool { return r.Name == name })
}

// prevDefault is the index in c of the closest default before d.Rules[k]
// that c has, skipping those skip names, or -1.
func prevDefault(c, d *Config, k int, skip func(string) bool) int {
	for j := k - 1; j >= 0; j-- {
		if skip(d.Rules[j].Name) {
			continue
		}
		if i := ruleIndex(c.Rules, d.Rules[j].Name); i >= 0 {
			return i
		}
	}
	return -1
}

// RestoreDefaultRules gives c this version's default rules named in names
// (all when names is empty): a rule of that name gets the default's
// definition, keeping its ID and whether it is enabled, and moves after
// the closest default before it that c has when it came before that one;
// a missing one is inserted after that default, or first. It returns how
// many rules it changed, moved or added.
func RestoreDefaultRules(c *Config, names []string) int {
	d := Defaults()
	Normalize(d)
	moved := outOfOrder(c, d)
	// a default out of order that stays so is no place to put one after
	skip := func(name string) bool { return moved[name] && len(names) > 0 && !slices.Contains(names, name) }
	n := 0
	for k, def := range d.Rules {
		if len(names) > 0 && !slices.Contains(names, def.Name) {
			continue
		}
		if i := ruleIndex(c.Rules, def.Name); i >= 0 {
			if !sameRule(c.Rules[i], def) || moved[def.Name] {
				def.ID, def.Enabled = c.Rules[i].ID, c.Rules[i].Enabled
				c.Rules[i] = def
				n++
			}
			if moved[def.Name] {
				c.Rules = slices.Delete(c.Rules, i, i+1)
				c.Rules = slices.Insert(c.Rules, prevDefault(c, d, k, skip)+1, def)
			}
			continue
		}
		def.ID = ""
		c.Rules = slices.Insert(c.Rules, prevDefault(c, d, k, skip)+1, def)
		n++
	}
	return n
}

// sameRule reports whether a and b do the same, IDs and Enabled aside.
func sameRule(a, b Rule) bool {
	a.ID, b.ID, a.Enabled, b.Enabled = "", "", false, false
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
