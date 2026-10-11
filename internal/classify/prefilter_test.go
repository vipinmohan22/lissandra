package classify

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

// The prefilter must be sound: whenever it says "can't match", the regexp
// really doesn't match. This differential test checks that over the default
// rules and tricky patterns, with inputs built from pattern fragments,
// case variants and the runes Go's case folding maps onto ASCII.
func TestPrefilterNeverRejectsAMatch(t *testing.T) {
	patterns := []string{
		`(?i)oom[- ]?kill`, `(?i)jwt (expired|invalid)`, `(?i)(segmentation fault|sigsegv)`,
		`a(?i)b`, `(?i)a(?-i)B`, `(?i)k`, `(?i)s+x`, `(?i)(ab|)c`, `(?i)(ab)*c`, `(?i)(ab){2,3}`,
		`(?i)(ab){0,2}z`, `^\[?error`, `(?i)\bwarn(ing)?\b`, `x|(?i)y`, `(?i)ſ`, `(?i)straße`, `(?i)ǅ`,
		`(?i)σ`, `(?i)ı`, `(?i)İ`, `(?i)[a-c]x`, `"\s5\d\d\s`, `(?i)(?:foo|bar)baz`, `(?i)é`, `\x{212A}`,
		`\x{FFFD}`, `(?i)[\x{FFFD}]`, `caf\x{FFFD} login`,
	}
	c := config.Defaults()
	config.Normalize(c)
	for _, r := range c.Rules {
		for _, cd := range r.Conditions {
			if cd.Op == config.OpRegex {
				patterns = append(patterns, "(?i)"+cd.Value)
			}
		}
	}
	tricky := []string{"K", "k", "K", "s", "S", "ſ", "ß", "ẞ", "σ", "ς", "Σ", "ı", "I", "i", "İ", "ǅ", "ǆ", "Ǆ", "é", "É", "\xff", "\n", " "}
	rng := rand.New(rand.NewSource(1))
	checked := 0
	for _, pat := range patterns {
		re := regexp.MustCompile(pat)
		pre := newPrefilter(pat)
		if pre == nil {
			continue
		}
		var frags []string
		for _, l := range pre.lits {
			frags = append(frags, l.s, strings.ToUpper(l.s))
		}
		frags = append(frags, tricky...)
		for i := 0; i < 3000; i++ {
			var b strings.Builder
			for j := rng.Intn(6); j >= 0; j-- {
				f := frags[rng.Intn(len(frags))]
				// Randomly swap letters for fold-equivalent runes.
				f = strings.Map(func(r rune) rune {
					if rng.Intn(4) != 0 {
						return r
					}
					switch r {
					case 'k', 'K':
						return 'K'
					case 's', 'S':
						return 'ſ'
					}
					if rng.Intn(2) == 0 {
						return []rune(strings.ToUpper(string(r)))[0]
					}
					return r
				}, f)
				b.WriteString(f)
				if rng.Intn(3) == 0 {
					b.WriteString(tricky[rng.Intn(len(tricky))])
				}
			}
			in := b.String()
			if re.MatchString(in) && !pre.mayMatch(in, foldASCII(in)) {
				t.Fatalf("prefilter rejected a match: pattern %q input %q", pat, in)
			}
			checked++
		}
	}
	if checked < 50000 {
		t.Fatalf("only %d cases checked", checked)
	}
}

func TestPrefilterLiterals(t *testing.T) {
	cases := map[string][]string{
		`(?i)oom[- ]?kill`:          {"kill"},
		`(?i)jwt (expired|invalid)`: {"expired", "invalid"},
		`(?i)(ab|)c`:                {"c"},
		`deadlock`:                  {"deadlock"},
	}
	for pat, want := range cases {
		pre := newPrefilter(pat)
		if pre == nil {
			t.Errorf("%s: no prefilter", pat)
			continue
		}
		var got []string
		for _, l := range pre.lits {
			got = append(got, l.s)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: literals %q, want %q", pat, got, want)
		}
	}
	for _, pat := range []string{`(?i)é`, `.*`, `\d+`, `(?i)(a|b)?`, `x*`} {
		if newPrefilter(pat) != nil {
			t.Errorf("%s: expected no prefilter", pat)
		}
	}
}

func BenchmarkClassifyDefaultRules(b *testing.B) {
	msgs := []string{
		"2026-10-11T10:15:02Z INFO GET /api/orders/1234 200 12ms user=42 trace=3f2b9c1e-1111-2222-3333-444455556666",
		"2026-10-11T10:15:02Z ERROR payment failed: connection refused to db:5432",
		`level=info msg="cache hit ratio 93%" component=cache`,
	}
	c := config.Defaults()
	config.Normalize(c)
	e := New(c.Rules)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ev := model.Event{Message: msgs[i%len(msgs)], SourceID: "s"}
		e.Classify(&ev)
	}
}
