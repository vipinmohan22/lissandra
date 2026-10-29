package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/model"
)

// Title is a one-line summary such as "[ERROR] security · api-gateway", or
// for a digest "[DIGEST] 14 problems · 230 events · highest ERROR".
func (m *Message) Title() string {
	prefix := ""
	if m.Test {
		prefix = "[TEST] "
	}
	if dg := m.Digest; dg != nil {
		if dg.Reason == ReasonSummary {
			return fmt.Sprintf("%s[SUMMARY] %s held back · %s", prefix, plural(dg.Held, "alert"), heldSource(dg))
		}
		return fmt.Sprintf("%s[DIGEST] %s · %s · highest %s", prefix, plural(dg.Patterns, "problem"), plural(dg.Events, "event"), strings.ToUpper(string(dg.Highest)))
	}
	return fmt.Sprintf("%s[%s] %s · %s", prefix, strings.ToUpper(string(m.Event.Severity)), m.Event.Category, m.Event.SourceName)
}

// Headline is Title with what made an alert go right away, for chat
// headers and email subjects: "[ERROR] database · orders · new problem".
func (m *Message) Headline() string {
	if m.Digest != nil {
		return m.Title()
	}
	switch m.Reason.Code {
	case ReasonNew:
		return m.Title() + " · new problem"
	case ReasonSpike:
		return m.Title() + " · spiking"
	case ReasonWorse:
		return m.Title() + " · got worse"
	case ReasonRepeat:
		return m.Title() + " · still happening"
	}
	return m.Title()
}

// heldSource names the source most held entries of a summary came from,
// or how many sources they came from when no one source has most of them.
func heldSource(dg *Digest) string {
	count := map[string]int{}
	total := 0
	for _, e := range dg.Entries {
		if e.Held != "" {
			count[e.SourceName] += e.Count
			total += e.Count
		}
	}
	for name, n := range count {
		if 2*n > total {
			return name
		}
	}
	return plural(len(count), "source")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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

// today is the reference for clock texts: when the message was decided.
func (m *Message) today() time.Time {
	if m.at.IsZero() {
		return time.Now()
	}
	return m.at
}

// Facts lists why the message was sent and the event's classification
// details, or a digest's period and size.
func (m *Message) Facts() []Fact {
	var facts []Fact
	if m.Reason.Text != "" {
		facts = append(facts, Fact{"Why", m.Reason.Text})
	}
	if dg := m.Digest; dg != nil {
		facts = append(facts, Fact{"Period", fmtSpan(dg.From, dg.To, m.today())})
		if len(dg.Routes) > 0 {
			facts = append(facts, Fact{"Routes", strings.Join(dg.Routes, ", ")})
		}
		return append(facts,
			Fact{"Events", fmt.Sprint(dg.Events)},
			Fact{"Problems", fmt.Sprint(dg.Patterns)},
			Fact{"Highest", string(dg.Highest)})
	}
	ev := &m.Event
	facts = append(facts,
		Fact{"Severity", string(ev.Severity)},
		Fact{"Category", ev.Category},
		Fact{"Source", ev.SourceName},
		Fact{"Time", ev.Timestamp.UTC().Format(time.RFC3339)})
	if len(ev.Tags) > 0 {
		facts = append(facts, Fact{"Tags", strings.Join(ev.Tags, ", ")})
	}
	if len(ev.MatchedRules) > 0 {
		facts = append(facts, Fact{"Rules", strings.Join(ev.MatchedRules, ", ")})
	}
	if e := ev.Engine; e != nil && e.Applied {
		facts = append(facts, Fact{"Classified by", fmt.Sprintf("the engine (%s), %.0f%% confident", e.Model, e.Confidence*100)})
	}
	if m.RouteName != "" {
		facts = append(facts, Fact{"Route", m.RouteName})
	}
	if m.Suppressed > 0 {
		if !m.Since.IsZero() {
			facts = append(facts, Fact{"Suppressed", fmt.Sprintf("%d more since the alert at %s", m.Suppressed, fmtClock(m.Since, m.today()))})
		} else {
			facts = append(facts, Fact{"Suppressed", fmt.Sprintf("%d similar event(s) since the last alert", m.Suppressed)})
		}
	}
	facts = append(facts, Fact{"Fingerprint", ev.Fingerprint})
	return facts
}

// PlainText renders the message for text-only channels.
func (m *Message) PlainText() string {
	var b strings.Builder
	b.WriteString(m.Title())
	b.WriteString("\n\n")
	if m.Digest != nil {
		lines, more := m.digestLines(maxDigestListed)
		for _, l := range lines {
			b.WriteString(l.head)
			if l.flags != "" {
				b.WriteString("  " + l.flags)
			}
			fmt.Fprintf(&b, "\n    %s\n", strings.ReplaceAll(l.sample, "\n", "\n    "))
			if l.link != "" {
				fmt.Fprintf(&b, "    %s\n", l.link)
			}
		}
		if more != "" {
			b.WriteString(more + "\n")
		}
		if m.Reason.Text != "" {
			fmt.Fprintf(&b, "\nWhy: %s\n", m.Reason.Text)
		}
	} else {
		b.WriteString(m.Excerpt(3000))
		b.WriteString("\n\n")
		for _, f := range m.Facts() {
			fmt.Fprintf(&b, "%s: %s\n", f.Name, f.Value)
		}
	}
	if m.Link != "" {
		fmt.Fprintf(&b, "\nOpen in Lissandra: %s\n", m.Link)
	}
	return b.String()
}

// digestLine is one digest entry, rendered.
type digestLine struct {
	e        *DigestEntry
	severity string // "ERROR"
	count    string // "×120"
	what     string // "checkout-api · database"
	when     string // "10:02–10:29 UTC"
	head     string // the four above
	flags    string // "NEW | HELD (new) | REPEATS of 10:02 | 2 UNDELIVERED"
	sample   string
	link     string
}

// digestLines renders up to n entries and the overflow line ("+ 6 more
// problems (34 events)"; "" when everything is listed).
func (m *Message) digestLines(n int) ([]digestLine, string) {
	dg := m.Digest
	now := m.today()
	var lines []digestLine
	morePatterns, moreEvents := dg.OtherPatterns, dg.OtherEvents+dg.UntrackedEvents
	for i := range dg.Entries {
		e := &dg.Entries[i]
		if i >= n {
			morePatterns++
			moreEvents += e.Count
			continue
		}
		l := digestLine{e: e, severity: strings.ToUpper(string(e.Severity)), count: fmt.Sprintf("×%d", e.Count),
			what: sourceCategory(e.SourceName, e.Category), when: fmtSpan(e.First, e.Last, now), sample: e.Sample, link: e.Link}
		l.head = l.severity + " " + l.count + "  " + l.what + "  " + l.when
		var flags []string
		if e.New {
			flags = append(flags, "NEW")
		}
		if e.Held != "" {
			flags = append(flags, "HELD ("+e.Held+")")
		}
		if e.Repeats > 0 {
			flags = append(flags, "REPEATS of "+strings.TrimSuffix(fmtClock(e.AlertedAt, now), " UTC"))
		}
		if e.Undelivered > 0 {
			flags = append(flags, fmt.Sprintf("%d UNDELIVERED", e.Undelivered))
		}
		l.flags = strings.Join(flags, " | ")
		lines = append(lines, l)
	}
	more := ""
	switch {
	case morePatterns > 0:
		more = fmt.Sprintf("+ %s (%s)", plural(morePatterns, "more problem"), plural(moreEvents, "event"))
	case moreEvents > 0:
		more = fmt.Sprintf("+ %s", plural(moreEvents, "more event"))
	}
	return lines, more
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

// messageSeverity is the alert's severity, or a digest's highest.
func (m *Message) severity() model.Severity {
	if m.Digest != nil {
		return m.Digest.Highest
	}
	return m.Event.Severity
}

func sampleMessage(now time.Time) *Message {
	now = now.UTC()
	return &Message{
		ID:     model.NewID(),
		Kind:   KindAlert,
		Test:   true,
		Reason: Reason{ReasonTest, "Test notification."},
		at:     now,
		Event: model.Event{
			ID:           "test",
			SourceID:     "test",
			SourceName:   "lissandra-test",
			ReceivedAt:   now,
			Timestamp:    now,
			Message:      "This is a test notification from Lissandra. If you can read this, the channel is configured correctly.",
			Severity:     model.SeverityError,
			Category:     "application",
			Tags:         []string{"test"},
			MatchedRules: []string{"Test rule"},
			Classifier:   "rules",
			Fingerprint:  "0000000000000000",
		},
		RouteName: "Test route",
		Routes:    []string{"Test route"},
	}
}
