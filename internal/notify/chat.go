package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/model"
)

func init() {
	Register(&Plugin{
		Type:        "slack",
		Label:       "Slack",
		Description: "Post to a Slack channel through an Incoming Webhook. Also works with Slack-compatible webhooks (Mattermost, Rocket.Chat).",
		Audience:    "Everyone in the channel the webhook posts to. Mention on critical can ping people.",
		Fields: []Field{
			{Key: "webhook_url", Label: "Webhook URL", Type: "password", Required: true, Secret: true, Placeholder: "https://hooks.slack.com/services/…", Help: "Create one under Slack app → Incoming Webhooks. ${env:NAME} reads it from the environment."},
			{Key: "username", Label: "Username", Type: "text", Placeholder: "Lissandra", Help: "Optional display name (ignored by newer Slack apps)."},
			{Key: "icon_emoji", Label: "Icon emoji", Type: "text", Placeholder: ":rotating_light:"},
			{Key: "channel", Label: "Channel override", Type: "text", Placeholder: "#alerts", Help: "Only honoured by legacy webhooks and Mattermost."},
			{Key: "mention", Label: "Mention on critical", Type: "text", Placeholder: "<!here>", Help: "Text prepended to critical alerts, e.g. <!channel> or <@U012AB3CD>.", Recipient: true},
		},
		New: func(s map[string]string) (Sender, error) {
			if err := checkURL(s["webhook_url"]); err != nil {
				return nil, err
			}
			return &slack{s: s}, nil
		},
	})
	Register(&Plugin{
		Type:        "teams",
		Label:       "Microsoft Teams",
		Description: "Post an Adaptive Card to a Teams channel through a Workflows (Power Automate) webhook or a legacy Incoming Webhook.",
		Audience:    "Everyone in the Teams channel the workflow posts to.",
		Fields: []Field{
			{Key: "webhook_url", Label: "Webhook URL", Type: "password", Required: true, Secret: true, Placeholder: "https://…logic.azure.com/workflows/…", Help: "In Teams: channel → Workflows → \"Post to a channel when a webhook request is received\"."},
		},
		New: func(s map[string]string) (Sender, error) {
			if err := checkURL(s["webhook_url"]); err != nil {
				return nil, err
			}
			return &teams{url: s["webhook_url"]}, nil
		},
	})
	Register(&Plugin{
		Type:        "discord",
		Label:       "Discord",
		Description: "Post an embed to a Discord channel webhook.",
		Audience:    "Everyone in the channel the webhook posts to.",
		Fields: []Field{
			{Key: "webhook_url", Label: "Webhook URL", Type: "password", Required: true, Secret: true, Placeholder: "https://discord.com/api/webhooks/…"},
			{Key: "username", Label: "Username", Type: "text", Placeholder: "Lissandra"},
		},
		New: func(s map[string]string) (Sender, error) {
			if err := checkURL(s["webhook_url"]); err != nil {
				return nil, err
			}
			return &discord{s: s}, nil
		},
	})
	Register(&Plugin{
		Type:        "googlechat",
		Label:       "Google Chat",
		Description: "Post to a Google Chat space through an incoming webhook.",
		Audience:    "Everyone in the space the webhook posts to.",
		Fields: []Field{
			{Key: "webhook_url", Label: "Webhook URL", Type: "password", Required: true, Secret: true, Placeholder: "https://chat.googleapis.com/v1/spaces/…/messages?key=…"},
		},
		New: func(s map[string]string) (Sender, error) {
			if err := checkURL(s["webhook_url"]); err != nil {
				return nil, err
			}
			return &googleChat{url: s["webhook_url"]}, nil
		},
	})
	Register(&Plugin{
		Type:        "pagerduty",
		Label:       "PagerDuty",
		Description: "Trigger PagerDuty incidents through the Events API v2. Repeats of the same problem share a dedup key. Gets right-away alerts only, never digests.",
		Audience:    "Whoever is on call for the PagerDuty service this routing key belongs to.",
		NoDigest:    true,
		Fields: []Field{
			{Key: "routing_key", Label: "Integration (routing) key", Type: "password", Required: true, Secret: true},
			{Key: "api_url", Label: "Events API URL", Type: "text", Default: "https://events.pagerduty.com/v2/enqueue", Help: "Use https://events.eu.pagerduty.com/v2/enqueue for EU accounts."},
		},
		New: func(s map[string]string) (Sender, error) {
			if err := checkURL(s["api_url"]); err != nil {
				return nil, err
			}
			return &pagerDuty{s: s}, nil
		},
	})
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("a valid http(s) URL is required")
	}
	return nil
}

// --- Slack ---

type slack struct{ s map[string]string }

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (n *slack) Send(ctx context.Context, m *Message) error {
	if m.Digest != nil {
		return n.post(ctx, m.Title(), m.Digest.Highest, n.digestBlocks(m))
	}
	esc := slackEscaper.Replace
	head := fmt.Sprintf("*%s*", esc(m.Headline()))
	if m.Event.Severity == model.SeverityCritical && n.s["mention"] != "" {
		head = n.s["mention"] + " " + head
	}
	var ctxParts []string
	for _, f := range m.Facts() {
		if f.Name == "Fingerprint" {
			continue
		}
		ctxParts = append(ctxParts, fmt.Sprintf("*%s:* %s", f.Name, esc(f.Value)))
	}
	blocks := []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": head}},
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "```" + esc(strings.ReplaceAll(m.Excerpt(2800), "```", "'''")) + "```"}},
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": model.Truncate(strings.Join(ctxParts, "  |  "), 2900)}}})
	if m.Link != "" {
		blocks = append(blocks, slackButton("Open in Lissandra", m.Link))
	}
	return n.post(ctx, m.Title(), m.Event.Severity, blocks)
}

// slackDigestEntries is how many digest entries a Slack message lists:
// with the header, overflow, Why and button it stays under Slack's 20
// blocks per attachment.
const slackDigestEntries = 15

// digestBlocks lays a digest out as one section per entry. The mention is
// used only when critical alerts were held back.
func (n *slack) digestBlocks(m *Message) []map[string]any {
	esc := slackEscaper.Replace
	head := fmt.Sprintf("*%s*", esc(m.Title()))
	if m.Digest.CriticalHeld > 0 && n.s["mention"] != "" {
		head = n.s["mention"] + " " + head
	}
	section := func(text string) map[string]any {
		return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": model.Truncate(text, 3000)}}
	}
	context := func(text string) map[string]any {
		return map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": model.Truncate(text, 2900)}}}
	}
	blocks := []map[string]any{section(head)}
	lines, more := m.digestLines(slackDigestEntries)
	for _, l := range lines {
		count := l.count
		if l.link != "" {
			count = fmt.Sprintf("<%s|%s>", l.link, l.count)
		}
		text := fmt.Sprintf("*%s* %s  %s  %s", l.severity, count, esc(l.what), esc(l.when))
		if l.flags != "" {
			text += "  _" + esc(l.flags) + "_"
		}
		sample := strings.Join(strings.Fields(strings.ReplaceAll(model.Truncate(l.sample, sampleBytes), "`", "'")), " ")
		if sample != "" {
			text += "\n`" + esc(sample) + "`"
		}
		blocks = append(blocks, section(text))
	}
	if more != "" {
		blocks = append(blocks, context(esc(more)))
	}
	if m.Reason.Text != "" {
		blocks = append(blocks, context("*Why:* "+esc(m.Reason.Text)))
	}
	if m.Link != "" {
		blocks = append(blocks, slackButton("Open digest", m.Link))
	}
	return blocks
}

func slackButton(text, url string) map[string]any {
	return map[string]any{"type": "actions", "elements": []map[string]any{{
		"type": "button", "text": map[string]any{"type": "plain_text", "text": text}, "url": url,
	}}}
}

func (n *slack) post(ctx context.Context, text string, sev model.Severity, blocks []map[string]any) error {
	payload := map[string]any{
		"text":        text,
		"attachments": []map[string]any{{"color": severityColor(sev), "blocks": blocks}},
	}
	for _, k := range []string{"username", "icon_emoji", "channel"} {
		if v := n.s[k]; v != "" {
			payload[k] = v
		}
	}
	return postJSON(ctx, n.s["webhook_url"], payload, nil)
}

// --- Microsoft Teams ---

type teams struct{ url string }

// teamsMaxPayload keeps a card under Teams' 28 KB message limit.
const teamsMaxPayload = 27 << 10

func (n *teams) Send(ctx context.Context, m *Message) error {
	color := "Default"
	switch sev := m.severity(); {
	case sev.AtLeast(model.SeverityError):
		color = "Attention"
	case sev == model.SeverityWarning:
		color = "Warning"
	case sev == model.SeverityInfo:
		color = "Good"
	}
	var body []map[string]any
	actionTitle := "Open in Lissandra"
	if m.Digest != nil {
		actionTitle = "Open digest"
		body = teamsDigestBody(m, color, true)
		if payload, _ := json.Marshal(teamsPayload(body, actionTitle, m.Link)); len(payload) >= teamsMaxPayload {
			body = teamsDigestBody(m, color, false) // without samples
		}
	} else {
		facts := []map[string]string{}
		for _, f := range m.Facts() {
			facts = append(facts, map[string]string{"title": f.Name, "value": f.Value})
		}
		body = []map[string]any{
			{"type": "TextBlock", "text": m.Headline(), "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
			{"type": "TextBlock", "text": m.Excerpt(2000), "fontType": "Monospace", "wrap": true},
			{"type": "FactSet", "facts": facts},
		}
	}
	return postJSON(ctx, n.url, teamsPayload(body, actionTitle, m.Link), nil)
}

const teamsDigestEntries = 15

func teamsDigestBody(m *Message, color string, samples bool) []map[string]any {
	body := []map[string]any{
		{"type": "TextBlock", "text": m.Title(), "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
	}
	if m.Reason.Text != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": m.Reason.Text, "isSubtle": true, "wrap": true})
	}
	lines, more := m.digestLines(teamsDigestEntries)
	for _, l := range lines {
		text := l.head
		if l.flags != "" {
			text += "  " + l.flags
		}
		body = append(body, map[string]any{"type": "TextBlock", "text": text, "weight": "Bolder", "wrap": true, "spacing": "Medium"})
		if samples && l.sample != "" {
			body = append(body, map[string]any{"type": "TextBlock", "text": model.Truncate(l.sample, 200), "fontType": "Monospace", "wrap": true, "spacing": "None"})
		}
	}
	if more != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": more, "isSubtle": true, "wrap": true})
	}
	return body
}

func teamsPayload(body []map[string]any, actionTitle, link string) map[string]any {
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"msteams": map[string]any{"width": "Full"},
		"body":    body,
	}
	if link != "" {
		card["actions"] = []map[string]any{{"type": "Action.OpenUrl", "title": actionTitle, "url": link}}
	}
	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card,
		}},
	}
}

// --- Discord ---

type discord struct{ s map[string]string }

// Discord's embed limits.
const (
	discordDigestEntries = 10
	discordDescription   = 4096
	discordEmbed         = 6000
)

func (n *discord) Send(ctx context.Context, m *Message) error {
	color, _ := strconv.ParseInt(strings.TrimPrefix(severityColor(m.severity()), "#"), 16, 64)
	var embed map[string]any
	if m.Digest != nil {
		embed = discordDigest(m)
		embed["color"] = color
	} else {
		fields := []map[string]any{}
		for _, f := range m.Facts() {
			fields = append(fields, map[string]any{"name": f.Name, "value": model.Truncate(f.Value, 1000), "inline": len(f.Value) < 40})
		}
		embed = map[string]any{
			"title":       model.Truncate(m.Headline(), 250),
			"description": "```" + strings.ReplaceAll(m.Excerpt(3800), "```", "'''") + "```",
			"color":       color,
			"fields":      fields,
			"timestamp":   m.Event.Timestamp.UTC().Format(time.RFC3339),
		}
	}
	if m.Link != "" {
		embed["url"] = m.Link
	}
	payload := map[string]any{"embeds": []any{embed}, "allowed_mentions": map[string]any{"parse": []string{}}}
	if u := n.s["username"]; u != "" {
		payload["username"] = u
	}
	return postJSON(ctx, n.s["webhook_url"], payload, nil)
}

// discordDigest is a digest as one embed: entry lines in the description,
// the Why in the footer, within Discord's limits.
func discordDigest(m *Message) map[string]any {
	title := model.Truncate(m.Title(), 250)
	footer := model.Truncate(m.Reason.Text, 2000)
	budget := min(discordDescription, discordEmbed-len(title)-len(footer)-100)
	lines, more := m.digestLines(discordDigestEntries)
	var b strings.Builder
	for i, l := range lines {
		var e strings.Builder
		fmt.Fprintf(&e, "**%s** %s  %s  %s", l.severity, l.count, discordEscape(l.what), l.when)
		if l.flags != "" {
			e.WriteString("  _" + l.flags + "_")
		}
		if sample := strings.Join(strings.Fields(strings.ReplaceAll(model.Truncate(l.sample, 120), "`", "'")), " "); sample != "" {
			e.WriteString("\n`" + sample + "`")
		}
		e.WriteString("\n")
		if b.Len()+e.Len()+len(more)+40 > budget {
			more = fmt.Sprintf("+ %d more problems in the digest", len(lines)-i+m.Digest.OtherPatterns)
			break
		}
		b.WriteString(e.String())
	}
	if more != "" {
		b.WriteString(more)
	}
	embed := map[string]any{
		"title":       title,
		"description": model.Truncate(b.String(), budget),
		"timestamp":   m.Digest.To.UTC().Format(time.RFC3339),
	}
	if footer != "" {
		embed["footer"] = map[string]any{"text": footer}
	}
	return embed
}

var discordEscaper = strings.NewReplacer("*", "\\*", "_", "\\_", "~", "\\~", "`", "'", "|", "\\|")

func discordEscape(s string) string { return discordEscaper.Replace(s) }

// --- Google Chat ---

type googleChat struct{ url string }

// gchatEscaper neutralises Google Chat markup in log-derived text: <…>
// would otherwise create mentions (<users/all>) or disguised links.
var gchatEscaper = strings.NewReplacer("<", "‹", ">", "›")

// Google Chat digests: entries listed, and the text's length.
const (
	gchatDigestEntries = 15
	gchatMaxText       = 4000
)

func (n *googleChat) Send(ctx context.Context, m *Message) error {
	esc := gchatEscaper.Replace
	var b strings.Builder
	if m.Digest != nil {
		return postJSON(ctx, n.url, map[string]any{"text": gchatDigest(m)}, nil)
	}
	fmt.Fprintf(&b, "*%s*\n```%s```\n", esc(m.Headline()), esc(strings.ReplaceAll(m.Excerpt(3000), "```", "'''")))
	for _, f := range m.Facts() {
		fmt.Fprintf(&b, "*%s:* %s\n", f.Name, esc(f.Value))
	}
	if m.Link != "" {
		fmt.Fprintf(&b, "<%s|Open in Lissandra>\n", m.Link)
	}
	return postJSON(ctx, n.url, map[string]any{"text": b.String()}, nil)
}

// gchatDigest renders a digest as text lines within gchatMaxText.
func gchatDigest(m *Message) string {
	esc := gchatEscaper.Replace
	var tail strings.Builder
	if m.Reason.Text != "" {
		fmt.Fprintf(&tail, "_%s_\n", esc(m.Reason.Text))
	}
	if m.Link != "" {
		fmt.Fprintf(&tail, "<%s|Open digest>\n", m.Link)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%s*\n", esc(m.Title()))
	lines, more := m.digestLines(gchatDigestEntries)
	for i, l := range lines {
		var e strings.Builder
		fmt.Fprintf(&e, "*%s* %s  %s  %s", l.severity, l.count, esc(l.what), esc(l.when))
		if l.flags != "" {
			e.WriteString("  " + esc(l.flags))
		}
		if sample := strings.Join(strings.Fields(strings.ReplaceAll(model.Truncate(l.sample, 200), "`", "'")), " "); sample != "" {
			e.WriteString("\n`" + esc(sample) + "`")
		}
		e.WriteString("\n")
		if b.Len()+e.Len()+len(more)+tail.Len()+60 > gchatMaxText {
			more = fmt.Sprintf("+ %d more problems in the digest", len(lines)-i+m.Digest.OtherPatterns)
			break
		}
		b.WriteString(e.String())
	}
	if more != "" {
		b.WriteString(esc(more) + "\n")
	}
	b.WriteString(tail.String())
	return model.Truncate(b.String(), gchatMaxText)
}

// --- PagerDuty ---

type pagerDuty struct{ s map[string]string }

func (n *pagerDuty) Send(ctx context.Context, m *Message) error {
	if m.Digest != nil { // the dispatcher never queues one (Plugin.NoDigest)
		return Permanent(ErrNoDigest)
	}
	sev := "info"
	switch {
	case m.Event.Severity == model.SeverityCritical:
		sev = "critical"
	case m.Event.Severity == model.SeverityError:
		sev = "error"
	case m.Event.Severity == model.SeverityWarning:
		sev = "warning"
	}
	details := map[string]any{"message": m.Excerpt(4000)}
	for _, f := range m.Facts() {
		details[strings.ToLower(strings.ReplaceAll(f.Name, " ", "_"))] = f.Value
	}
	payload := map[string]any{
		"routing_key":  n.s["routing_key"],
		"event_action": "trigger",
		"dedup_key":    "lissandra-" + m.Event.Fingerprint,
		"payload": map[string]any{
			"summary":        model.Truncate(m.Title()+": "+m.Event.Message, 1000),
			"source":         m.Event.SourceName,
			"severity":       sev,
			"timestamp":      m.Event.Timestamp.UTC().Format(time.RFC3339),
			"class":          m.Event.Category,
			"group":          m.Event.SourceName,
			"custom_details": details,
		},
	}
	if m.Link != "" {
		payload["links"] = []map[string]string{{"href": m.Link, "text": "Open in Lissandra"}}
	}
	return postJSON(ctx, n.s["api_url"], payload, nil)
}
