// Package model holds the types shared across Laya's pipeline stages.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// Severity is a normalized log severity. The zero value means "not yet known".
type Severity string

const (
	SeverityDebug    Severity = "debug"
	SeverityInfo     Severity = "info"
	SeverityNotice   Severity = "notice"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Severities lists every valid severity from lowest to highest.
var Severities = []Severity{SeverityDebug, SeverityInfo, SeverityNotice, SeverityWarning, SeverityError, SeverityCritical}

// Rank orders severities; unknown values rank 0.
func (s Severity) Rank() int {
	for i, v := range Severities {
		if v == s {
			return i + 1
		}
	}
	return 0
}

// Valid reports whether s is one of the known severities.
func (s Severity) Valid() bool { return s.Rank() > 0 }

// AtLeast reports whether s is the same as or more severe than other.
func (s Severity) AtLeast(other Severity) bool { return s.Rank() >= other.Rank() }

// ParseSeverity maps a free-form string onto a Severity. ok is false when the
// string isn't recognised.
func ParseSeverity(v string) (Severity, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "trace", "debug", "dbg", "verbose", "d", "t", "finest", "finer", "fine":
		return SeverityDebug, true
	case "info", "information", "informational", "inf", "i", "config":
		return SeverityInfo, true
	case "notice", "note", "n":
		return SeverityNotice, true
	case "warn", "warning", "wrn", "w":
		return SeverityWarning, true
	case "error", "err", "e", "severe", "fail", "failure":
		return SeverityError, true
	case "critical", "crit", "fatal", "ftl", "f", "panic", "emerg", "emergency", "alert", "c":
		return SeverityCritical, true
	}
	return "", false
}

// Uncategorized is the category assigned when no rule or classifier matched.
const Uncategorized = "uncategorized"

// Event is a single log record flowing through Laya, before and after
// classification.
type Event struct {
	ID         string         `json:"id"`
	SourceID   string         `json:"source_id"`
	SourceName string         `json:"source_name"`
	ReceivedAt time.Time      `json:"received_at"`
	Timestamp  time.Time      `json:"timestamp"`
	Level      string         `json:"level,omitempty"`
	Message    string         `json:"message"`
	Fields     map[string]any `json:"fields,omitempty"`

	// Classification results.
	Severity     Severity `json:"severity"`
	Category     string   `json:"category"`
	Tags         []string `json:"tags,omitempty"`
	MatchedRules []string `json:"matched_rules,omitempty"`
	Classifier   string   `json:"classifier"`
	Summary      string   `json:"summary,omitempty"`
	Fingerprint  string   `json:"fingerprint"`
}

// AddTag appends tag if it isn't already present.
func (e *Event) AddTag(tag string) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return
	}
	for _, t := range e.Tags {
		if t == tag {
			return
		}
	}
	e.Tags = append(e.Tags, tag)
}

// HasTag reports whether the event carries tag.
func (e *Event) HasTag(tag string) bool {
	for _, t := range e.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// NewID returns a random 16-character hex identifier.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewToken returns a random 48-character hex secret suitable for ingest tokens.
func NewToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
