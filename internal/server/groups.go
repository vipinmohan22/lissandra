package server

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/learn"
	"github.com/vipinmohan22/lissandra/internal/model"
)

func (s *Server) groupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/alert-groups", s.listAlertGroups)
	mux.HandleFunc("POST /api/alert-groups", s.createAlertGroup)
	mux.HandleFunc("PUT /api/alert-groups/{id}", s.updateAlertGroup)
	mux.HandleFunc("DELETE /api/alert-groups/{id}", s.deleteAlertGroup)

	mux.HandleFunc("POST /api/rules/review", s.reviewRules)
}

// --- alert groups ---

func alertGroupID(g *config.AlertGroup) string { return g.ID }

// alertGroupView is an alert group with the sources assigned to it, which
// are stored on the sources (Source.AlertGroupIDs).
type alertGroupView struct {
	config.AlertGroup
	SourceIDs []string `json:"source_ids"`
}

func groupView(c *config.Config, g config.AlertGroup) alertGroupView {
	v := alertGroupView{AlertGroup: g, SourceIDs: []string{}}
	for _, src := range c.Sources {
		if slices.Contains(src.AlertGroupIDs, g.ID) {
			v.SourceIDs = append(v.SourceIDs, src.ID)
		}
	}
	return v
}

func (s *Server) listAlertGroups(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Get()
	out := make([]alertGroupView, len(c.AlertGroups))
	for i, g := range c.AlertGroups {
		out[i] = groupView(c, g)
	}
	writeJSON(w, http.StatusOK, out)
}

// applyGroup stores g and assigns it to exactly the sources in sourceIDs
// (nil leaves the assignment as it is).
func applyGroup(c *config.Config, g config.AlertGroup, sourceIDs []string) error {
	if len(g.Emails) > 0 && g.EmailNotifierID == "" {
		return fmt.Errorf("choose the email notifier the group's members are emailed through")
	}
	if g.EmailNotifierID != "" && indexOf(c.Notifiers, g.EmailNotifierID, notifierID) < 0 {
		return fmt.Errorf("email notifier %q not found", g.EmailNotifierID)
	}
	if i := indexOf(c.AlertGroups, g.ID, alertGroupID); i >= 0 {
		c.AlertGroups[i] = g
	} else {
		c.AlertGroups = append(c.AlertGroups, g)
	}
	if sourceIDs == nil {
		return nil
	}
	for _, id := range sourceIDs {
		if indexOf(c.Sources, id, sourceID) < 0 {
			return fmt.Errorf("source %q not found", id)
		}
	}
	for i := range c.Sources {
		src := &c.Sources[i]
		src.AlertGroupIDs = slices.DeleteFunc(src.AlertGroupIDs, func(id string) bool { return id == g.ID })
		if slices.Contains(sourceIDs, src.ID) {
			src.AlertGroupIDs = append(src.AlertGroupIDs, g.ID)
		}
	}
	return nil
}

func (s *Server) writeGroup(w http.ResponseWriter, status int, id string) {
	c := s.cfg.Get()
	if i := indexOf(c.AlertGroups, id, alertGroupID); i >= 0 {
		writeJSON(w, status, groupView(c, c.AlertGroups[i]))
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

func (s *Server) createAlertGroup(w http.ResponseWriter, r *http.Request) {
	var body alertGroupView
	if !readJSON(w, r, &body) {
		return
	}
	g := body.AlertGroup
	g.ID = model.NewID()
	if s.update(w, func(c *config.Config) error { return applyGroup(c, g, body.SourceIDs) }) {
		s.writeGroup(w, http.StatusCreated, g.ID)
	}
}

func (s *Server) updateAlertGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body alertGroupView
	if !readJSON(w, r, &body) {
		return
	}
	g := body.AlertGroup
	g.ID = id
	if s.update(w, func(c *config.Config) error {
		if indexOf(c.AlertGroups, id, alertGroupID) < 0 {
			return errNotFound
		}
		return applyGroup(c, g, body.SourceIDs)
	}) {
		s.writeGroup(w, http.StatusOK, id)
	}
}

func (s *Server) deleteAlertGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.update(w, func(c *config.Config) error {
		i := indexOf(c.AlertGroups, id, alertGroupID)
		if i < 0 {
			return errNotFound
		}
		// Normalize drops the sources' references to it.
		c.AlertGroups = slices.Delete(c.AlertGroups, i, i+1)
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- rule review ---

// reviewRules approves or rejects learned rules. An approved rule becomes
// an ordinary rule (approving a rejected one restores what was learned). A
// rejected one stays, without a category or
// severity, and keeps matching events from the Laya engine, so its answer
// is neither applied nor learned again; delete it to let Lissandra learn the
// pattern anew.
func (s *Server) reviewRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string   `json:"action"` // approve or reject
		IDs    []string `json:"ids"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Action != "approve" && body.Action != "reject" {
		writeError(w, http.StatusBadRequest, `action must be "approve" or "reject"`)
		return
	}
	if s.update(w, func(c *config.Config) error {
		for _, id := range body.IDs {
			i := indexOf(c.Rules, id, ruleID)
			if i < 0 {
				return fmt.Errorf("rule %q: %w", id, errNotFound)
			}
			rule := &c.Rules[i]
			if body.Action == "approve" {
				if rule.Status == config.RuleRejected && rule.Learned != nil {
					// Undoes a rejection: what was learned applies again.
					rule.Category, rule.SkipEngine = rule.Learned.Category, false
					if rule.Learned.Severity != "" {
						rule.Severity, rule.SeverityMode = rule.Learned.Severity, config.SeverityAtLeast
					}
				}
				rule.Status = config.RuleActive
				continue
			}
			rule.Status, rule.Enabled, rule.SkipEngine = config.RuleRejected, true, true
			rule.Category, rule.Severity, rule.SeverityMode = "", "", ""
		}
		return nil
	}) {
		writeJSON(w, http.StatusOK, s.cfg.Get().Rules)
	}
}

// learningStats reports rule learning for /api/stats.
func (s *Server) learningStats() learn.Stats {
	if s.learner != nil {
		return s.learner.Stats()
	}
	c := s.cfg.Get()
	st := learn.Stats{Enabled: c.Settings.Engine.LearnRules, MaxReview: config.MaxReviewRules}
	for _, r := range c.Rules {
		if r.Status == config.RuleReview {
			st.Review++
		}
	}
	return st
}
