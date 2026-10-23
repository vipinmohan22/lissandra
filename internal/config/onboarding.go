package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

// Change is one thing a setup step's defaults add, listed by the wizard
// before it applies them.
type Change struct {
	Kind string `json:"kind"` // source, category or route
	Name string `json:"name"`
}

// ErrNeedsChannel is the notify step's defaults without an enabled
// notifier to route alerts to.
var ErrNeedsChannel = errors.New("add an enabled alert channel first")

// ApplyStepDefaults adds Laya's defaults for a setup step to c and lists
// what it added (empty, not nil, when there is nothing to add). Defaults
// only add what is missing and never remove or edit anything, so applying
// them again changes nothing.
func ApplyStepDefaults(c *Config, step string) ([]Change, error) {
	changes := []Change{}
	switch step {
	case StepAccess, StepChannels:
		// The token is set through the auth API, and "no alerts for now"
		// is a decision, not a change.
	case StepSources:
		if slices.ContainsFunc(c.Sources, func(s Source) bool { return s.Enabled }) {
			break
		}
		src := Defaults().Sources[0]
		src.ID = model.NewID()
		src.Name = unusedSourceName(c.Sources, src.Name)
		c.Sources = append(c.Sources, src)
		changes = append(changes, Change{Kind: "source", Name: src.Name})
	case StepClassify:
		// Categories only: rules may have been edited or deleted on purpose.
		for _, cat := range DefaultCategories {
			if len(c.Settings.Categories) >= MaxCategories {
				break
			}
			if !slices.ContainsFunc(c.Settings.Categories, func(x Category) bool { return strings.ToLower(x.Name) == cat.Name }) {
				c.Settings.Categories = append(c.Settings.Categories, cat)
				changes = append(changes, Change{Kind: "category", Name: cat.Name})
			}
		}
	case StepNotify:
		var enabled []Notifier
		for _, n := range c.Notifiers {
			if n.Enabled {
				enabled = append(enabled, n)
			}
		}
		if len(enabled) == 0 {
			return nil, ErrNeedsChannel
		}
		grouped := groupChannels(c)
		for _, n := range enabled {
			// A team's channel stays scoped to the team's sources: a
			// catch-all route would send it every source's alerts.
			if grouped[n.ID] || slices.ContainsFunc(c.Routes, func(r Route) bool { return r.Enabled && slices.Contains(r.NotifierIDs, n.ID) }) {
				continue
			}
			r := DefaultRouteFor(n)
			c.Routes = append(c.Routes, r)
			changes = append(changes, Change{Kind: "route", Name: r.Name})
		}
	default:
		return nil, fmt.Errorf("unknown setup step %q", step)
	}
	return changes, nil
}

// groupChannels returns the notifiers enabled alert groups alert through,
// counting only groups with sources assigned: a group without sources gets
// no alerts.
func groupChannels(c *Config) map[string]bool {
	assigned := map[string]bool{}
	for _, src := range c.Sources {
		for _, id := range src.AlertGroupIDs {
			assigned[id] = true
		}
	}
	out := map[string]bool{}
	for _, g := range c.AlertGroups {
		if g.Enabled && assigned[g.ID] {
			for _, id := range g.NotifierIDs {
				out[id] = true
			}
		}
	}
	return out
}

// unusedSourceName returns name, or name-2, name-3 and so on, whichever no
// source has yet: names are unique because /ingest/{name} resolves by name.
func unusedSourceName(sources []Source, name string) string {
	taken := func(n string) bool {
		return slices.ContainsFunc(sources, func(s Source) bool { return s.Name == n })
	}
	candidate := name
	for i := 2; taken(candidate); i++ {
		candidate = name + "-" + strconv.Itoa(i)
	}
	return candidate
}

// DefaultRouteFor is the route the setup wizard suggests for a notifier
// nothing routes to yet.
func DefaultRouteFor(n Notifier) Route {
	sev := model.SeverityError
	if n.Type == "pagerduty" {
		sev = model.SeverityCritical // don't page on-call for every error
	}
	return Route{Name: fmt.Sprintf("%s: %s and above", n.Name, sev), Enabled: true, MinSeverity: sev,
		NotifierIDs: []string{n.ID}, ThrottleSeconds: 900}
}

// RecordStep sets how a setup step ended. It touches only that step, so
// records made at the same time by different admins don't undo each other.
func RecordStep(c *Config, step, outcome string, now time.Time) {
	if c.Onboarding == nil {
		c.Onboarding = &Onboarding{Status: OnboardingPending}
	}
	if c.Onboarding.Steps == nil {
		c.Onboarding.Steps = map[string]OnboardingStep{}
	}
	c.Onboarding.Steps[step] = OnboardingStep{Outcome: outcome, At: now}
}
