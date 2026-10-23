// Package notify delivers classified events to external channels (email,
// Slack, Microsoft Teams, Discord, Google Chat, PagerDuty, generic
// webhooks). Channel types are plugins: each registers a field schema that
// the UI renders as a form, and a constructor that builds a Sender from the
// saved settings.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
)

// Message is what a Sender delivers: one classified event plus routing
// context.
type Message struct {
	Event      model.Event
	RouteName  string
	Suppressed int    // similar events throttled since the previous notification
	Link       string // link to the event in the Laya UI, when a public URL is set
	Test       bool
}

// Sender delivers messages to one configured channel.
type Sender interface {
	Send(ctx context.Context, m *Message) error
}

// Field describes one setting of a notifier type, for UI rendering and
// validation.
type Field struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // text, password, textarea, select, number, checkbox
	Required    bool     `json:"required,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Options     []string `json:"options,omitempty"`
	Default     string   `json:"default,omitempty"`
	Recipient   bool     `json:"recipient,omitempty"` // who receives alerts; the setup wizard edits these inline
}

// Plugin is a notifier type.
type Plugin struct {
	Type        string  `json:"type"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	Audience    string  `json:"audience,omitempty"` // one line: who receives this channel's alerts
	Fields      []Field `json:"fields"`
	// New builds a Sender. settings have defaults applied and ${env:...}
	// references expanded.
	New func(settings map[string]string) (Sender, error) `json:"-"`
}

var (
	regMu    sync.RWMutex
	registry = map[string]*Plugin{}
)

// Register adds a notifier type. Call it from an init function.
func Register(p *Plugin) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[p.Type]; dup {
		panic("notify: duplicate plugin " + p.Type)
	}
	registry[p.Type] = p
}

// Plugins returns every registered notifier type, sorted by label.
func Plugins() []*Plugin {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]*Plugin, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// PluginFor returns the plugin registered for typ.
func PluginFor(typ string) (*Plugin, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := registry[typ]
	return p, ok
}

// resolve applies defaults and expands env references.
func resolve(p *Plugin, settings map[string]string) map[string]string {
	out := make(map[string]string, len(p.Fields))
	for _, f := range p.Fields {
		v := strings.TrimSpace(config.ExpandEnv(settings[f.Key]))
		if v == "" {
			v = f.Default
		}
		out[f.Key] = v
	}
	return out
}

// Build constructs the Sender for a saved notifier.
func Build(n config.Notifier) (Sender, error) {
	p, ok := PluginFor(n.Type)
	if !ok {
		return nil, fmt.Errorf("unknown notifier type %q", n.Type)
	}
	return p.New(resolve(p, n.Settings))
}

// Validate checks a notifier's settings against its plugin schema. Settings
// that reference environment variables are only checked for presence, since
// the variable may not be set where validation runs.
func Validate(n config.Notifier) error {
	p, ok := PluginFor(n.Type)
	if !ok {
		return fmt.Errorf("unknown notifier type %q", n.Type)
	}
	usesEnv := false
	for _, f := range p.Fields {
		v := strings.TrimSpace(n.Settings[f.Key])
		if f.Required && v == "" && f.Default == "" {
			return fmt.Errorf("%s is required", f.Label)
		}
		if config.HasRef(v) {
			usesEnv = true
		}
		if f.Type == "select" && v != "" && len(f.Options) > 0 {
			found := false
			for _, o := range f.Options {
				found = found || o == v
			}
			if !found {
				return fmt.Errorf("%s must be one of %s", f.Label, strings.Join(f.Options, ", "))
			}
		}
	}
	if usesEnv {
		return nil
	}
	_, err := p.New(resolve(p, n.Settings))
	return err
}

// Mask hides secret settings for API responses.
func Mask(n config.Notifier) config.Notifier {
	p, ok := PluginFor(n.Type)
	if !ok {
		return n
	}
	s := make(map[string]string, len(n.Settings))
	for k, v := range n.Settings {
		s[k] = v
	}
	for _, f := range p.Fields {
		if f.Secret {
			s[f.Key] = config.MaskValue(s[f.Key])
		}
	}
	n.Settings = s
	return n
}

// Restore puts back secret settings a client sent as the mask.
func Restore(n *config.Notifier, old config.Notifier) {
	if n.Type != old.Type {
		return
	}
	for k, v := range n.Settings {
		n.Settings[k] = config.Restore(v, old.Settings[k])
	}
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// postJSON sends payload and treats any 2xx response as success.
func postJSON(ctx context.Context, url string, payload any, headers map[string]string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return doRequest(ctx, http.MethodPost, url, "application/json", body, headers)
}

func doRequest(ctx context.Context, method, url, contentType string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return hideURL(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "laya-notifier/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return hideURL(err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	return nil
}

// hideURL shows only the scheme and host of the URL in an HTTP client
// error: a webhook URL's path is often its secret (Slack, Teams), and Go
// hides only a password.
func hideURL(err error) error {
	var ue *neturl.Error
	if errors.As(err, &ue) {
		if u, perr := neturl.Parse(ue.URL); perr == nil && u.Host != "" {
			ue.URL = u.Scheme + "://" + u.Host + "/…"
		} else {
			ue.URL = "(the notifier's URL)"
		}
	}
	return err
}

// HTTPError is a non-2xx response from a notification endpoint.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// retryable reports whether a failed send is worth retrying.
func retryable(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusTooManyRequests || he.Status >= 500
	}
	return !errors.Is(err, context.Canceled)
}
