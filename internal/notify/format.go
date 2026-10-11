package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

// Title is a one-line summary such as "[ERROR] security · api-gateway".
func (m *Message) Title() string {
	prefix := ""
	if m.Test {
		prefix = "[TEST] "
	}
	return fmt.Sprintf("%s[%s] %s · %s", prefix, strings.ToUpper(string(m.Event.Severity)), m.Event.Category, m.Event.SourceName)
}

// Excerpt returns the log message trimmed to n bytes.
func (m *Message) Excerpt(n int) string {
	return model.Truncate(m.Event.Message, n)
}

// Fact is a labelled value shown in rich notification formats.
type Fact struct {
	Name  string
	Value string
}

// Facts lists the event's classification details.
func (m *Message) Facts() []Fact {
	ev := &m.Event
	facts := []Fact{
		{"Severity", string(ev.Severity)},
		{"Category", ev.Category},
		{"Source", ev.SourceName},
		{"Time", ev.Timestamp.UTC().Format(time.RFC3339)},
	}
	if len(ev.Tags) > 0 {
		facts = append(facts, Fact{"Tags", strings.Join(ev.Tags, ", ")})
	}
	if len(ev.MatchedRules) > 0 {
		facts = append(facts, Fact{"Rules", strings.Join(ev.MatchedRules, ", ")})
	}
	if ev.Summary != "" {
		facts = append(facts, Fact{"AI summary", ev.Summary})
	}
	if m.RouteName != "" {
		facts = append(facts, Fact{"Route", m.RouteName})
	}
	if m.Suppressed > 0 {
		facts = append(facts, Fact{"Suppressed", fmt.Sprintf("%d similar event(s) since the last alert", m.Suppressed)})
	}
	facts = append(facts, Fact{"Fingerprint", ev.Fingerprint})
	return facts
}

// PlainText renders the message for text-only channels.
func (m *Message) PlainText() string {
	var b strings.Builder
	b.WriteString(m.Title())
	b.WriteString("\n\n")
	b.WriteString(m.Excerpt(3000))
	b.WriteString("\n\n")
	for _, f := range m.Facts() {
		fmt.Fprintf(&b, "%s: %s\n", f.Name, f.Value)
	}
	if m.Link != "" {
		fmt.Fprintf(&b, "\nOpen in Laya: %s\n", m.Link)
	}
	return b.String()
}

// severityColor returns a hex color per severity.
func severityColor(s model.Severity) string {
	switch s {
	case model.SeverityCritical:
		return "#B3261E"
	case model.SeverityError:
		return "#E5484D"
	case model.SeverityWarning:
		return "#F5A524"
	case model.SeverityNotice:
		return "#3E8ED0"
	case model.SeverityInfo:
		return "#2F9E6B"
	default:
		return "#8A8F98"
	}
}

func sampleMessage() *Message {
	now := time.Now().UTC()
	return &Message{
		Test: true,
		Event: model.Event{
			ID:           "test",
			SourceID:     "test",
			SourceName:   "laya-test",
			ReceivedAt:   now,
			Timestamp:    now,
			Message:      "This is a test notification from Laya. If you can read this, the channel is configured correctly.",
			Severity:     model.SeverityError,
			Category:     "application",
			Tags:         []string{"test"},
			MatchedRules: []string{"Test rule"},
			Classifier:   "rules",
			Fingerprint:  "0000000000000000",
		},
		RouteName: "Test route",
	}
}
