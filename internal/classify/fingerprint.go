package classify

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	reUUID  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reIP    = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	reTime  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[t ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:z|[+-]\d{2}:?\d{2})?`)
	reHex   = regexp.MustCompile(`\b(?:0x)?[0-9a-f]{8,}\b`)
	reNum   = regexp.MustCompile(`\d+(?:\.\d+)?`)
	reSpace = regexp.MustCompile(`\s+`)

	// Variable parts found after the steps above, so that they also apply
	// to a pattern an older version computed (digits already "<n>"):
	//   - a mail address (not "pkg@<n>.<n>" or "postgresql@<n>-main.service");
	reEmail = regexp.MustCompile(`(?:[a-z0-9._%+-]|<n>)+@(?:(?:[a-z0-9-]|<n>)+\.)+([a-z]{2,24})\b`)
	//   - the value of a key that names an account ("user=alice");
	reKVUser     = regexp.MustCompile(`\b(` + userKeys + `)=("[^"]*"|'[^']*'|[^\s,;&)\]}"']+)`)
	reKVUserJSON = regexp.MustCompile(`"(` + userKeys + `)"(\s*:\s*)"([^"]*)"`)
	//   - the value of a key that names an id, when it mixes letters and
	//     digits ("tid=2kqj", "\"request_id\":\"ab12cd\""; not "task_id=load");
	reKVID     = regexp.MustCompile(`\b(` + idKeys + `)=(["']?)((?:[a-z]|<n>)+)(["']?)`)
	reKVIDJSON = regexp.MustCompile(`"(` + idKeys + `)"(\s*:\s*)"((?:[a-z]|<n>)+)"`)
	//   - the account named in sshd's, PAM's, MySQL's and similar messages
	//     ("Failed password for invalid user ubuntu from", "for user 'root'").
	reUserQuoted = regexp.MustCompile(`\b(user ['"])([^'"\s]{1,64})(['"])`)
	reUser       = regexp.MustCompile(`\b((?:failed|accepted) (?:password|publickey|keyboard-interactive/pam|none|hostbased|gssapi-with-mic) for (?:invalid user )?|(?:invalid|illegal|unknown) user |authenticating user |for user )([^\s'":]+)`)
	//   - the generated end of a Kubernetes pod name: a Deployment's pods
	//     are "<deployment>-<pod-template-hash>-<suffix>", the hash 6-10 and
	//     the suffix 5 characters of "bcdfghjklmnpqrstvwxz2456789", which
	//     has no vowels, so no word is one ("orders-java-5b7c9d-ktfjq").
	//     By then digits are "<n>", a long hash may be "<hex>", and an
	//     older pattern may hold "<id>"; podIDs checks the lengths. A
	//     StatefulSet's pods ("web-0") already end in "<n>".
	rePod = regexp.MustCompile(`([a-z>])-(<hex>|<id>|(?:[bcdfghjklmnpqrstvwxz]|<n>)+)-(<id>|(?:[bcdfghjklmnpqrstvwxz]|<n>)+)([^a-z<-]|$)`)
	// An id-like key and the start of its value in raw text, for markCaseIDs.
	reIDValue = regexp.MustCompile(`(?i)\b(?:` + idKeys + `)["']?\s*[=:]\s*["']?$`)
)

// userStates are values of a userKeys key that say how it went, not who
// it was ("login=failed", "account=locked"): they stay, so success and
// failure are different problems.
var userStates = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`failed failure success succeeded successful ok true false yes no locked unlocked
		active inactive disabled enabled expired sent delivered bounced denied allowed blocked rejected accepted`) {
		userStates[w] = true
	}
}

// idKeys are keys whose values name one request, job or session;
// userKeys name an account.
const (
	idKeys   = `(?:[a-z]+[_.-])*(?:id|uuid|guid|requestid|traceid|spanid|userid|sessionid|correlationid|jobid|txid|txnid|msgid|messageid|eventid|orderid|accountid|deviceid|reqid|rid|jid|tid|sid|trace|span|session|token)`
	userKeys = `(?:user|username|login|logname|ruser|luser|account|email)`
)

// idPrefixes are the words a generated id follows, with "_" or "-":
// request, trace, span, session, transaction, message, job, event and
// correlation ids ("req_", "sess-", "txn_", "evt_", "corr-", "cid-",
// "rid-"), and order, customer and user ids ("ord_", "cus_", "usr_"). A
// token of letters only after one is an id when it reads as random letters
// ("req_pxvjxvmupm"), not when it reads as a word ("request_timeout",
// "session-expired"); see prefixedIDs. Ids with digits are idTokens' after
// any short prefix. The list is short on purpose: "in", "ch", "pi", "sub"
// or "order" start words ("in-flight", "sub-task", "order-service") as
// often as ids, and their ids have digits.
var idPrefixes = map[string]bool{"req": true, "request": true, "trace": true, "span": true, "sess": true, "session": true,
	"txn": true, "tx": true, "msg": true, "job": true, "evt": true, "corr": true, "cid": true, "rid": true,
	"ord": true, "cus": true, "usr": true}

// follows[c-'a'] lists the letters that follow c in English words: each
// pair is in at least four of some 4,000 words of technical English
// (documentation of Go's standard library, this project's, and names of
// common software). randomLetters counts the pairs not listed.
var follows = [26]string{
	"bcdefgijklmnprstuvwxyz",    // a
	"abcdehijloprstuy",          // b
	"acdeghikloqrstuvy",         // c
	"abcdefgijlmnorstuvwy",      // d
	"abcdefghijklmnopqrstuvwxy", // e
	"acdefilorstuwy",            // f
	"aceghilmnorstu",            // g
	"aeilmorstuy",               // h
	"abcdefgklmnopqrstvxz",      // i
	"aeou",                      // j
	"aegilnopsuw",               // k
	"abcdefgilmopstuvy",         // l
	"abceiklmnopsu",             // m
	"abcdefghiklmnoprstuvwy",    // n
	"abcdefgiklmnoprstuvwx",     // o
	"acdeghikloprstuy",          // p
	"lu",                        // q
	"abcdefghiklmnoprstuvwy",    // r
	"acefghiklmnopqstuwy",       // s
	"abcdefghilmoprstuwyz",      // t
	"abcdefgilmnoprstv",         // u
	"aeio",                      // v
	"aehiklnors",                // w
	"aceipt",                    // x
	"eilmnoprstwz",              // y
	"aeio",                      // z
}

// onsets are the two consonants an English word can start with.
var onsets = map[string]bool{"bl": true, "br": true, "ch": true, "cl": true, "cr": true, "dr": true, "dw": true, "fl": true, "fr": true,
	"gh": true, "gl": true, "gn": true, "gr": true, "kl": true, "kn": true, "ph": true, "pl": true, "pr": true, "ps": true, "pt": true,
	"sc": true, "sh": true, "sk": true, "sl": true, "sm": true, "sn": true, "sp": true, "sq": true, "st": true, "sw": true, "th": true,
	"tr": true, "tw": true, "wh": true, "wr": true}

// unitTypes are systemd's unit suffixes: "postgresql@14-main.service" is
// not a mail address.
var unitTypes = map[string]bool{"service": true, "socket": true, "timer": true, "target": true, "mount": true, "path": true, "slice": true, "scope": true, "device": true, "swap": true, "automount": true}

// Pattern reduces a log message to a template by replacing variable parts
// (timestamps, UUIDs, IPs, hex ids, numbers, generated ids, mail addresses,
// account names) with placeholders, so that repeats of the same problem
// share a pattern. Pattern(Pattern(x)) == Pattern(x) and
// Normalize(Pattern(x)) == Pattern(x).
func Pattern(msg string) string {
	if len(msg) > patternHead {
		msg = msg[:patternHead]
	}
	return clip(Normalize(msg), patternMax)
}

// A pattern is read from the first patternHead bytes of a message (a
// learned rule keeps that much of its example) and cut to patternMax bytes
// after normalizing, at a space: a variable part of another length before
// the cut ("exec-10" or "exec-5") does not move it.
const (
	patternHead = 500
	patternMax  = 400
)

func clip(p string, n int) string {
	if len(p) <= n {
		return p
	}
	cut := strings.LastIndexByte(p[:n+1], ' ')
	if cut <= 0 {
		cut = n
		for cut > 0 && !utf8.RuneStart(p[cut]) {
			cut--
		}
	}
	return strings.TrimSpace(p[:cut])
}

// Normalize is Pattern for the whole of s, however long.
func Normalize(s string) string {
	return normalizeIDs(legacyNormalize(markCaseIDs(s)))
}

// PatternVersion changes whenever Pattern does: state keyed by fingerprints
// (alerting, caches) saved under another version is stale. 3: letters-only
// ids after an id prefix, and pod names.
const PatternVersion = 3

// Renormalize turns a pattern saved by an older version into the pattern
// of the same messages now, as far as the pattern alone tells (a generated
// id that only letter case gave away cannot be told any more; see
// learn.MigratePatterns). Renormalize(Pattern(x)) == Pattern(x).
func Renormalize(p string) string { return clip(Normalize(p), patternMax) }

// LegacyPattern is the Pattern of Lissandra before generated ids were
// normalized. Learned rules saved then hold it; see learn.MigratePatterns.
func LegacyPattern(msg string) string {
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return legacyNormalize(msg)
}

func legacyNormalize(s string) string {
	s = strings.ToLower(s)
	s = reTime.ReplaceAllString(s, "<ts>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reIP.ReplaceAllString(s, "<ip>")
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reNum.ReplaceAllString(s, "<n>")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// markCaseIDs replaces generated ids that only their letter case gives
// away, before the text is lowercased: a letters-only token of 8 or more
// with at least two capitals and two small letters after a short prefix
// and "_" ("req_QEPeJPPoTB", "cus_NffrFeUfNV"). Tokens with digits are left
// to normalizeIDs, which also sees patterns saved before this existed.
func markCaseIDs(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if !alnum(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && alnum(s[j]) {
			j++
		}
		tok := s[i:j]
		// only after a short prefix and "_": the case of a lone token
		// does not tell an id from a class name ("KafkaZkClient")
		if len(tok) >= 8 && (i >= 2 && s[i-1] == '_' && alnum(s[i-2]) && shortPrefix(s, i-1) || idValue(s, i)) && caseRandom(tok) {
			b.WriteString(s[last:i])
			b.WriteString("<id>")
			last = j
		}
		i = j
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// idValue reports whether s[i:] starts the value of an id-like key
// ("request_id=", "\"traceId\": \"").
func idValue(s string, i int) bool {
	if i == 0 || !strings.ContainsAny(s[max(0, i-3):i], "=:") {
		return false
	}
	return reIDValue.MatchString(s[max(0, i-40):i])
}

// shortPrefix reports whether the word ending at s[end] (exclusive) is
// 1-12 letters and digits starting with a letter, alone (not part of a
// longer snake_case name: "req_" yes, "max_conn_" no).
func shortPrefix(s string, end int) bool {
	k := end
	for k > 0 && alnum(s[k-1]) {
		k--
	}
	if n := end - k; n < 1 || n > 12 || !isLetter(s[k]) {
		return false
	}
	return k == 0 || s[k-1] != '_'
}

func alnum(c byte) bool { return '0' <= c && c <= '9' || isLetter(c) }

// identifier is the shape of a PascalCase or camelCase name: words of a
// capital and two or more small letters ("Device", "Request"), a two-letter
// word only before a longer one ("Of" in "OutOfMemoryError"), an acronym
// only before a word or at the end ("IOException", "HTTPServer",
// "PostgreSQL", "macOS", "URLs").
var identifier = regexp.MustCompile(`^(?:[a-z]{2,}|[A-Z][a-z]{2,}|[A-Z]{2,}[A-Z][a-z]{2,}|[A-Z][a-z][A-Z][a-z]{2,})(?:[A-Z][a-z]{2,}|[A-Z][a-z][A-Z][a-z]{2,}|[A-Z]{2,}[A-Z][a-z]{2,})*(?:[A-Z]{2,}s?)?$`)

// caseRandom reports whether tok, all letters, mixes case the way a
// generated id does.
func caseRandom(tok string) bool {
	up, lo := 0, 0
	for i := 0; i < len(tok); i++ {
		switch c := tok[i]; {
		case 'A' <= c && c <= 'Z':
			up++
		case 'a' <= c && c <= 'z':
			lo++
		default:
			return false
		}
	}
	return up >= 2 && lo >= 2 && !(identifier.MatchString(tok) && wordHumps(tok))
}

// abbrevs are words without a vowel that names are made of.
var abbrevs = map[string]bool{"http": true, "https": true, "html": true, "xml": true, "sql": true, "tcp": true, "udp": true, "dns": true,
	"rpc": true, "grpc": true, "ssh": true, "tls": true, "ssl": true, "ctx": true, "msg": true, "cfg": true, "pkg": true, "src": true,
	"dst": true, "svc": true, "mgr": true, "cmd": true, "std": true, "jwt": true, "smtp": true, "ntp": true, "nfs": true, "pdf": true}

// wordHumps reports whether every word of three or more small letters in
// tok, a name ("DeviceRequest"), has a vowel or is a known abbreviation
// ("HttpCtx"): "lwPqnqRTGs" has "Pqnq".
func wordHumps(tok string) bool {
	for i := 0; i < len(tok); {
		j := i + 1
		for j < len(tok) && 'a' <= tok[j] && tok[j] <= 'z' {
			j++
		}
		w := strings.ToLower(tok[i:j])
		if j-i >= 4 || j-i >= 3 && 'a' <= tok[i] && tok[i] <= 'z' {
			if !strings.ContainsAny(w, "aeiou") && !abbrevs[w] {
				return false
			}
		}
		i = j
	}
	return true
}

// normalizeIDs replaces the variable parts that remain after
// legacyNormalize, in its output: mail addresses, id-like key values,
// account names, generated ids that mix letters and digits (a digit run
// being "<n>" by then), letters-only ids after an id prefix, and the
// generated end of pod names.
func normalizeIDs(s string) string {
	if strings.Contains(s, "@") {
		s = reEmail.ReplaceAllStringFunc(s, func(m string) string {
			if unitTypes[reEmail.FindStringSubmatch(m)[1]] {
				return m
			}
			return "<email>"
		})
	}
	kvUser := strings.Contains(s, "user") || strings.Contains(s, "login") || strings.Contains(s, "logname") || strings.Contains(s, "account") || strings.Contains(s, "email")
	kvID := strings.Contains(s, "id=") || strings.Contains(s, `id"`) || strings.Contains(s, "trace") || strings.Contains(s, "span") || strings.Contains(s, "session") || strings.Contains(s, "token")
	if kvUser && strings.ContainsAny(s, "=:") {
		s = reKVUser.ReplaceAllStringFunc(s, func(m string) string {
			sm := reKVUser.FindStringSubmatch(m)
			v, q := sm[2], ""
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
				q, v = v[:1], v[1:len(v)-1]
			}
			if v == "" || holdsPlaceholderOnly(v) || userStates[v] {
				return m
			}
			return sm[1] + "=" + q + "<user>" + q
		})
		s = reKVUserJSON.ReplaceAllStringFunc(s, func(m string) string {
			sm := reKVUserJSON.FindStringSubmatch(m)
			if sm[3] == "" || holdsPlaceholderOnly(sm[3]) || userStates[sm[3]] {
				return m
			}
			return `"` + sm[1] + `"` + sm[2] + `"<user>"`
		})
	}
	if kvID && strings.ContainsAny(s, "=:") {
		s = reKVID.ReplaceAllStringFunc(s, func(m string) string {
			sm := reKVID.FindStringSubmatch(m)
			if sm[2] != sm[4] || !mixed(sm[3]) {
				return m
			}
			return sm[1] + "=" + sm[2] + "<id>" + sm[4]
		})
		s = reKVIDJSON.ReplaceAllStringFunc(s, func(m string) string {
			sm := reKVIDJSON.FindStringSubmatch(m)
			if !mixed(sm[3]) {
				return m
			}
			return `"` + sm[1] + `"` + sm[2] + `"<id>"`
		})
	}
	if strings.Contains(s, "user ") || strings.Contains(s, " for ") { // sshd's "failed password for root"
		s = reUserQuoted.ReplaceAllString(s, "$1<user>$3")
		s = reUser.ReplaceAllStringFunc(s, func(m string) string {
			sm := reUser.FindStringSubmatch(m)
			if isPlaceholder(sm[2]) {
				return m
			}
			return sm[1] + "<user>"
		})
	}
	return podIDs(prefixedIDs(idTokens(s)))
}

// mixed reports whether t, letters and "<n>", has both.
func mixed(t string) bool {
	return strings.Contains(t, "<n>") && strings.Trim(strings.ReplaceAll(t, "<n>", ""), "") != ""
}

// holdsPlaceholderOnly reports whether v is one placeholder, possibly in
// brackets ("<n>", "<<email>>").
func holdsPlaceholderOnly(v string) bool {
	return isPlaceholder(strings.TrimRight(strings.TrimLeft(v, "<([{"), ">)]}")) || isPlaceholder(v) ||
		len(v) > 2 && isPlaceholder(v[1:len(v)-1])
}

func isPlaceholder(v string) bool {
	switch v {
	case "<n>", "<id>", "<hex>", "<uuid>", "<ip>", "<ts>", "<email>", "<user>":
		return true
	}
	return false
}

// idTokens scans s for tokens of letters and "<n>" and replaces generated
// ids among them:
//   - a hex literal ("0x1d4", "<n>x<n>d<n>" by then) with <hex>;
//   - a token that switches between letters and digits three times or more
//     ("e3LK2IIpMb", "x2k4p") with <id>;
//   - one of six or more characters with letters and digits after a short
//     prefix and "_" or "-" ("req_xfaveapzy7", "web-6d5f7c8b9") with <id>.
func idTokens(s string) string {
	if !strings.Contains(s, "<n>") {
		return s
	}
	var b strings.Builder
	last := 0
	prevWordEnd, prevWordLen, prevWordStart := -1, 0, -1 // the letters-only token before, if any
	for i := 0; i < len(s); {
		j, atoms, switches, digits, letters, onlyLetters := scanToken(s, i)
		if j == i {
			i++
			continue
		}
		if atoms == 0 { // the name inside a placeholder ("<ts>")
			i, prevWordEnd = j, -1
			continue
		}
		repl := ""
		switch {
		case digits > 0 && strings.HasPrefix(s[i:], "<n>x") && hexTail(s[i+4:j]):
			repl = "<hex>"
		case switches >= 3:
			repl = "<id>"
		case digits > 0 && letters > 0 && atoms >= 6 && i > 0 && (s[i-1] == '_' || s[i-1] == '-') &&
			prevWordEnd == i-1 && prevWordLen >= 1 && prevWordLen <= 12 && (prevWordStart == 0 || !atomByte(s[prevWordStart-1])):
			repl = "<id>"
		}
		if repl != "" {
			b.WriteString(s[last:i])
			b.WriteString(repl)
			last = j
		}
		if onlyLetters {
			prevWordStart, prevWordEnd, prevWordLen = i, j, j-i
		} else {
			prevWordEnd = -1
		}
		i = j
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func atomByte(c byte) bool { return 'a' <= c && c <= 'z' || c == '>' }

// prefixedIDs replaces a token of six or more letters after an id prefix
// and "_" or "-" ("[req_pxvjxvmupm]") with <id> when it reads as random
// letters. It works on idTokens' output, where letter case is gone, so it
// also applies to patterns older versions saved. The prefix must be a word
// of its own ("x_req_…" is not one); the token must end where the letters
// do (a digit after it makes it idTokens').
func prefixedIDs(s string) string {
	if !strings.ContainsAny(s, "_-") {
		return s
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if !lowerLetter(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && lowerLetter(s[j]) {
			j++
		}
		if j+1 < len(s) && (s[j] == '_' || s[j] == '-') && idPrefixes[s[i:j]] && (i == 0 || !nameByte(s[i-1])) {
			k := j + 1
			m := k
			for m < len(s) && lowerLetter(s[m]) {
				m++
			}
			if m-k >= 6 && (m == len(s) || s[m] != '<') && randomLetters(s[k:m]) {
				b.WriteString(s[last:k])
				b.WriteString("<id>")
				last, i = m, m
				continue
			}
		}
		i = j
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func lowerLetter(c byte) bool { return 'a' <= c && c <= 'z' }

// nameByte reports whether c continues a name: a word before an id prefix
// makes it part of a longer one ("max_req_", "x-request-").
func nameByte(c byte) bool { return alnum(c) || c == '_' || c == '-' || c == '>' }

// randomLetters reports whether t, six or more small letters, reads as
// random letters rather than as a word: it has no vowel ("gltbdfnsfr"), or
// two or more letter pairs English words don't have (follows), counting a
// start none has ("lv…" but not "str…"). "pxvjxvmupm" has eight,
// "checkout" and "upstream" none. About 1% of words of technical English
// read as random, mostly abbreviations run together ("fchdir",
// "cmdline"); random letters do 79% of the time at 6 letters, 89% at 8
// and 95% at 10 (the others keep their own pattern).
func randomLetters(t string) bool {
	if !strings.ContainsAny(t, "aeiouy") {
		return true
	}
	odd := 0
	if !vowel(t[0]) && !vowel(t[1]) && !onsets[t[:2]] {
		odd++
	}
	for i := 0; i+1 < len(t) && odd < 2; i++ {
		if strings.IndexByte(follows[t[i]-'a'], t[i+1]) < 0 {
			odd++
		}
	}
	return odd >= 2
}

func vowel(c byte) bool { return strings.IndexByte("aeiouy", c) >= 0 }

// podIDs replaces the pod-template-hash and the random suffix of a pod
// name ("orders-java-5b7c9d-ktfjq", "web-<hex>-x<n>x<n>z") with
// "<id>-<id>", so one container's restarts are one problem. A part with
// digits needs at least two letters: "retry-<n>-<n>s" is no pod.
func podIDs(s string) string {
	if !strings.Contains(s, "-") {
		return s
	}
	return rePod.ReplaceAllStringFunc(s, func(m string) string {
		sm := rePod.FindStringSubmatch(m)
		hash, suffix := sm[2], sm[3]
		if !(hash == "<hex>" || hash == "<id>" || podPart(hash, 6, 10)) || !(suffix == "<id>" || podPart(suffix, 5, 5) && !abbrevs[suffix]) {
			return m
		}
		return sm[1] + "-<id>-<id>" + sm[4]
	})
}

// podPart reports whether t, consonants and "<n>", can be lo to hi
// characters long: letters only, lo to hi of them; with digits, at least
// two letters, and at most hi letters and digit runs together.
func podPart(t string, lo, hi int) bool {
	n := strings.Count(t, "<n>")
	letters := len(t) - 3*n
	if n == 0 {
		return lo <= letters && letters <= hi
	}
	return letters >= 2 && letters+n <= hi
}

// scanToken reads the token of letters and "<n>" at s[i:]: its end, its
// length in atoms, how often it switches between letters and digits, and
// how many of each it has.
func scanToken(s string, i int) (end, atoms, switches, digits, letters int, onlyLetters bool) {
	j, last := i, 0 // last: 1 letter, 2 digit
	for j < len(s) {
		k := 0
		switch {
		case 'a' <= s[j] && s[j] <= 'z':
			k = 1
			letters++
			j++
		case strings.HasPrefix(s[j:], "<n>"):
			// "<n>" inside another placeholder ("<n>" never is) or after
			// a letter run
			k = 2
			digits++
			j += 3
		default:
			goto done
		}
		atoms++
		if last != 0 && last != k {
			switches++
		}
		last = k
	}
done:
	// a token must not be a letter run inside a placeholder ("<ts>")
	if j > i && i > 0 && s[i-1] == '<' && j < len(s) && s[j] == '>' && letters == atoms {
		return j, 0, 0, 0, 0, false
	}
	return j, atoms, switches, digits, letters, digits == 0 && j > i
}

func hexTail(t string) bool {
	if t == "" {
		return false
	}
	for len(t) > 0 {
		switch {
		case strings.HasPrefix(t, "<n>"):
			t = t[3:]
		case 'a' <= t[0] && t[0] <= 'f':
			t = t[1:]
		default:
			return false
		}
	}
	return true
}

// Fingerprint identifies "the same problem from the same source". It drives
// grouping in the UI, notification throttling and the engine's answer cache.
func Fingerprint(sourceID, msg string) string {
	sum := sha1.Sum([]byte(sourceID + "\x00" + Pattern(msg)))
	return hex.EncodeToString(sum[:8])
}
