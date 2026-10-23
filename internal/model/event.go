// Package model holds the types shared across Lissandra's pipeline stages.
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

// Event is a single log record flowing through Lissandra, before and after
// classification.
type Event struct {
	ID         string         `json:"id"`
	SourceID   string         `json:"source_id"`
	SourceName string         `json:"source_name"`
	ReceivedAt time.Time      `json:"received_at"`
	Timestamp  time.Time      `json:"timestamp,omitzero"` // empty only for file records with no time
	Level      string         `json:"level,omitempty"`
	Message    string         `json:"message"`
	Fields     map[string]any `json:"fields,omitempty"`

	// Classification results.
	Severity Severity `json:"severity"`
	// SeverityGuessed is set when Severity was inferred from keywords: the
	// record stated no level and no rule set one.
	SeverityGuessed bool          `json:"severity_guessed,omitempty"`
	Category        string        `json:"category"`
	Tags            []string      `json:"tags,omitempty"`
	MatchedRules    []string      `json:"matched_rules,omitempty"`
	Classifier      string        `json:"classifier"`
	Engine          *EngineResult `json:"engine,omitempty"` // the engine's answer, when it was asked
	// NoEngine is set by a matching rule that keeps the event from the engine.
	NoEngine    bool   `json:"-"`
	Fingerprint string `json:"fingerprint"`

	// Set on events read from an uploaded log file rather than a live source.
	ImportID   string `json:"import_id,omitempty"`
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`        // line the record starts on
	TimeSource string `json:"time_source,omitempty"` // "" (from the record), TimeInferred or TimeNone
	// Compacted is set when the message or fields were cut to keep the
	// event for browsing.
	Compacted bool `json:"compacted,omitempty"`
}

// EngineResult records what the engine answered for an event.
type EngineResult struct {
	Model              string             `json:"model"`                         // checkpoint that answered
	Category           string             `json:"category"`                      // its choice (Uncategorized: none fits)
	Confidence         float64            `json:"confidence"`                    // answer confidence of Category
	Top                map[string]float64 `json:"top,omitempty"`                 // most likely categories
	Severity           Severity           `json:"severity,omitempty"`            // when asked
	SeverityConfidence float64            `json:"severity_confidence,omitempty"` // answer confidence of Severity
	Threshold          float64            `json:"threshold"`                     // min_confidence when it was applied
	Applied            bool               `json:"applied"`                       // the answer changed the category or severity
	CategoryApplied    bool               `json:"category_applied,omitempty"`    // …the category
	SeverityApplied    bool               `json:"severity_applied,omitempty"`    // …the severity
	Truncated          bool               `json:"truncated,omitempty"`           // the event was cut to fit the model
}

// TimeSource values for events whose record had no timestamp.
const (
	TimeInferred = "inferred" // copied from the previous record in the file
	TimeNone     = "none"     // no timestamp seen yet; Timestamp is the import time
)

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
