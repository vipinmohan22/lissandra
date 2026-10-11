package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Lookup resolves a dot-separated path ("a.b.0.c") inside decoded JSON.
// A key that itself contains dots (e.g. "log.level" as a flat key) is tried
// before descending.
func Lookup(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	switch t := v.(type) {
	case map[string]any:
		if val, ok := t[path]; ok {
			return val, true
		}
		// Try every prefix that exists as a key, longest first.
		for i := len(path) - 1; i > 0; i-- {
			if path[i] != '.' {
				continue
			}
			if child, ok := t[path[:i]]; ok {
				if val, ok := Lookup(child, path[i+1:]); ok {
					return val, true
				}
			}
		}
	case []any:
		head, rest, _ := strings.Cut(path, ".")
		idx, err := strconv.Atoi(head)
		if err != nil || idx < 0 || idx >= len(t) {
			return nil, false
		}
		return Lookup(t[idx], rest)
	}
	return nil, false
}

// ToString renders a decoded JSON value for matching and display.
func ToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// Truncate shortens s to at most n bytes without splitting a UTF-8
// sequence, appending an ellipsis when it cuts.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
