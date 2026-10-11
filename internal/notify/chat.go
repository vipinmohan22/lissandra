package notify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

func init() {
	Register(&Plugin{
		Type:        "slack",
		Label:       "Slack",
		Description: "Post to a Slack channel through an Incoming Webhook. Also works with Slack-compatible webhooks (Mattermost, Rocket.Chat).",
		Fields: []Field{
			{Key: "webhook_url", Label: "Webhook URL", Type: "password", Required: true, Secret: true, Placeholder: "https://hooks.slack.com/services/…", Help: "Create one under Slack app → Incoming Webhooks. ${env:NAME} reads it from the environment."},
			{Key: "username", Label: "Username", Type: "text", Placeholder: "Laya", Help: "Optional display name (ignored by newer Slack apps)."},
			{Key: "icon_emoji", Label: "Icon emoji", Type: "text", Placeholder: ":rotating_light:"},
			{Key: "channel", Label: "Channel override", Type: "text", Placeholder: "#alerts", Help: "Only honoured by legacy webhooks and Mattermost."},
			{Key: "mention", Label: "Mention on critical", Type: "text", Placeholder: "<!here>", Help: "Text prepended to critical alerts, e.g. <!channel> or <@U012AB3CD>."},
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
		Fields: []Field{
			{Key: "webhook_url", Label: "Webhook URL", Type: "password", Required: true, Secret: true, Placeholder: "https://discord.com/api/webhooks/…"},
			{Key: "username", Label: "Username", Type: "text", Placeholder: "Laya"},
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
		Description: "Trigger PagerDuty incidents through the Events API v2. Repeats of the same problem share a dedup key.",
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
	esc := slackEscaper.Replace
	head := fmt.Sprintf("*%s*", esc(m.Title()))
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
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []map[string]any{{
			"type": "button", "text": map[string]any{"type": "plain_text", "text": "Open in Laya"}, "url": m.Link,
		}}})
	}
	payload := map[string]any{
		"text":        m.Title(),
		"attachments": []map[string]any{{"color": severityColor(m.Event.Severity), "blocks": blocks}},
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

func (n *teams) Send(ctx context.Context, m *Message) error {
	color := "Default"
	switch {
	case m.Event.Severity.AtLeast(model.SeverityError):
		color = "Attention"
	case m.Event.Severity == model.SeverityWarning:
		color = "Warning"
	case m.Event.Severity == model.SeverityInfo:
		color = "Good"
	}
	facts := []map[string]string{}
	for _, f := range m.Facts() {
		facts = append(facts, map[string]string{"title": f.Name, "value": f.Value})
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"msteams": map[string]any{"width": "Full"},
		"body": []map[string]any{
			{"type": "TextBlock", "text": m.Title(), "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
			{"type": "TextBlock", "text": m.Excerpt(2000), "fontType": "Monospace", "wrap": true},
			{"type": "FactSet", "facts": facts},
		},
	}
	if m.Link != "" {
		card["actions"] = []map[string]any{{"type": "Action.OpenUrl", "title": "Open in Laya", "url": m.Link}}
	}
	payload := map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card,
		}},
	}
	return postJSON(ctx, n.url, payload, nil)
}

// --- Discord ---

type discord struct{ s map[string]string }

func (n *discord) Send(ctx context.Context, m *Message) error {
	color, _ := strconv.ParseInt(strings.TrimPrefix(severityColor(m.Event.Severity), "#"), 16, 64)
	fields := []map[string]any{}
	for _, f := range m.Facts() {
		fields = append(fields, map[string]any{"name": f.Name, "value": model.Truncate(f.Value, 1000), "inline": len(f.Value) < 40})
	}
	embed := map[string]any{
		"title":       model.Truncate(m.Title(), 250),
		"description": "```" + strings.ReplaceAll(m.Excerpt(3800), "```", "'''") + "```",
		"color":       color,
		"fields":      fields,
		"timestamp":   m.Event.Timestamp.UTC().Format(time.RFC3339),
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

// --- Google Chat ---

type googleChat struct{ url string }

// gchatEscaper neutralises Google Chat markup in log-derived text: <…>
// would otherwise create mentions (<users/all>) or disguised links.
var gchatEscaper = strings.NewReplacer("<", "‹", ">", "›")

func (n *googleChat) Send(ctx context.Context, m *Message) error {
	esc := gchatEscaper.Replace
	var b strings.Builder
	fmt.Fprintf(&b, "*%s*\n```%s```\n", esc(m.Title()), esc(strings.ReplaceAll(m.Excerpt(3000), "```", "'''")))
	for _, f := range m.Facts() {
		fmt.Fprintf(&b, "*%s:* %s\n", f.Name, esc(f.Value))
	}
	if m.Link != "" {
		fmt.Fprintf(&b, "<%s|Open in Laya>\n", m.Link)
	}
	return postJSON(ctx, n.url, map[string]any{"text": b.String()}, nil)
}

// --- PagerDuty ---

type pagerDuty struct{ s map[string]string }

func (n *pagerDuty) Send(ctx context.Context, m *Message) error {
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
		"dedup_key":    "laya-" + m.Event.Fingerprint,
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
		payload["links"] = []map[string]string{{"href": m.Link, "text": "Open in Laya"}}
	}
	return postJSON(ctx, n.s["api_url"], payload, nil)
}
