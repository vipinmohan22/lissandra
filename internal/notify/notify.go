// Package notify delivers classified events to external channels (email,
// Slack, Microsoft Teams, Discord, Google Chat, PagerDuty, generic
// webhooks), and decides per channel which go right away and which wait
// for the channel's digest. Channel types are plugins: each registers a
// field schema that the UI renders as a form, and a constructor that builds
// a Sender from the saved settings.
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

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// Message kinds.
const (
	KindAlert  = "alert"  // one event, right away
	KindDigest = "digest" // a channel's batched events
)

// Message is what a Sender delivers: one classified event plus routing
// context (an alert), or a channel's batched events (a digest).
type Message struct {
	ID         string      // delivery ID; for digests also Digest.ID
	Kind       string      // KindAlert ("" counts as one) or KindDigest
	Event      model.Event // digests: digestEvent(Digest), for webhook templates
	Digest     *Digest
	RouteName  string // Routes joined ", "
	Routes     []string
	Reason     Reason
	Suppressed int       // repeats since the previous alert, reported with this one
	Since      time.Time // the previous alert on this channel for this problem
	// SinceDigest is the digest that reported the earlier repeats of the
	// alert at Since: Suppressed counts from it (zero: none did).
	SinceDigest time.Time
	LastRepeat  time.Time // the newest of the Suppressed repeats (zero: not known)
	Link        string    // link to the event (or digest) in the Lissandra UI, when a public URL is set
	Test        bool

	// For the completion hook (dispatcher.completed) and texts.
	at        time.Time // when it was decided: "today" for clock texts
	nid, fp   string
	seq       uint64
	prevSeq   uint64   // the alert state's Seq before this alert: a failure restores it
	prev      *Message // the alert before, when it was still in flight: if it failed too, a failure restores its window
	failed    bool     // delivered and failed retryably (amu)
	prevSent  time.Time
	prevSev   model.Severity
	prevWhy   string        // the previous alert's reason code
	digestMin int           // alerts: the interval undelivered events are batched at; 0 = counted instead
	buf       *digestBuffer // digests: the buffer it was made from, for a merge back
	// summaries: the windows it opened for its held alerts, by alert state
	// key; if it isn't delivered they close again
	opened map[string]openedWindow
}

// openedWindow is an alert window a summary opened: the state's Seq it
// set, and what the fields it set held before.
type openedWindow struct {
	seq  uint64
	prev alertState
}

// IsDigest reports whether m carries a digest rather than one event.
func (m *Message) IsDigest() bool { return m.Digest != nil }

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
	// NoDigest is set by channels that must only get right-away alerts
	// (PagerDuty): smart and digest routes never batch for them.
	NoDigest bool `json:"no_digest,omitempty"`
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
	req.Header.Set("User-Agent", "lissandra-notifier/1")
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

// permanentError is a failed send that retrying can't fix.
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// Permanent marks err as not worth retrying (a bad template, a message the
// channel never takes). Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// ErrNoDigest is returned for a digest handed to a channel that only takes
// right-away alerts.
var ErrNoDigest = errors.New("this notifier never receives digests")

// retryable reports whether a failed send is worth retrying.
func retryable(err error) bool {
	var pe permanentError
	if errors.As(err, &pe) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusTooManyRequests || he.Status >= 500
	}
	return !errors.Is(err, context.Canceled)
}
