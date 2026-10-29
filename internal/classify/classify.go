// Package classify assigns a severity, category and tags to log events using
// built-in heuristics followed by the operator's ordered rules.
package classify

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// Classifier names recorded on events.
const (
	ByHeuristic = "heuristic"
	ByRules     = "rules"
)

// Engine is an immutable, compiled rule set. Build a new one when rules change.
type Engine struct {
	rules []compiledRule
}

type compiledRule struct {
	config.Rule
	conds []compiledCond
}

type compiledCond struct {
	config.Condition
	re    *regexp.Regexp
	pre   *prefilter // nil: always run re
	value string
}

// New compiles rules. Invalid rules (which config validation should already
// have rejected) are skipped.
func New(rules []config.Rule) *Engine {
	e := &Engine{}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := compiledRule{Rule: r}
		ok := true
		for _, c := range r.Conditions {
			cc := compiledCond{Condition: c, value: c.Value}
			if !c.CaseSensitive {
				cc.value = strings.ToLower(c.Value)
			}
			if c.Field == "pattern" && (c.Op == config.OpEquals || c.Op == config.OpNotEquals) {
				// A pattern saved by an older version (learned rules)
				// keeps matching: normalize it as messages are now.
				cc.value = Renormalize(c.Value)
			}
			if c.Op == config.OpRegex || c.Op == config.OpNotRegex {
				pat := c.Value
				if !c.CaseSensitive {
					pat = "(?i)" + pat
				}
				re, err := regexp.Compile(pat)
				if err != nil {
					ok = false
					break
				}
				cc.re = re
				cc.pre = newPrefilter(pat)
			}
			cr.conds = append(cr.conds, cc)
		}
		if ok {
			e.rules = append(e.rules, cr)
		}
	}
	return e
}

// Classify sets ev's severity, category, tags, matched rules and fingerprint.
// It returns false when a rule says the event should be dropped.
func (e *Engine) Classify(ev *model.Event) bool {
	var stated bool
	ev.Severity, stated = detectSeverity(ev.Level, ev.Message)
	ev.SeverityGuessed = !stated
	// keep_stated keeps a level the record states, not a transport's
	// default for a stream (journald's or GELF's 6 and 3, a syslog PRI of
	// info or err): worked out once, when a rule asks.
	own, ownKnown, statedSev := false, false, ev.Severity
	ownLevel := func() bool {
		if !ownKnown {
			own, ownKnown = stated && !streamDefault(ev.Level, ev.Message), true
		}
		return own
	}
	ev.Classifier = ByHeuristic
	ev.Category = ""
	ev.MatchedRules = nil
	ev.NoEngine = false

	ctx := evalCtx{ev: ev}
	for _, r := range e.rules {
		if len(r.SourceIDs) > 0 && !slices.Contains(r.SourceIDs, ev.SourceID) {
			continue
		}
		if !r.matches(&ctx) {
			continue
		}
		ev.MatchedRules = append(ev.MatchedRules, r.Name)
		ev.Classifier = ByRules
		if r.Drop {
			return false
		}
		if r.SkipEngine {
			ev.NoEngine = true
		}
		for _, t := range r.Tags {
			ev.AddTag(t)
		}
		if r.Severity != "" && !(r.KeepStated && ownLevel()) && !(r.KeepStatedBelowWarning && ownLevel() && statedSev.Rank() < model.SeverityWarning.Rank()) {
			ev.SeverityGuessed = false
			if r.SeverityMode == config.SeverityAtLeast {
				if !ev.Severity.AtLeast(r.Severity) {
					ev.Severity = r.Severity
				}
			} else {
				ev.Severity = r.Severity
			}
		}
		if ev.Category == "" && r.Category != "" {
			ev.Category = r.Category
		}
		if r.Stop {
			break
		}
	}
	if ev.Category == "" {
		ev.Category = model.Uncategorized
	}
	ev.Fingerprint = Fingerprint(ev.SourceID, ev.Message)
	return true
}

// evalCtx carries per-event state shared by all conditions.
type evalCtx struct {
	ev          *model.Event
	msgFold     string
	msgFoldDone bool
	pattern     string
	patternDone bool
}

// value returns field's value, computing the message's pattern only once
// per event: learned rules all test it.
func (c *evalCtx) value(field string) (string, bool) {
	if field != "pattern" {
		return FieldValue(c.ev, field)
	}
	if !c.patternDone {
		c.pattern, c.patternDone = Pattern(c.ev.Message), true
	}
	return c.pattern, true
}

// folded returns the case-folded form of a field value for prefilters,
// computing the message's only once per event.
func (c *evalCtx) folded(field, raw string) string {
	if field != "message" && field != "msg" {
		return foldASCII(raw)
	}
	if !c.msgFoldDone {
		c.msgFold, c.msgFoldDone = foldASCII(raw), true
	}
	return c.msgFold
}

func (r *compiledRule) matches(ctx *evalCtx) bool {
	for _, c := range r.conds {
		m := c.matches(ctx)
		if r.Match == config.MatchAny && m {
			return true
		}
		if r.Match != config.MatchAny && !m {
			return false
		}
	}
	return r.Match != config.MatchAny
}

func (c *compiledCond) matches(ctx *evalCtx) bool {
	raw, present := ctx.value(c.Field)
	switch c.Op {
	case config.OpExists:
		return present
	case config.OpMissing:
		return !present
	case config.OpRegex:
		return present && c.regexMatch(ctx, raw)
	case config.OpNotRegex:
		return !present || !c.regexMatch(ctx, raw)
	case config.OpGreater, config.OpLess:
		if !present {
			return false
		}
		a, err1 := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		b, err2 := strconv.ParseFloat(strings.TrimSpace(c.Value), 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if c.Op == config.OpGreater {
			return a > b
		}
		return a < b
	}
	v := raw
	if !c.CaseSensitive {
		v = strings.ToLower(raw)
	}
	switch c.Op {
	case config.OpContains:
		return present && strings.Contains(v, c.value)
	case config.OpNotContains:
		return !present || !strings.Contains(v, c.value)
	case config.OpEquals:
		return present && v == c.value
	case config.OpNotEquals:
		return !present || v != c.value
	case config.OpStartsWith:
		return present && strings.HasPrefix(v, c.value)
	case config.OpEndsWith:
		return present && strings.HasSuffix(v, c.value)
	}
	return false
}

func (c *compiledCond) regexMatch(ctx *evalCtx, raw string) bool {
	if c.pre != nil && !c.pre.mayMatch(raw, ctx.folded(c.Field, raw)) {
		return false
	}
	return c.re.MatchString(raw)
}

// FieldValue returns the string form of a named event field. "pattern" is
// the message's Pattern (lowercase, with numbers, ids and times replaced by
// placeholders). Names other than the built-ins resolve as dot paths into
// ev.Fields (an optional "fields." prefix is accepted).
func FieldValue(ev *model.Event, name string) (string, bool) {
	switch name {
	case "message", "msg":
		return ev.Message, true
	case "pattern":
		return Pattern(ev.Message), true
	case "level":
		return ev.Level, ev.Level != ""
	case "severity":
		return string(ev.Severity), ev.Severity != ""
	case "source":
		return ev.SourceName, true
	case "source_id":
		return ev.SourceID, true
	case "category":
		return ev.Category, ev.Category != ""
	case "tags":
		return strings.Join(ev.Tags, ","), len(ev.Tags) > 0
	}
	if ev.Fields == nil {
		return "", false
	}
	v, ok := model.Lookup(ev.Fields, strings.TrimPrefix(name, "fields."))
	if !ok {
		return "", false
	}
	return model.ToString(v), true
}

const levelWords = `trace|debug|verbose|info|information|notice|warn|warning|error|err|fatal|crit|critical|panic|emerg|alert|severe`

const months = `Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec`

// A levelMark finds a format's level column. Its capture is a level name
// (parsed by parseLevelToken) or one of the format's own marks.
type levelMark struct {
	re    *regexp.Regexp
	marks map[string]model.Severity
	lits  []string                   // when set, re only runs on text containing one of them
	ok    func(s string, i int) bool // when set, vets a capture at i
	own   bool                       // the capture must be the record's own (see recordScan)
}

func (lm levelMark) level(t string) (model.Severity, bool) {
	if s, ok := lm.marks[t]; ok {
		return s, true
	}
	return parseLevelToken(t)
}

const (
	sevDebug    = model.SeverityDebug
	sevInfo     = model.SeverityInfo
	sevNotice   = model.SeverityNotice
	sevWarning  = model.SeverityWarning
	sevError    = model.SeverityError
	sevCritical = model.SeverityCritical
)

var (
	// Terminal colour codes (winston, NestJS, zerolog, tracing and logrus
	// write them even when the output is not a terminal), including the
	// charset reset tput sgr0 writes ("\x1b(B").
	ansiCodes = regexp.MustCompile(`\x1b(?:\[[0-9;:?]*[A-Za-z]|[()][0AB])`)
	// A syslog PRI ("<134>1 2026-…", "<190>Oct 11 …", dmesg -r's "<4>[…",
	// sd-daemon's "<3>…"): its low three bits are the severity.
	priHeader = regexp.MustCompile(`^<(\d{1,3})>\S`)
	// Prefixes collectors and CI runners put before the record: a container
	// runtime's "<time> stdout F ", docker compose's "web-1  | " (or "web  | "
	// for a container_name), kubectl logs --prefix's "[pod/web-1/app] ", the
	// time GitHub Actions ("2026-10-11T10:00:00.1234567Z ") and Jenkins'
	// Timestamper ("[2026-10-11T10:00:00.123Z] ") put on each line, and
	// BuildKit's step and time ("#12 1.234 ").
	collectorPrefix = regexp.MustCompile(`^(?:\S+\s+std(?:out|err)\s+[FP]\s+|[a-z0-9][\w.-]*(?:[-_]\d+\s+|\s{2,})\| |\[pod/[^\]\s]+\] |\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{7}Z |\[\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z\] |#\d+ \d+\.\d+ )`)
	// A syslog header: "Oct 11 10:00:00[.123456] host prog[pid]: ", the
	// same with an ISO time or journalctl's short-full and short-unix
	// times ("Sun 2026-10-11 10:00:00 UTC", "1791712800.123456"), its
	// short-monotonic "[ 1234.567890] host prog[pid]: ", or RFC 5424's "1
	// <time> host app procid msgid [structured data] ".
	syslogPrefix = regexp.MustCompile(`^(?:(?:[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}(?:\.\d+)?|\d{4}-\d{2}-\d{2}T\S+|[A-Z][a-z]{2} \d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:\.\d+)? \S+|\d{10}\.\d{6})\s+\S+\s+[\w./-]+(?:\[\d+\])?:\s+|\[ *\d+\.\d{6}\] \S+ [\w./-]+\[\d+\]:\s+|1 \d{4}-\d\d-\d\dT\S+ \S+ \S+ \S+ \S+ (?:-|(?:\[[^\]]*\])+) )`)
	// A syslog time and host with no program tag after them (a network
	// device's "Oct 11 10:00:00 asa %ASA-4-106023: …").
	syslogHost = regexp.MustCompile(`^(?:[A-Z][a-z]{2}\s+\d{1,2}\s+\d\d:\d\d:\d\d(?:\.\d+)?|\d{4}-\d\d-\d\dT\S+)\s+\S+\s+`)
	// kubectl --timestamps' time.
	isoPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})\s+`)
	// Formats that put the level in a fixed place at the start of the
	// record, matched on the whole record rather than its first bytes.
	// Daemons' formats, also after a syslog header or kubectl's time:
	//   - klog and glog ("E1011 10:00:00.123456 …", glog 0.7's
	//     "E20261011 …", CockroachDB's "E261011 …");
	//   - a one-letter level column after an ISO time: MongoDB 4.2 and
	//     older ("… I CONTROL  [initandlisten] …"), etcd's capnslog
	//     ("… W | etcdserver: …"), Telegraf ("… W! [outputs.influxdb] …");
	//   - Redis ("1:M 11 Oct 2026 10:00:00.126 * …": "." debug, "-"
	//     verbose, "*" notice, "#" warning);
	//   - Android logcat ("10-11 10:00:00.123  1234  1234 W Tag: …");
	//   - Xorg ("[    12.400] (WW) …": (II), (WW), (EE), (!!); not (--),
	//     (**) and the like);
	//   - Samba ("[2026/10/11 10:00:00.123456,  0] …": the debug level,
	//     mapped as Samba maps it to syslog);
	//   - Ceph daemons ("<time> 7f1a2b3c4d50 -1 osd.3 …": -1 is derr);
	//   - GlusterFS ("[<time>] W [MSGID: …] …") and CUPS ("E [11/Oct/2026:
	//     10:00:00 +0000] …"), M, X and A being emergency and alert;
	//   - MikroTik RouterOS topics ("system,info,account user …");
	//   - Tornado's and Jupyter's letter column ("[I 261011 10:00:00
	//     web:2348] …", "[W 2026-10-11 10:00:00.123 ServerApp] …").
	klogPrefix   = regexp.MustCompile(`^([IWEF])(?:\d{4}|\d{6}|\d{8}) \d\d:\d\d:\d\d`)
	daemonLevels = []levelMark{
		{re: klogPrefix},
		{re: regexp.MustCompile(`^\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d\.\d+(?:Z|[+-]\d\d:?\d\d)? ([TDINWECF])\d? +(?:\| |(?:[A-Z_]+|-) +\[)`)},
		{re: regexp.MustCompile(`^\d{4}-\d\d-\d\dT\S+ ([DIWE])! `)},
		{re: regexp.MustCompile(`^(?:\d+:[XCSM]|\[\d+\]) \d{1,2} [A-Z][a-z]{2}(?: \d{4})? \d\d:\d\d:\d\d\.\d{3} ([.*#-]) `),
			marks: map[string]model.Severity{".": sevDebug, "-": sevDebug, "*": sevNotice, "#": sevWarning}},
		{re: regexp.MustCompile(`^(?:\d{4}-)?\d\d-\d\d \d\d:\d\d:\d\d\.\d{3}\s+\d+\s+\d+ ([VDIWEFA]) `),
			marks: map[string]model.Severity{"V": sevDebug, "A": sevCritical}},
		{re: regexp.MustCompile(`^(?:\[ *\d+\.\d+\] )?\((II|WW|EE|!!)\) `),
			marks: map[string]model.Severity{"II": sevInfo, "WW": sevWarning, "EE": sevError, "!!": sevNotice}},
		{re: regexp.MustCompile(`^\[\d{4}/\d\d/\d\d \d\d:\d\d:\d\d(?:\.\d+)?, +(\d)[,\]]`),
			marks: map[string]model.Severity{"0": sevError, "1": sevWarning, "2": sevNotice, "3": sevNotice, "4": sevNotice, "5": sevNotice, "6": sevInfo, "7": sevInfo, "8": sevInfo, "9": sevInfo}},
		{re: regexp.MustCompile(`^\d{4}-\d\d-\d\dT\S+ [0-9a-f]{8,16} +(-1) `),
			marks: map[string]model.Severity{"-1": sevError}},
		{re: regexp.MustCompile(`^\[\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+(?: [+-]\d{4})?\] ([TDINWECAM]) \[`),
			marks: map[string]model.Severity{"A": sevCritical, "M": sevCritical}},
		{re: regexp.MustCompile(`^([XACEWNIDd]) \[\d\d/[A-Z][a-z]{2}/\d{4}:\d\d:\d\d:\d\d`),
			marks: map[string]model.Severity{"X": sevCritical, "A": sevCritical}},
		{re: regexp.MustCompile(`^(?:[A-Z][a-z]{2} +\d{1,2} \d\d:\d\d:\d\d \S+ |\d\d:\d\d:\d\d )?(?:(?:[a-z][\w-]*,)+(debug|info|warning|error|critical)(?:,[a-z][\w-]*)*|(debug|info|warning|error|critical)(?:,[a-z][\w-]*)+) `)},
		{re: regexp.MustCompile(`^\[([DIWEC]) (?:\d{6} \d\d:\d\d:\d\d [\w.]+:\d+|\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3} [\w.-]+)\] `)},
	}
	// And formats only seen as whole lines:
	//   - dmesg -x ("kern  :err   : […");
	//   - MongoDB 4.4+ JSON ({"t":{…},"s":"W", …});
	//   - java.util.logging's two-line SimpleFormatter and Tomcat's
	//     OneLineFormatter ("FINE", "CONFIG", "SEVERE", …);
	//   - Monolog (Laravel, Symfony: "[…] production.ERROR: …");
	//   - NestJS ("[Nest] 1  - 10/11/2026, 10:00:00 AM     LOG [Ctx] …");
	//   - context fields before the level (Sidekiq: "… pid=7 tid=… class=…
	//     jid=… INFO: done");
	//   - PostgreSQL csvlog's error_severity column (after the transaction
	//     ids, before the SQLSTATE);
	//   - kubectl get events' TYPE column (Normal, Warning);
	//   - Kotlin compiler diagnostics ("e: file:///…");
	//   - npm ("npm warn …", and its debug log "31 error code E404", which
	//     opens with "0 verbose …" and "1 info …"; so not a count such as
	//     clang's "1 error generated." or webpack's "1 error has …");
	//   - yarn v1 and similar CLIs ("error Command failed …", "info Visit
	//     …"): a lowercase level word followed by a capital or a quote;
	//   - Erlang/OTP's report header ("=ERROR REPORT==== 11-Oct-2026::…");
	//   - GitHub Actions workflow commands ("::error file=…::…");
	//   - webpack's infrastructure logger ("<w> [webpack-dev-server] …");
	//   - SLF4J's own messages ("SLF4J(W): No SLF4J providers were found.");
	//   - apt ("E: Unable to locate package …", "W: …", "N: …"; not
	//     udevadm info's "E: DEVNAME=/dev/sda" and "N: sda").
	lineLevels = []*regexp.Regexp{
		regexp.MustCompile(`^(?:kern|user|mail|daemon|auth|syslog|lpr|news|uucp|cron|authpriv|ftp|local[0-7]) *:(emerg|alert|crit|err|warn|notice|info|debug) *: `),
		regexp.MustCompile(`^\{"t":\{"\$date":"[^"]*"\},"s":"([DIWEF])\d?"`),
		regexp.MustCompile(`^(?:[A-Z][a-z]{2} \d{1,2}, \d{4} \d{1,2}:\d\d:\d\d [AP]M [^\n]*\n|\d{2}-[A-Z][a-z]{2}-\d{4} \d\d:\d\d:\d\d\.\d{3} )(SEVERE|WARNING|INFO|CONFIG|FINEST|FINER|FINE)[: ]`),
		regexp.MustCompile(`^\[[^\]]+\] [\w.-]+\.(DEBUG|INFO|NOTICE|WARNING|ERROR|CRITICAL|ALERT|EMERGENCY): `),
		regexp.MustCompile(`^\[\w+\] \d+\s+- [^\[a-z]*?\s(LOG|ERROR|WARN|DEBUG|VERBOSE|FATAL) `),
		regexp.MustCompile(`^\S+ pid=\d+ tid=\S+(?: [\w-]+=\S*)* (DEBUG|INFO|WARN|ERROR|FATAL): `),
		regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:\.\d+)? [^,\n]*,[^\n]*?,(?:\d+/\d+)?,\d+,(DEBUG[1-5]?|LOG|INFO|NOTICE|WARNING|ERROR|FATAL|PANIC),[0-9A-Z]{5},`),
		regexp.MustCompile(`^(?:[a-z0-9][a-z0-9.-]*\s{2,})?(?:\d+[smhd](?:\d+[smhd])?|<unknown>)\s{2,}(Normal|Warning)\s{2,}\S`),
		regexp.MustCompile(`^([ew]): (?:file:/|/|[A-Za-z]:\\)`),
		regexp.MustCompile(`^(?:npm (error|warn|notice|info|verbose|verb|silly) \S|(?:\d+ (warn|notice|info|verbose|verb|silly)|(?:[2-9]|\d\d+) (error)) \S+ \S)`),
		regexp.MustCompile(`^(error|warning|info) (?:[A-Z"]|\S+@\d)`),
		regexp.MustCompile(`^=(ERROR|WARNING|INFO) REPORT==== `),
		regexp.MustCompile(`^::(error|warning|notice|debug)(?: [^:\n]*)?::`),
		regexp.MustCompile(`^<([ewi])> `),
		regexp.MustCompile(`^SLF4J\(([DIWE])\): `),
		regexp.MustCompile(`^([EWN]): [A-Za-z][^\s=]*[\s:]`),
	}
	// A level stated in the text; the leftmost of these wins, as a record
	// states its own level before its message:
	//   - an uppercase level word ("ERROR", "WARN[0042]" from logrus,
	//     "INFO/MainProcess" from Celery, "[WRN]" from Serilog, "*ERROR*"
	//     from DRM, "WARNING!" from Squid, "|-INFO in" from logback's
	//     status printer, "cpu12:2097652)WARNING:" from ESXi's vmkernel,
	//     "DEBG" from supervisord; zerolog's and Serilog's TRC, DBG, INF,
	//     WRN, FTL, PNC and VRB only after a time or in brackets, not as
	//     Pi-hole's "FTL"; ALERT not after another uppercase word, as in
	//     Nagios' "SERVICE ALERT:"; VERBOSE and java.util.logging's FINE
	//     only as "VERBOSE[1234]", "VERBOSE:", "FINE [", "FINE:"), also
	//     "[Server thread/WARN]" (log4j's "[%t/%level]"), zap's "LEVEL(-2)"
	//     between tabs (logr's V(2)), loguru's "<time> | SUCCESS  | " (not a
	//     table's "| SUCCESS |") and NLog's "|Info|";
	//   - a bracketed one ("[info]", "<warn>", "[ info]", Apache's
	//     "[core:error]" and "[core:trace5]", ClickHouse's "<Information>");
	//   - one that opens the line after a syslog date or up to three
	//     timestamp-like tokens, possibly after a "[tag]" (Suricata's
	//     "[1234 - Suricata-Main]"), when a colon, tab or column spacing
	//     follows ("[api] 2026/10/11 10:00:00 warn:"), or in lowercase a
	//     "prog[pid] [" or a "Kind/name - " (ESXi's hostd, the flux CLI; not
	//     a syslog host named "alert" before "prog[pid]:", Go's "2026/10/11
	//     10:00:00 debug server[1]: …", or Printf's "Alert host.example -
	//     DOWN" and "trace 8.8.8.8 - 11 hops"); in title case after a time
	//     and a colon or column spacing (Falco's "10:00:00.123: Warning …",
	//     dbt's "10:00:09  Warning in …"), or before a source
	//     and an event id ("2026-10-11 10:00:00 Error Service Control
	//     Manager 7031 …"; not "Debug server listening"); with nothing or
	//     only a symbol before it, when a colon, tab, column spacing, " - "
	//     or the end follows ("error: …", "info   …", "debug1: …", "fatal
	//     error: …", rustc's "error[E0308]: …", "Note: …", Next.js's "error
	//     - …"; not "error rate 0.1%" or a table's "error   |"), and .NET's
	//     "fail: …" (not "FAIL\t…" from go test); after a symbol only with a
	//     colon (Terraform's "│ Error:", Buildkite's "🚨 Error:"; not ASCII
	//     symbols such as a table's "| error   |" or curl's "< Warning:");
	//   - the first column of a CSV export ("\"Warning\",…", "Error,10/…"),
	//     or the second of a quoted one ("\"Oct 11, 2026 @ …\",\"INFO\",…");
	//   - a label after a tag ("app[42]: warning: …", "main.go:42: error:",
	//     "[app] warn: …", "dovecot: master: Warning: …", "csproj : warning
	//     NU1603: …", tsc's "app.ts:3:7 - error TS2322: …" (not an Error's
	//     string after a count: "Attempt 3 - Error: …"), BIND's
	//     "resolver: debug 1: …", monit's "error    : …", Icinga 2's
	//     "critical/ApiListener: …") or between tabs (zap console); after a
	//     word that follows another one only in lowercase ("Build failed:
	//     error: …", not the error string in "Sync failed: Error: …");
	//   - PHP's "PHP Fatal error:" (or "Fatal error:" opening the line, as
	//     display_errors prints it), chrony's "Fatal error : ", MySQL's
	//     "[Note]" and "[System]", the kernel's "EXT4-fs warning", "BTRFS
	//     error (device …)" and "ACPI Error:", PostgreSQL's "LOG:  " and
	//     PgBouncer's "[1] LOG ";
	//   - the syslog severity in Cisco IOS, NX-OS and ASA mnemonics and in
	//     Junos' explicit-priority ones ("%LINK-3-UPDOWN:", "%ASA-4-106023:")
	//     opening the device's message (not one a bot quotes: "posted to
	//     #netops: sw01 %LINK-3-UPDOWN: …");
	//   - corosync's "notice  [TOTEM ] ";
	//   - glog's letter before a source position in TensorFlow's and
	//     PyTorch's logs (": W tensorflow/…/py_utils.cc:38] ", "[W
	//     socket.cpp:464] "), and a bracketed letter after a time or a
	//     caller (beego, frp, Gitea: "10:00:00.123 [I] ", "listen() [E] ").
	// Lowercase level words inside a sentence ("user info", "debug
	// symbols", "error rate") are not levels, nor are quoted values
	// ("set to \"WARN\""), words inside {…} (JSON, register dumps), echoed
	// commands, HTTP request targets ("GET /logs?level=error", TRACE), an
	// exception's message ("PSQLException: FATAL: …", quoting a server), a
	// level word as a key or a column ("CRITICAL: 0)", "error\t12", "│
	// CRITICAL │") or the value of a level key that is not the record's
	// own ("Log level: DEBUG").
	upperLevel   = regexp.MustCompile(`(?:^|[\s\[|:<*)]|\|-)(TRACE|DEBUG|INFO|NOTICE|WARN|WARNING|ERROR|ERR|FATAL|CRIT|CRITICAL|PANIC|DPANIC|EMERG|SEVERE|ERRO|FATA|PANI|DEBU|TRAC|DEBG|BLAT)(?:$|[\s\]):|"',>/\[!*|])|(?:\[|[\d>](?:[AP]M|Z)? )(TRC|DBG|INF|WRN|FTL|PNC|VRB)[\s\]]`)
	bracketLevel = regexp.MustCompile(`(?i)[\[(<]\s*(?:[\w-]*:)?(` + levelWords + `|note)[1-8]?\s*[\])>]`)
	openingLevel = regexp.MustCompile(`^\s*(?:\[[\w.-]+(?: - [\w.-]+)?\]\s+)?(?:(?i:` + months + `)\s+\d{1,2}\s+|\S*\d\S*(?: [AP]M)?\s+){1,3}(?:((?i:` + levelWords + `))(?::|\t|\s{2,})|(` + levelWords + `)(?: [\w.-]+\[\d+\] \[| [\w.-]*/\S* - ))`)
	titleTime    = `^\s*(?:\[[\w.-]+\]\s+)?(?:(?:` + months + `)\s+\d{1,2}\s+|\S*\d\S*\s+){0,2}\S*\d:\d\d[^\s|]*`
	titleWords   = `(Trace|Debug|Verbose|Info|Information|Informational|Notice|Warn|Warning|Error|Fatal|Critical|Alert|Emergency|Severe)`
	openingTitle = regexp.MustCompile(titleTime + `(?:(?::\s+|\s{2,})` + titleWords + `\s|\s` + titleWords + ` (?:[A-Z][\w-]* )+\d+ )`)
	openingBare  = regexp.MustCompile(`(?i)^(?:\s*(?:(fatal error|` + levelWords + `|note)\d?(?:\[[A-Z]\d{4}\])?(?::|\t|\s{2,}(?:[^\s|]|$)|$| - )|(fail|dbug|trce):)|(?:\p{So}|[^\x00-\x7F\P{Sm}])\x{FE0F}?\s+(fatal error|` + levelWords + `|note):)`)
	csvLevel     = regexp.MustCompile(`(?i)^(?:"?(` + levelWords + `)"?,"?\d|"[^"]*","(` + levelWords + `)",")`)
	labelLevel   = regexp.MustCompile(`(?i)(?:(?:[\]:]\s+|\t|\[\s|\b(?:Server|Client) VM )(fatal error|` + levelWords + `|(?-i:fail|dbug|trce))|\d - (?-i:(fatal error|` + levelWords + `)))(?:\d| \d{1,2})?(?-i: [A-Z]{1,5}\d{2,6})?(?::\s|\t| +:\s|/[A-Z]\w*:\s)`)
	phpLevel     = regexp.MustCompile(`(?:^|\bPHP )(Fatal error|Parse error|Warning|Notice|Deprecated|Recoverable fatal error):|[\]:]\s+(Fatal error) : `)
	kernelLevel  = regexp.MustCompile(`\b(?:[A-Z0-9]+-fs|BTRFS) (emergency|alert|critical|error|warning|notice|info|debug)(?: \(|:)|\bACPI(?: BIOS)? (Error|Warning)(?: \(bug\))?: `)
	pgLog        = regexp.MustCompile(`(?:^|[\s:])(LOG):\s\s|\[(System)\] \[MY-\d+\] |\[\d+\] (LOG) `)
	textLevels   = []levelMark{
		{re: openingLevel, ok: notInException}, {re: openingTitle, lits: titleLits}, {re: openingBare}, {re: csvLevel}, {re: bracketLevel}, {re: upperLevel, ok: notInException},
		{re: regexp.MustCompile(`(?:^|[\s\[|:<*)])(VERBOSE)[\[\]:]`), lits: []string{"VERBOSE"}},
		{re: regexp.MustCompile(`(?:^|[\s\[|])(FINEST|FINER|FINE)(?: \[|:)`), lits: []string{"FINE"}},
		{re: regexp.MustCompile(`(?:^\s*|[^A-Z\s]\s+|[\[|:<*)])(ALERT)(?:$|[\s\]):|"',>/\[!*|])`), lits: []string{"ALERT"}},
		{re: regexp.MustCompile(`/(TRACE|DEBUG|INFO|WARN|ERROR|FATAL)\]`)},
		{re: regexp.MustCompile(`\t(LEVEL\(-\d+\))\t`)},
		{re: regexp.MustCompile(`\d\d:\d\d:\d\d\.\d{3} \| (SUCCESS) +\| `), lits: []string{"| SUCCESS "}},
		{re: regexp.MustCompile(`\d\|(Trace|Debug|Info|Warn|Error|Fatal)\|`), lits: []string{"|"}},
		{re: labelLevel, ok: func(s string, i int) bool { return labelAfterTag(s, i) && notInException(s, i) }},
		{re: phpLevel, lits: []string{"error", "Warning", "Notice", "Deprecated"}},
		{re: kernelLevel, lits: []string{"-fs ", "BTRFS ", "ACPI "}},
		{re: pgLog, lits: []string{"LOG", "[System] "}},
		{re: regexp.MustCompile(`(?:^|[\s:*])%[A-Z][A-Z0-9_]*(?:-[A-Z0-9_]+)*-([0-7])-[A-Z0-9][A-Z0-9_]*\s?:\s`), lits: []string{"%"}, own: true,
			marks: map[string]model.Severity{"0": sevCritical, "1": sevCritical, "2": sevCritical, "3": sevError, "4": sevWarning, "5": sevNotice, "6": sevInfo, "7": sevDebug}},
		{re: regexp.MustCompile(`\b(emerg|alert|crit|error|warning|notice|info|debug|trace) +\[[A-Z0-9 ]{6}\] `), lits: []string{"  [", "warning ["}},
		{re: regexp.MustCompile(`(?:^\[|: )([IWEF]) [\w./-]+:\d+\] `), lits: []string{"] "}},
		{re: regexp.MustCompile(`(?::\d\d(?:[.,]\d+)?|\)) \[([DTINWECF])\] `), lits: []string{"[D] ", "[T] ", "[I] ", "[N] ", "[W] ", "[E] ", "[C] ", "[F] "}},
	}
	titleLits = []string{"Trace", "Debug", "Verbose", "Info", "Notice", "Warn", "Error", "Fatal", "Critical", "Alert", "Emergency", "Severe"}
	// The kernel's hung-task and RCU-stall reports say "INFO:" but are
	// logged at KERN_ERR.
	kernelStall = regexp.MustCompile(`\bINFO: (?:task \S+ blocked for more than \d+ seconds|\S+ (?:self-)?detected stalls? on CPU)`)
	// level=…, "level":"…", lvl=…, severity: …, python-json-logger's
	// "levelname", Monolog's "level_name", Serilog's "@l", Heroku's at=…,
	// and pino's, bunyan's and Monolog's numbers ("level":50); not a URL
	// query parameter ("?level=error&…"), a command-line flag
	// ("--log-level=ERROR", "--log.level=debug"), SQL ("level = 'ERROR'"),
	// a Python repr ("Namespace(…, loglevel='debug')", "{'level': 'x'}") or
	// a setting ("log_level": a config dump's, not the record's). Only a key
	// of the record itself counts (see recordScan).
	kvLits  = []string{"evel", "EVEL", "vl", "VL", "everity", "EVERITY", "at=", "AT=", "@l", "@L"}
	kvLevel = regexp.MustCompile(`(?i)(?:^|[\s{,;"'(\[.@|])(?:(?:level|lvl|severity|loglevel|levelname|level_name|@l)["']?(?:=|\s*:\s*)"?([a-z]+)\b|at=([a-z]+)\b|level":(\d+)[,}])`)
	// Data that is not the record's own words: HTTP request targets
	// ("\"GET /blog/postmortem-of-the-outage HTTP/1.1\"", gin's and Rails'
	// GET "/x", morgan's and IIS's unquoted ones with IIS's query; in them
	// "?level=error" is a parameter and TRACE a method), commands echoed
	// by sudo, cron, auditd's EXECVE, a shell's xtrace ("+ grep ERROR") or
	// prompt ("$ grep ERROR", PowerShell's "PS C:\> …"), GitHub Actions
	// ("##[group]Run grep ERROR …") and Junos ("command 'show log | match
	// error'"), Ansible's task headers ("TASK [Set level to WARNING] ***")
	// and Snort's and Suricata's rule messages ("[**] [1:2016141:4] ET INFO
	// … [**]").
	echoed = regexp.MustCompile(`"[A-Z][A-Z-]{2,15} \S+ HTTP/[\d.]+"|\b(?:GET|HEAD|POST|PUT|DELETE|PATCH|OPTIONS|CONNECT|TRACE|PROPFIND|PURGE|BAN)\s+(?:"(?:/|[a-z]+://)[^"]*"|(?:/|[a-z]+://)\S*(?: [^\s="]+=\S*)?(?: (?:\d{3}\b|-(?: |$)|HTTP/)|$))|(?:\bCOMMAND=|\bCMD \(|\bargc=\d+ |^\++ |^\$ |^PS [^>\n]*> |##\[group\]Run )[^\n]*|\bcommand '[^'\n]*'|^(?:TASK|PLAY|RUNNING HANDLER) \[[^\n]*\] \*{3,}|\[\*\*\] \[\d+:\d+:\d+\] [^\n]*?\[\*\*\]`)
	braced = regexp.MustCompile(`\{[^{}]*\}`)
	// An indented "key: <level>" line is a setting in a config dump or a
	// banner (helm template's "    severity: critical", Loki's
	// -print-config-stderr "  log_level: warn", Nomad's "  Log Level:
	// DEBUG", jq's "  \"level\": \"debug\",", Kafka's "\tlog.level = DEBUG"),
	// not a record's level, nor is a lone "key: <lowercase level>" (YAML's
	// "logging.level: debug", npm audit's "Severity: critical"; a Windows
	// event's "Level: Error" is).
	settingLine = regexp.MustCompile(`(?i)^(?:[ \t]+["']?[a-z][\w.-]*(?: [a-z][\w.-]*)?["']?(?:[ \t]*:[ \t]+|[ \t]*=[ \t]*)["']?(?:` + levelWords + `)["']?,?|[a-z][\w.-]*: (?-i:` + levelWords + `))[ \t]*$`)
	// What follows a level word that is a key or a column: only a number or
	// a boolean ("CRITICAL: 0)" in a tally, "error\t12" in a query's rows,
	// "debug: false" in a config dump).
	valueOnly = regexp.MustCompile(`(?i)^["']?\s*[:=\t,]\s*["']?(?:\d[\d.,:/-]*|true|false|yes|no|on|off)["']?[\s,;)]*$`)
	// Text that misleads keyword guessing (besides the above): names
	// (URLs, paths, mail addresses, systemd unit descriptions), zero counts
	// ("failed=0", "WARN=0 ERROR=0"), negations ("no errors", "0 Error(s)",
	// "0 failed", "without error", "Non-Fatal"), level settings ("set to
	// \"WARN\"", "--log-level=ERROR", "RUST_LOG: warn") and an empty error
	// field ("error":""). Each is tried only when its text has lit.
	guessNoise = []struct {
		lit string
		re  *regexp.Regexp
	}{
		{"://", regexp.MustCompile(`\b[a-zA-Z][\w+.-]*://[^\s"'<>]*`)},
		{"/", regexp.MustCompile(`(?:^|[\s=(<'"\[,])[~.]?/[^\s"'<>()\[\]{},;]*`)},
		{"@", regexp.MustCompile(`[\w.+-]+@[\w-]+(?:\.[\w-]+)+`)},
		{"'", regexp.MustCompile(`'[^\s'/]*/[^\s']*'`)},
		{" - ", regexp.MustCompile(`\.(?:service|socket|timer|target|mount|automount|path|scope|slice|swap|device) - [^\n]*`)},
		{"systemd[", regexp.MustCompile(`\bsystemd\[\d+\]: (?:Started|Starting|Stopping|Stopped|Finished|Reached target|Listening on) [^\n]*`)}, // systemd 245 and older
		{"=0", regexp.MustCompile(`(?i)\b(?:fail(?:ed|ures?)|errors?|warn(?:ings?)?|unreachable)=0\b`)},
		{"", regexp.MustCompile(`(?i)\b(?:no|zero|0|without(?: any)?) +(?:(?:error|failure|warning)s?(?:\(s\))?|failed)(?: +(?:occurred|found|detected|reported))?`)},
		{"-", regexp.MustCompile(`(?i)\bnon-(?:fatal|critical)\b`)},
		{"", regexp.MustCompile(`(?i)\bto\s+["'](?:` + levelWords + `)["']`)},
		{"", regexp.MustCompile(`(?i)(?:level|lvl|severity|\b[a-z]\w*_log)["']?\s*[=:]\s*["']?[a-z]+`)},
		{`"err`, regexp.MustCompile(`"(?:error|err)"\s*:\s*(?:""|null)`)},
	}
	kwCritical = regexp.MustCompile(`(?i)\b(fatal|critical|emergency|panic|outage|data loss|breach)\b`)
	// A warning class ("DeprecationWarning: …", Node's
	// "MaxListenersExceededWarning: …") outranks error words in its text.
	kwWarningClass = regexp.MustCompile(`\b[A-Z]\w*Warning: `)
	kwError        = regexp.MustCompile(`(?i)\b(error|errors? occurred|exception|failed|failure|failing|severe|refused|unable to|cannot|could not|timed out)\b`)
	kwWarning      = regexp.MustCompile(`(?i)\b(warn|warning|deprecated|retrying)\b`)
)

// DetectSeverity infers a severity from an explicit level value when there
// is one, otherwise from the message text.
func DetectSeverity(level, message string) model.Severity {
	s, _ := detectSeverity(level, message)
	return s
}

// detectSeverity also reports whether the record stated its level: a level
// field, or a level token in the text (syslog PRI, klog prefix, level=…,
// "ERROR …", "[crit]", "app[42]: warning:"). Otherwise the severity is a
// guess from keywords.
func detectSeverity(level, message string) (model.Severity, bool) {
	if level != "" {
		if s, ok := parseLevelToken(level); ok {
			return s, true
		}
		if n, err := strconv.Atoi(strings.TrimSpace(level)); err == nil {
			// journald gives a service's stdout and stderr PRIORITY 6
			// unless a line says otherwise, and Docker's journald and GELF
			// drivers give its stderr 3, so a level the text states
			// (klog's "E1011", level=error) is better.
			if n == 3 || n == 6 {
				if s, ok := textSeverity(message, true); ok {
					return s, true
				}
			}
			return numericSeverity(n), true
		}
	}
	return textSeverity(message, false)
}

// streamDefault reports whether the level detectSeverity reads for level
// and message is only a transport's default for a stream: journald's or
// GELF's 6 or 3, or a syslog PRI of info or err, with no level in the text.
func streamDefault(level, message string) bool {
	if n, err := strconv.Atoi(strings.TrimSpace(level)); err == nil {
		if n != 3 && n != 6 {
			return false
		}
		_, ok := textSeverity(message, true)
		return !ok
	}
	if level != "" {
		return false
	}
	if strings.IndexByte(message, 0x1b) >= 0 {
		message = ansiCodes.ReplaceAllString(message, "")
	}
	body := collectorPrefix.ReplaceAllString(message, "")
	if m := priHeader.FindStringSubmatch(body); m != nil {
		if n, _ := strconv.Atoi(m[1]); n <= 191 && (n%8 == 3 || n%8 == 6) {
			_, ok := textSeverity(body[strings.IndexByte(body, '>')+1:], true)
			return !ok
		}
	}
	return false
}

// textSeverity detects the severity a message states, or guesses one. An
// inner call, for text whose transport gave only a stream's default level
// (journald's 6, Docker's 3, a syslog PRI of 27 or 30), returns a level the
// text states the way a plain record would, and does not guess.
func textSeverity(message string, inner bool) (model.Severity, bool) {
	if strings.IndexByte(message, 0x1b) >= 0 {
		message = ansiCodes.ReplaceAllString(message, "")
	}
	body := collectorPrefix.ReplaceAllString(message, "")
	if m := priHeader.FindStringSubmatch(body); m != nil {
		if n, _ := strconv.Atoi(m[1]); n <= 191 {
			// Docker's syslog driver sends a container's stdout at info
			// and its stderr at err, whatever the line says.
			if (n%8 == 3 || n%8 == 6) && !inner {
				if s, ok := textSeverity(body[strings.IndexByte(body, '>')+1:], true); ok {
					return s, true
				}
			}
			return numericSeverity(n % 8), true
		}
	}
	plen := 0 // length of a syslog header (a long first token is not one)
	if loc := syslogPrefix.FindStringIndex(body); loc != nil && loc[1] <= 160 {
		plen = loc[1]
	}
	for _, re := range lineLevels {
		if m := re.FindStringSubmatchIndex(body); m != nil {
			if i, j := firstGroup(m); i >= 0 {
				if s, ok := parseLevelToken(body[i:j]); ok {
					return s, true
				}
			}
		}
	}
	starts := []string{body}
	if plen > 0 {
		starts = append(starts, body[plen:])
	}
	if loc := isoPrefix.FindStringIndex(body); loc != nil {
		starts = append(starts, body[loc[1]:])
	}
	for _, b := range starts {
		for _, lm := range daemonLevels {
			if m := lm.re.FindStringSubmatchIndex(b); m != nil {
				if i, j := firstGroup(m); i >= 0 {
					if s, ok := lm.level(b[i:j]); ok {
						return s, true
					}
				}
			}
		}
	}
	// Offsets below are into body: masking keeps lengths.
	own := body
	if mayEcho(body) {
		own = echoed.ReplaceAllStringFunc(body, blank)
	}
	scan := own
	for i := 0; i < 4 && strings.Contains(scan, "}"); i++ {
		scan = braced.ReplaceAllStringFunc(scan, blank)
	}
	// The first bytes, cut between words: "$" in the patterns must not
	// match inside "INFORMATION_SCHEMA" or "ALERTS".
	cut := min(len(scan), plen+96)
	for cut > plen && cut < len(scan) && wordByte(scan[cut-1]) && wordByte(scan[cut]) {
		cut--
	}
	head := scan[:cut]
	// A lone word is a value (psql -tAc's "warning", redis-cli's, ls'), not
	// a record, nor is one after a "name | " a collector strips (psql \x's
	// "level      | error"); kubectl's custom-columns print "Warning".
	w := strings.TrimSpace(body)
	lone := isWord(w) && (len(body) < len(message) || strings.ToLower(w) == w)
	setting := lone || strings.ContainsAny(body, ":=") && settingLine.MatchString(body)
	if setting {
		head = ""
	}
	// level=… states the level when the key is the record's own; otherwise
	// its value states nothing ("Log level: DEBUG"). A logfmt severity= in
	// a line without a msg= is an alert's or a finding's ("alert=DiskFull
	// severity=critical status=firing").
	var kvs [][]int
	var data []bool
	if !setting && containsAny(own, kvLits) {
		kvs = kvLevel.FindAllStringSubmatchIndex(own, -1)
		data = make([]bool, len(kvs))
		rs := newRecordScan(own, plen)
		msg := strings.Contains(own, "msg=") || strings.Contains(own, "message=")
		for k, m := range kvs {
			end := m[0] // the key's delimiter, if any
			if end < len(own) && (own[end] == ' ' || own[end] == '\t') {
				end++ // it ends the word before
			}
			i, j := firstGroup(m)
			// data, a command-line flag ("--log.level=debug") or a query parameter
			data[k] = !rs.own(end) || rs.start <= m[0] && own[rs.start] == '-' || j < len(own) && own[j] == '&' ||
				!msg && strings.HasSuffix(strings.ToLower(strings.TrimRight(own[m[0]:i], `"`)), "severity=")
		}
	}
	best, at := model.Severity(""), len(head)+1
	if strings.Contains(head, "INFO: ") {
		if loc := kernelStall.FindStringIndex(head); loc != nil {
			best, at = model.SeverityError, loc[0]
		}
	}
	for _, lm := range textLevels { // each captures a level name or mark
		if lm.lits != nil && !containsAny(head, lm.lits) {
			continue
		}
		m := lm.re.FindStringSubmatchIndex(head)
		if m == nil {
			continue
		}
		i, j := firstGroup(m)
		if i < 0 || i >= at || lm.ok != nil && !lm.ok(head, i) || !notValue(scan, i, j) || dataValue(kvs, data, i) {
			continue
		}
		if lm.own {
			from := plen
			if loc := syslogHost.FindStringIndex(head); from == 0 && loc != nil {
				from = loc[1]
			}
			if rs := newRecordScan(head, from); !rs.own(i) {
				continue
			}
		}
		if s, ok := lm.level(head[i:j]); ok {
			best, at = s, i
		}
	}
	// level=… states the level too, unless a level comes before it (the
	// record's own level comes before its message and data).
	for k, m := range kvs {
		i, j := firstGroup(m)
		if best != "" && i >= at {
			break
		}
		if data[k] {
			continue
		}
		s, ok := parseLevelToken(own[i:j])
		if m[6] >= 0 { // a number
			s, ok = numericLevel(own[i:j], own)
		}
		if ok {
			return s, true
		}
	}
	if best != "" {
		return best, true
	}
	if inner {
		return "", false
	}
	text := own
	for _, n := range guessNoise {
		if strings.Contains(text, n.lit) {
			text = n.re.ReplaceAllString(text, " ")
		}
	}
	switch {
	case kwCritical.MatchString(text):
		return model.SeverityCritical, false
	case kwWarningClass.MatchString(text):
		return model.SeverityWarning, false
	case kwError.MatchString(text), endsWithErrno(text):
		return model.SeverityError, false
	case kwWarning.MatchString(text):
		return model.SeverityWarning, false
	}
	return model.SeverityInfo, false
}

// recordScan follows a record from the end of any syslog header to tell
// whether a level key or token is the record's own: one at the top level of
// a JSON object (or in ECS's "log" object), or one after only times,
// numbers, tags and other key=value fields. A key in a JSON payload, in a
// struct or dict dump ("{Addr::8080 LogLevel:debug}"), or after free words
// ("Log level: debug", "received alert … severity=critical"), describes
// something else. Calls go forward.
type recordScan struct {
	s                   string
	i, start, depth     int  // the scan position, the current token's start, and {[ depth
	quoted, word, freed bool // in a "string"; the token has only letters; a free word was seen
}

func newRecordScan(s string, from int) recordScan {
	return recordScan{s: s, i: from, start: from, word: true}
}

// own reports whether a key starting at end is the record's own.
func (r *recordScan) own(end int) bool {
	for ; r.i < end; r.i++ {
		c := r.s[r.i]
		switch {
		case r.quoted:
			if c == '\\' {
				r.i++
			} else if c == '"' {
				r.quoted = false
			}
		case c == ' ' || c == '\t' || c == '\n':
			if w := strings.TrimRight(r.s[r.start:r.i], ",.;"); r.word && r.depth == 0 && len(w) > 1 && !timeWords[w] {
				r.freed = true
			}
			r.start, r.word = r.i+1, true
		case c == '"':
			r.quoted, r.word = true, false
		case c == '{' || c == '[':
			if !strings.HasSuffix(strings.TrimRight(r.s[:r.i], " "), `"log":`) { // ECS's "log":{"level":…}
				r.depth++
			}
			if c == '{' {
				if next := strings.TrimLeft(r.s[r.i+1:], " \t\n"); next != "" && next[0] != '"' && next[0] != '}' {
					r.freed = true // not JSON
				}
			}
			r.word = false
		case c == '}' || c == ']':
			r.depth--
			r.word = false
		case isLetter(c) || c == ',' || c == '.' || c == ';':
		default:
			r.word = false // a time, number, tag or key=value
		}
	}
	return !r.freed && r.depth <= 1
}

// timeWords are the words of a time.
var timeWords = map[string]bool{"Jan": true, "Feb": true, "Mar": true, "Apr": true, "May": true, "Jun": true, "Jul": true, "Aug": true, "Sep": true, "Oct": true, "Nov": true, "Dec": true,
	"Mon": true, "Tue": true, "Wed": true, "Thu": true, "Fri": true, "Sat": true, "Sun": true, "AM": true, "PM": true, "UTC": true, "GMT": true}

// labelAfterTag reports whether the label at i in s follows a tag rather
// than a plain word: "Error:" after "Sync failed: " is an error string, not
// after "dovecot: master: ", "imap-login: " opening a message, or "]: ".
// Lowercase labels ("clang: error:") always count.
func labelAfterTag(s string, i int) bool {
	if c := s[i]; 'a' <= c && c <= 'z' {
		return true
	}
	w := strings.TrimRight(s[:i], " \t")
	if !strings.HasSuffix(w, ":") || len(w) < 2 || !isLetter(w[len(w)-2]) {
		return true // "]: ", "main.go:42: ", "imap(…)<…>: "
	}
	w = w[:len(w)-1]
	k := strings.LastIndexAny(w, " \t")
	if k < 0 {
		return true // the word opens the text
	}
	before := strings.TrimRight(w[:k], " \t")
	if before == "" {
		return true
	}
	c := before[len(before)-1]
	return c == ':' || c == ']' || '0' <= c && c <= '9'
}

// notValue reports whether the level word at s[i:j] is not a key or a
// column: one followed only by a number or a boolean (see valueOnly), or a
// cell of a box-drawn table ("│ CRITICAL │" in trivy's).
func notValue(s string, i, j int) bool {
	if valueOnly.MatchString(s[j:]) {
		return false
	}
	return !strings.HasSuffix(strings.TrimRight(s[:i], " "), "│") || !strings.HasPrefix(strings.TrimLeft(s[j:], " "), "│")
}

// dataValue reports whether i is the value of a level key that is not the
// record's own (data[k] for kvs[k]).
func dataValue(kvs [][]int, data []bool, i int) bool {
	for k, m := range kvs {
		if a, _ := firstGroup(m); a == i {
			return data[k]
		}
	}
	return false
}

// isWord reports whether s is one word of ASCII letters.
func isWord(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isLetter(s[i]) {
			return false
		}
	}
	return s != ""
}

// endsWithErrno reports whether text ends with the error a failed call
// reports ("exec /app/server: permission denied", "…: No such file or
// directory"); not sshd's "Permission denied (publickey)".
func endsWithErrno(text string) bool {
	t := strings.TrimRight(text, " \t\r\n")
	for _, e := range []string{"permission denied", "no such file or directory"} {
		if n := len(t) - len(e); n >= 0 && strings.EqualFold(t[n:], e) && (n == 0 || !wordByte(t[n-1])) {
			return true
		}
	}
	return false
}

// notInException reports whether the level at i in s does not open an
// exception's message ("PSQLException: FATAL: …", "OperationalError: ERROR:
// …"), which quotes another program's severity, not the record's level.
func notInException(s string, i int) bool {
	w := strings.TrimRight(s[:i], " ")
	for _, class := range []string{"Exception:", "Error:"} {
		if n := len(w) - len(class); n > 0 && strings.HasSuffix(w, class) && wordByte(w[n-1]) {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }

func containsAll(s string, lits ...string) bool {
	for _, l := range lits {
		if !strings.Contains(s, l) {
			return false
		}
	}
	return true
}

func containsAny(s string, lits []string) bool {
	for _, l := range lits {
		if strings.Contains(s, l) {
			return true
		}
	}
	return false
}

// numericLevel maps a number in a JSON "level" field of record: pino's and
// bunyan's (10-60) and Monolog's (100-600). Other numbers are not levels,
// nor is a "level" in JSON that is not shaped as their records (pino's and
// bunyan's "time", "pid" and "hostname", or Monolog's "channel" and
// "datetime"): a game's or a sensor's level.
func numericLevel(t, record string) (model.Severity, bool) {
	if !containsAll(record, `"time":`, `"pid":`, `"hostname":`) && !containsAll(record, `"channel":`, `"datetime":`) {
		return "", false
	}
	switch n, _ := strconv.Atoi(t); n {
	case 10, 20, 30, 40, 50, 60, 100, 200, 250, 300, 400, 500, 550, 600:
		return numericSeverity(n), true
	}
	return "", false
}

// mayEcho reports whether s may hold a request target or an echoed
// command, sparing most lines the echoed regexp.
func mayEcho(s string) bool {
	if strings.Contains(s, "HTTP/") || strings.Contains(s, "COMMAND=") || strings.Contains(s, "CMD (") || strings.Contains(s, "argc=") ||
		strings.Contains(s, "##[group]Run ") || strings.Contains(s, "command '") || strings.Contains(s, "[**] ") || strings.Contains(s, "] ***") {
		return true
	}
	if s != "" && (s[0] == '+' || s[0] == '$' || strings.HasPrefix(s, "PS ")) {
		return true
	}
	if !strings.Contains(s, "/") {
		return false
	}
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "CONNECT", "TRACE", "PROPFIND", "PURGE", "BAN"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// wordByte reports whether c is part of a word: a letter, digit, '_' or a
// byte of a multi-byte UTF-8 rune.
func wordByte(c byte) bool {
	return c >= 0x80 || c == '_' || '0' <= c && c <= '9' || isLetter(c)
}

// blank replaces s with as many dashes, so masking keeps offsets.
func blank(s string) string { return strings.Repeat("-", len(s)) }

// firstGroup returns the bounds of the first capture group that matched.
func firstGroup(m []int) (int, int) {
	for g := 2; g+1 < len(m); g += 2 {
		if m[g] >= 0 {
			return m[g], m[g+1]
		}
	}
	return -1, -1
}

// parseLevelToken is model.ParseSeverity plus the abbreviations logrus,
// zerolog, Serilog, log15, .NET, supervisord and npm print (ERRO, TRC, VRB,
// eror, dbug, DEBG, verb, …), zap's DPANIC and its "LEVEL(-2)" for logr's
// V(2), PostgreSQL's and NestJS's LOG, MySQL's System, loguru's SUCCESS,
// Kubernetes events' Normal, and numbered or offset levels ("debug1",
// MongoDB's "D2", PostgreSQL's "DEBUG5", slog's "DEBUG-4").
func parseLevelToken(t string) (model.Severity, bool) {
	if t = strings.TrimSpace(t); len(t) > 32 { // no level is longer; bounds the recursion below
		return "", false
	}
	u := strings.ToUpper(t)
	switch u {
	case "ERRO", "EROR", "PARSE ERROR", "RECOVERABLE FATAL ERROR":
		return model.SeverityError, true
	case "FATAL ERROR", "FATA", "PANI", "PNC", "DPANIC":
		return model.SeverityCritical, true
	case "DEPRECATED":
		return model.SeverityWarning, true
	case "DEBU", "TRAC", "TRC", "VRB", "DBUG", "TRCE", "VERB", "SILLY", "DEBG", "BLAT":
		return model.SeverityDebug, true
	case "LOG", "SYSTEM", "NORMAL", "SUCCESS":
		return model.SeverityInfo, true
	}
	if strings.HasPrefix(u, "LEVEL(-") && strings.HasSuffix(u, ")") {
		return model.SeverityDebug, true
	}
	if s, ok := model.ParseSeverity(t); ok {
		return s, true
	}
	if base := strings.TrimRight(t, "0123456789"); base != t && base != "" {
		return parseLevelToken(strings.TrimRight(base, "+-"))
	}
	return "", false
}

// numericSeverity maps syslog priorities (0-7), pino and bunyan levels
// (10-60) and Monolog's (100-600) onto severities; below zero are zap's,
// zerolog's and slog's debug and trace levels.
func numericSeverity(n int) model.Severity {
	if n >= 100 {
		switch {
		case n >= 500:
			return model.SeverityCritical
		case n >= 400:
			return model.SeverityError
		case n >= 300:
			return model.SeverityWarning
		case n >= 250:
			return model.SeverityNotice
		case n >= 200:
			return model.SeverityInfo
		default:
			return model.SeverityDebug
		}
	}
	if n >= 10 {
		switch {
		case n >= 60:
			return model.SeverityCritical
		case n >= 50:
			return model.SeverityError
		case n >= 40:
			return model.SeverityWarning
		case n >= 30:
			return model.SeverityInfo
		default:
			return model.SeverityDebug
		}
	}
	switch {
	case n < 0:
		return model.SeverityDebug
	case n <= 2:
		return model.SeverityCritical
	case n == 3:
		return model.SeverityError
	case n == 4:
		return model.SeverityWarning
	case n == 5:
		return model.SeverityNotice
	case n == 6:
		return model.SeverityInfo
	default:
		return model.SeverityDebug
	}
}
