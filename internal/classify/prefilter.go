package classify

import (
	"regexp/syntax"
	"strings"
	"unicode"
	"unicode/utf8"
)

// prefilter is a cheap necessary condition for a regexp: every match
// contains at least one of its literals. When none of them occurs in the
// input, the regexp cannot match and need not run. This matters because
// rules are mostly unanchored, case-insensitive alternations, which Go
// evaluates with its slow NFA engine.
type prefilter struct {
	lits []literal
}

type literal struct {
	s    string // lowercased when fold is set
	fold bool   // compare case-insensitively
}

// newPrefilter derives a prefilter from pattern, or returns nil when no
// useful one exists (then the regexp always runs).
func newPrefilter(pattern string) *prefilter {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	lits, ok := required(re.Simplify())
	if !ok || len(lits) == 0 {
		return nil
	}
	for _, l := range lits {
		if l.s == "" {
			return nil
		}
	}
	return &prefilter{lits: lits}
}

// mayMatch reports whether s contains any of the literals. folded is s
// passed through foldASCII, computed once per input by the caller.
func (p *prefilter) mayMatch(s, folded string) bool {
	for _, l := range p.lits {
		if l.fold {
			if strings.Contains(folded, l.s) {
				return true
			}
		} else if strings.Contains(s, l.s) {
			return true
		}
	}
	return false
}

// required returns literals of which every match of re contains at least
// one. ok is false when no such set is known.
func required(re *syntax.Regexp) ([]literal, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		fold := re.Flags&syntax.FoldCase != 0
		s := string(re.Rune)
		if fold {
			// Only ASCII literals are folded safely by foldASCII below.
			for _, r := range re.Rune {
				if r >= utf8.RuneSelf {
					return nil, false
				}
			}
			s = strings.ToLower(s)
		}
		return []literal{{s: s, fold: fold}}, true
	case syntax.OpCapture, syntax.OpPlus:
		return required(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return required(re.Sub[0])
		}
		return nil, false
	case syntax.OpConcat:
		// Any one element's requirement is a requirement of the whole;
		// pick the most selective (longest shortest literal).
		var best []literal
		bestScore := 0
		for _, sub := range re.Sub {
			lits, ok := required(sub)
			if !ok {
				continue
			}
			if score := shortest(lits); score > bestScore {
				best, bestScore = lits, score
			}
		}
		return best, best != nil
	case syntax.OpAlternate:
		var all []literal
		for _, sub := range re.Sub {
			lits, ok := required(sub)
			if !ok {
				return nil, false
			}
			all = append(all, lits...)
		}
		return all, true
	}
	return nil, false
}

func shortest(lits []literal) int {
	n := -1
	for _, l := range lits {
		if n < 0 || len(l.s) < n {
			n = len(l.s)
		}
	}
	return n
}

// foldASCII lowercases s for comparison with folded ASCII literals. Besides
// ASCII letters it maps the two non-ASCII runes that case-fold onto ASCII
// letters in Go's regexp (U+017F ſ → s, U+212A Kelvin sign → k), so the
// prefilter never rejects input the regexp would match.
func foldASCII(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r == 'ſ':
			return 's'
		case r == 'K':
			return 'k'
		case r < utf8.RuneSelf:
			return r
		}
		return unicode.ToLower(r)
	}, s)
}
