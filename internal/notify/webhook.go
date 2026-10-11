package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

const defaultWebhookTemplate = `{{ json .Payload }}`

func init() {
	Register(&Plugin{
		Type:  "webhook",
		Label: "Generic webhook",
		Description: "Send an HTTP request to any endpoint (Opsgenie, ntfy, an internal service, …). " +
			"The body is a Go text/template; leave it empty to send Laya's standard JSON payload.",
		Fields: []Field{
			{Key: "url", Label: "URL", Type: "text", Required: true, Placeholder: "https://example.com/hooks/laya"},
			{Key: "method", Label: "Method", Type: "select", Options: []string{"POST", "PUT", "PATCH"}, Default: "POST"},
			{Key: "headers", Label: "Headers", Type: "textarea", Secret: true, Placeholder: "Authorization: Bearer ${env:HOOK_TOKEN}\nX-Team: platform", Help: "One \"Name: value\" per line."},
			{Key: "content_type", Label: "Content type", Type: "text", Default: "application/json"},
			{Key: "body_template", Label: "Body template", Type: "textarea", Placeholder: `{"text": {{ json .Title }}, "severity": {{ json .Event.Severity }}}`,
				Help: "Fields: .Title .Text .Link .RouteName .Suppressed .Test .Event (.Message .Severity .Category .SourceName .Tags .Summary .Fingerprint .Fields …) and .Payload. Functions: json, upper, lower, truncate."},
		},
		New: newWebhook,
	})
}

type webhook struct {
	url, method, contentType string
	headers                  map[string]string
	tmpl                     *template.Template
}

func newWebhook(s map[string]string) (Sender, error) {
	if err := checkURL(s["url"]); err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for _, line := range strings.Split(s["headers"], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("invalid header line %q (want \"Name: value\")", line)
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	body := s["body_template"]
	if strings.TrimSpace(body) == "" {
		body = defaultWebhookTemplate
	}
	tmpl, err := template.New("body").Funcs(template.FuncMap{
		"json": func(v any) (string, error) {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(v); err != nil {
				return "", err
			}
			return strings.TrimSuffix(buf.String(), "\n"), nil
		},
		"upper":    strings.ToUpper,
		"lower":    strings.ToLower,
		"truncate": func(n int, s string) string { return model.Truncate(s, n) },
	}).Option("missingkey=zero").Parse(body)
	if err != nil {
		return nil, fmt.Errorf("body template: %w", err)
	}
	return &webhook{url: s["url"], method: s["method"], contentType: s["content_type"], headers: headers, tmpl: tmpl}, nil
}

// webhookData is the template context.
type webhookData struct {
	*Message
	Title   string
	Text    string
	Payload map[string]any
}

// StandardPayload is the JSON body sent by the generic webhook when no
// template is configured.
func StandardPayload(m *Message) map[string]any {
	return map[string]any{
		"title":      m.Title(),
		"text":       m.PlainText(),
		"link":       m.Link,
		"route":      m.RouteName,
		"suppressed": m.Suppressed,
		"test":       m.Test,
		"sent_at":    time.Now().UTC().Format(time.RFC3339),
		"event":      m.Event,
	}
}

func (n *webhook) Send(ctx context.Context, m *Message) error {
	var buf bytes.Buffer
	data := webhookData{Message: m, Title: m.Title(), Text: m.PlainText(), Payload: StandardPayload(m)}
	if err := n.tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("render body: %w", err)
	}
	return doRequest(ctx, n.method, n.url, n.contentType, buf.Bytes(), n.headers)
}
