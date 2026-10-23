package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/vipinmohan22/layalogs/internal/auth"
	"github.com/vipinmohan22/layalogs/internal/config"
)

// The setup wizard's API: the admin token (/api/auth) and the wizard's
// progress (/api/onboarding). Like the rest of /api/, it needs the admin
// token, which on a first start is the one Laya generated and logged.
func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth", s.getAuth)
	mux.HandleFunc("PUT /api/auth/token", s.putAuthToken)
	mux.HandleFunc("DELETE /api/auth/token", s.deleteAuthToken)

	mux.HandleFunc("GET /api/onboarding", s.getOnboarding)
	mux.HandleFunc("POST /api/onboarding/start", s.startOnboarding)
	mux.HandleFunc("POST /api/onboarding/steps/{step}", s.recordStep)
	mux.HandleFunc("POST /api/onboarding/defaults", s.onboardingDefaults)
	mux.HandleFunc("POST /api/onboarding/defer", s.deferOnboarding)
	mux.HandleFunc("POST /api/onboarding/complete", s.completeOnboarding)
	mux.HandleFunc("POST /api/onboarding/dismiss", s.dismissOnboarding)
}

// errUnchanged ends an update that would change nothing, so repeating a
// request neither saves the config nor reconfigures Laya.
var errUnchanged = errors.New("unchanged")

var (
	errNotRunning  = &apiError{http.StatusConflict, "setup_not_running", "setup is not running (it was finished, skipped or never started)"}
	errTokenNotSet = &apiError{http.StatusConflict, "token_not_set", "set a permanent admin token first: the one in use was generated for this run"}
)

// --- admin token ---

// authView is the admin token's status plus what the UI needs for its hints.
type authView struct {
	auth.Status
	OnNomad bool `json:"on_nomad"`
}

func (s *Server) getAuth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, authView{s.auth.Status(), s.opts.OnNomad})
}

// putAuthToken sets the admin token to one the browser generated and showed
// the user. The request's token must be the one in use, so of two changes
// made with the same token only the first succeeds.
func (s *Server) putAuthToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !readStrictJSON(w, r, &body) {
		return
	}
	st, err := s.auth.Rotate(bearer(r), body.Token)
	switch {
	case err == nil:
		s.log.Warn("admin token changed in Laya", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, authView{st, s.opts.OnNomad})
	case errors.Is(err, auth.ErrWeakToken):
		writeErrorCode(w, http.StatusBadRequest, "weak_token", err.Error())
	case errors.Is(err, auth.ErrSameToken):
		writeErrorCode(w, http.StatusBadRequest, "same_token", err.Error())
	case errors.Is(err, auth.ErrLoggedToken):
		writeErrorCode(w, http.StatusBadRequest, "logged_token", err.Error())
	case errors.Is(err, auth.ErrStale):
		writeErrorCode(w, http.StatusUnauthorized, "stale_token", err.Error())
	case errors.Is(err, auth.ErrEnvManaged):
		writeErrorCode(w, http.StatusConflict, "env_managed", err.Error())
	default:
		s.log.Error("saving the admin token failed", "err", err)
		writeErrorCode(w, http.StatusInternalServerError, "save_failed", "saving the admin token failed; see Laya's log")
	}
}

// deleteAuthToken removes a stored token that LAYA_ADMIN_TOKEN overrides.
func (s *Server) deleteAuthToken(w http.ResponseWriter, r *http.Request) {
	switch err := s.auth.DeleteStored(); {
	case err == nil:
		s.log.Info("stored admin token deleted", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrNotEnv):
		writeErrorCode(w, http.StatusConflict, "not_env_managed", err.Error())
	case errors.Is(err, auth.ErrNoStored):
		writeErrorCode(w, http.StatusNotFound, "no_stored_token", err.Error())
	default:
		s.log.Error("deleting the stored admin token failed", "err", err)
		writeErrorCode(w, http.StatusInternalServerError, "save_failed", "deleting the stored admin token failed; see Laya's log")
	}
}

// --- setup progress ---

// Setup pages that aren't steps.
const (
	pageWelcome = "welcome"
	pageReview  = "review"
)

// onboardingView is the setup state as the wizard reads it: what is
// stored, plus what follows from the config and the admin token (where to
// resume, what is left), which is never stored.
type onboardingView struct {
	Status     string                           `json:"status"` // "none" when never offered
	Steps      map[string]config.OnboardingStep `json:"steps"`
	Order      []string                         `json:"order"`
	Resume     string                           `json:"resume"`
	Remaining  []string                         `json:"remaining"`
	NotNeeded  []string                         `json:"not_needed"`
	StartedAt  *time.Time                       `json:"started_at"`
	FinishedAt *time.Time                       `json:"finished_at"`
	Auth       auth.Status                      `json:"auth"`
}

func (s *Server) onboardingView(c *config.Config) onboardingView {
	v := onboardingView{
		Status: "none", Steps: map[string]config.OnboardingStep{}, Order: slices.Clone(config.OnboardingSteps),
		Remaining: []string{}, NotNeeded: []string{}, Auth: s.auth.Status(),
	}
	if o := c.Onboarding; o != nil {
		v.Status, v.StartedAt, v.FinishedAt = o.Status, o.StartedAt, o.FinishedAt
		maps.Copy(v.Steps, o.Steps)
	}
	// Without a channel there is nobody to notify.
	if !hasEnabledNotifier(c) {
		v.NotNeeded = append(v.NotNeeded, config.StepNotify)
	}
	for _, step := range v.Order {
		if slices.Contains(v.NotNeeded, step) {
			continue
		}
		// A permanent token leaves nothing to do on the access step.
		needed := step != config.StepAccess || v.Auth.Source == auth.SourceGenerated
		rec, ok := v.Steps[step]
		if !ok && needed && v.Resume == "" {
			v.Resume = step
		}
		if (!ok || rec.Outcome == config.OutcomeSkipped) && needed {
			v.Remaining = append(v.Remaining, step)
		}
	}
	switch {
	case len(v.Steps) == 0:
		v.Resume = pageWelcome
	case v.Resume == "":
		v.Resume = pageReview
	}
	return v
}

func hasEnabledNotifier(c *config.Config) bool {
	return slices.ContainsFunc(c.Notifiers, func(n config.Notifier) bool { return n.Enabled })
}

// checkRunning refuses to change a setup that another session finished or
// skipped, or that was never offered.
func checkRunning(c *config.Config) error {
	if o := c.Onboarding; o != nil && (o.Status == config.OnboardingPending || o.Status == config.OnboardingDeferred) {
		return nil
	}
	return errNotRunning
}

// setupNow is the time recorded in setup state: seconds are plenty.
func setupNow() time.Time { return time.Now().UTC().Truncate(time.Second) }

func (s *Server) getOnboarding(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.onboardingView(s.cfg.Get()))
}

// changeOnboarding runs a setup status change and answers with the view.
func (s *Server) changeOnboarding(w http.ResponseWriter, fn func(c *config.Config, now time.Time) error) {
	now := setupNow()
	if s.update(w, func(c *config.Config) error { return fn(c, now) }) {
		writeJSON(w, http.StatusOK, s.onboardingView(s.cfg.Get()))
	}
}

// startOnboarding starts setup, or resumes it when it is still running.
func (s *Server) startOnboarding(w http.ResponseWriter, r *http.Request) {
	s.changeOnboarding(w, func(c *config.Config, now time.Time) error {
		switch o := c.Onboarding; {
		case o == nil || o.Status == config.OnboardingCompleted || o.Status == config.OnboardingDismissed:
			c.Onboarding = &config.Onboarding{Status: config.OnboardingPending, StartedAt: &now}
		case o.Status == config.OnboardingPending:
			return errUnchanged
		default:
			o.Status = config.OnboardingPending
		}
		return nil
	})
}

// deferOnboarding is "Finish later": the UI stops opening setup by itself
// and shows a banner instead.
func (s *Server) deferOnboarding(w http.ResponseWriter, r *http.Request) {
	s.changeOnboarding(w, func(c *config.Config, now time.Time) error {
		if err := checkRunning(c); err != nil {
			return err
		}
		if c.Onboarding.Status == config.OnboardingDeferred {
			return errUnchanged
		}
		c.Onboarding.Status = config.OnboardingDeferred
		return nil
	})
}

func (s *Server) completeOnboarding(w http.ResponseWriter, r *http.Request) {
	s.finishOnboarding(w, config.OnboardingCompleted)
}

func (s *Server) dismissOnboarding(w http.ResponseWriter, r *http.Request) {
	s.finishOnboarding(w, config.OnboardingDismissed)
}

// finishOnboarding ends setup as completed (steps may be left: the user's
// choice) or dismissed. Ending it the same way twice is a no-op.
func (s *Server) finishOnboarding(w http.ResponseWriter, status string) {
	s.changeOnboarding(w, func(c *config.Config, now time.Time) error {
		if c.Onboarding != nil && c.Onboarding.Status == status {
			return errUnchanged
		}
		if err := checkRunning(c); err != nil {
			return err
		}
		c.Onboarding.Status, c.Onboarding.FinishedAt = status, &now
		return nil
	})
}

// recordStep records a step's outcome. With "defaults" it first applies the
// step's defaults, in the same update, so two admins clicking at once can't
// add a source or route twice. With dry_run it only lists what the
// defaults would add, for the button's label.
func (s *Server) recordStep(w http.ResponseWriter, r *http.Request) {
	step := r.PathValue("step")
	if !slices.Contains(config.OnboardingSteps, step) {
		writeErrorCode(w, http.StatusNotFound, "unknown_step", fmt.Sprintf("unknown setup step %q", step))
		return
	}
	var body struct {
		Outcome string `json:"outcome"`
		DryRun  bool   `json:"dry_run"`
	}
	if !readStrictJSON(w, r, &body) {
		return
	}
	switch {
	case !config.ValidOutcome(body.Outcome):
		writeErrorCode(w, http.StatusBadRequest, "invalid_outcome", `outcome must be "done", "defaults" or "skipped"`)
		return
	case body.DryRun && body.Outcome != config.OutcomeDefaults:
		writeErrorCode(w, http.StatusBadRequest, "invalid_outcome", `dry_run is only for the "defaults" outcome`)
		return
	case body.DryRun:
		s.dryRun(w, func(c *config.Config) ([]config.Change, error) { return stepDefaults(c, step) })
		return
	}
	now := setupNow()
	changes := []config.Change{}
	if !s.update(w, func(c *config.Config) error {
		if err := checkRunning(c); err != nil {
			return err
		}
		if step == config.StepAccess && body.Outcome != config.OutcomeSkipped && s.auth.Status().Source == auth.SourceGenerated {
			return errTokenNotSet
		}
		if body.Outcome == config.OutcomeDefaults {
			var err error
			if changes, err = stepDefaults(c, step); err != nil {
				return err
			}
		}
		if prev, ok := c.Onboarding.Steps[step]; ok && prev.Outcome == body.Outcome && len(changes) == 0 {
			return errUnchanged
		}
		config.RecordStep(c, step, body.Outcome, now)
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": s.onboardingView(s.cfg.Get()), "changes": changes})
}

// onboardingDefaults is "Use defaults for everything". The body is
// optional: without one the defaults are applied.
func (s *Server) onboardingDefaults(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun bool `json:"dry_run"`
	}
	// A chunked request has no length (-1), so look at the body itself.
	br := bufio.NewReader(r.Body)
	if _, err := br.Peek(1); err == nil {
		r.Body = io.NopCloser(br)
		if !readStrictJSON(w, r, &body) {
			return
		}
	}
	now := setupNow()
	if body.DryRun {
		s.dryRun(w, func(c *config.Config) ([]config.Change, error) {
			if err := checkRunning(c); err != nil {
				return nil, err
			}
			changes, _, err := allDefaults(c, s.auth.Status().Source, now)
			return changes, err
		})
		return
	}
	var changes []config.Change
	if !s.update(w, func(c *config.Config) error {
		if err := checkRunning(c); err != nil {
			return err
		}
		var changed bool
		var err error
		if changes, changed, err = allDefaults(c, s.auth.Status().Source, now); err == nil && !changed {
			return errUnchanged
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": s.onboardingView(s.cfg.Get()), "changes": changes})
}

// allDefaults gives every step not yet done its defaults and reports
// whether that changed anything. The admin token never changes implicitly,
// so a generated one leaves access skipped; without a channel, notify is
// left alone.
func allDefaults(c *config.Config, source auth.Source, now time.Time) ([]config.Change, bool, error) {
	changes := []config.Change{}
	changed := false
	for _, step := range config.OnboardingSteps {
		prev, ok := c.Onboarding.Steps[step]
		if ok && prev.Outcome != config.OutcomeSkipped {
			continue
		}
		outcome := config.OutcomeDefaults
		switch step {
		case config.StepAccess:
			outcome = config.OutcomeDone
			if source == auth.SourceGenerated {
				outcome = config.OutcomeSkipped
			}
		case config.StepNotify:
			if !hasEnabledNotifier(c) {
				continue
			}
		}
		n := len(changes)
		if outcome == config.OutcomeDefaults {
			ch, err := stepDefaults(c, step)
			if err != nil {
				return nil, false, err
			}
			changes = append(changes, ch...)
		}
		if len(changes) > n || prev.Outcome != outcome {
			config.RecordStep(c, step, outcome, now)
			changed = true
		}
	}
	return changes, changed, nil
}

// stepDefaults is config.ApplyStepDefaults with its error as the API's.
func stepDefaults(c *config.Config, step string) ([]config.Change, error) {
	changes, err := config.ApplyStepDefaults(c, step)
	if errors.Is(err, config.ErrNeedsChannel) {
		return nil, &apiError{http.StatusBadRequest, "needs_channel", err.Error()}
	}
	return changes, err
}

// dryRun answers with what apply would change on a copy of the config,
// validated as a save would be. Nothing is saved.
func (s *Server) dryRun(w http.ResponseWriter, apply func(c *config.Config) ([]config.Change, error)) {
	c := s.cfg.Get()
	changes, err := apply(c)
	if err == nil {
		err = config.Normalize(c)
	}
	var ae *apiError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
	case errors.As(err, &ae):
		writeErrorCode(w, ae.status, ae.code, ae.msg)
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
