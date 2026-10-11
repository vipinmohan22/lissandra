package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/vipinmohan22/layalogs/internal/classify"
	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/events"
	"github.com/vipinmohan22/layalogs/internal/ingest"
	"github.com/vipinmohan22/layalogs/internal/model"
	"github.com/vipinmohan22/layalogs/internal/notify"
	"github.com/vipinmohan22/layalogs/internal/poller"
)

var errNotFound = errors.New("not found")

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/info", s.getInfo)
	mux.HandleFunc("GET /api/stats", s.getStats)

	mux.HandleFunc("GET /api/events", s.listEvents)
	mux.HandleFunc("GET /api/events/{id}", s.getEvent)

	mux.HandleFunc("GET /api/sources", s.listSources)
	mux.HandleFunc("POST /api/sources", s.createSource)
	mux.HandleFunc("POST /api/sources/test", s.testSource)
	mux.HandleFunc("PUT /api/sources/{id}", s.updateSource)
	mux.HandleFunc("DELETE /api/sources/{id}", s.deleteSource)
	mux.HandleFunc("POST /api/sources/{id}/rotate-token", s.rotateToken)

	mux.HandleFunc("GET /api/rules", s.listRules)
	mux.HandleFunc("POST /api/rules", s.createRule)
	mux.HandleFunc("POST /api/rules/test", s.testRules)
	mux.HandleFunc("POST /api/rules/reorder", s.reorderRules)
	mux.HandleFunc("PUT /api/rules/{id}", s.updateRule)
	mux.HandleFunc("DELETE /api/rules/{id}", s.deleteRule)

	mux.HandleFunc("GET /api/notifier-types", s.listNotifierTypes)
	mux.HandleFunc("GET /api/notifiers", s.listNotifiers)
	mux.HandleFunc("POST /api/notifiers", s.createNotifier)
	mux.HandleFunc("POST /api/notifiers/test", s.testNotifier)
	mux.HandleFunc("PUT /api/notifiers/{id}", s.updateNotifier)
	mux.HandleFunc("DELETE /api/notifiers/{id}", s.deleteNotifier)

	mux.HandleFunc("GET /api/routes", s.listRoutes)
	mux.HandleFunc("POST /api/routes", s.createRoute)
	mux.HandleFunc("PUT /api/routes/{id}", s.updateRoute)
	mux.HandleFunc("DELETE /api/routes/{id}", s.deleteRoute)

	mux.HandleFunc("GET /api/deliveries", s.listDeliveries)

	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.updateSettings)
	mux.HandleFunc("POST /api/ai/test", s.testAI)

	mux.HandleFunc("GET /api/config/export", s.exportConfig)
	mux.HandleFunc("POST /api/config/import", s.importConfig)

	s.importRoutes(mux)
}

// update runs a config mutation and maps errors to HTTP responses.
func (s *Server) update(w http.ResponseWriter, fn func(c *config.Config) error) bool {
	err := s.cfg.Update(fn)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
	return false
}

func indexOf[T any](items []T, id string, idOf func(*T) string) int {
	for i := range items {
		if idOf(&items[i]) == id {
			return i
		}
	}
	return -1
}

// --- info & stats ---

func (s *Server) getInfo(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    s.opts.Version,
		"public_url": s.publicURL(r, c),
		"started_at": s.started.UTC(),
		"severities": model.Severities,
		"operators":  config.Operators,
		"categories": c.Settings.Categories,
		"imports": map[string]any{
			"max_upload_bytes":       s.opts.MaxImportBytes,
			"max_decompressed_bytes": s.opts.MaxImportDecompressed,
			"concurrency":            s.imports.Config().Concurrency,
			"keep":                   s.imports.Config().Keep,
			"formats":                ImportFormats,
		},
	})
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	p, d := s.pipeline, s.dispatcher
	writeJSON(w, http.StatusOK, map[string]any{
		"events": s.store.Stats(time.Now()),
		"pipeline": map[string]any{
			"queue_depth": p.QueueDepth(), "received": p.Received.Load(), "processed": p.Processed.Load(),
			"dropped": p.Dropped.Load(), "rejected": p.Rejected.Load(), "ai_queued": p.AIQueued.Load(),
		},
		"notifications": map[string]any{
			"queue_depth": d.QueueDepth(), "sent": d.Sent.Load(), "failed": d.Failed.Load(),
			"dropped": d.Dropped.Load(), "throttled": d.Throttled.Load(),
		},
		"ai":             s.ai.Status(),
		"imports":        map[string]any{"active": s.imports.Active(), "done": s.imports.Succeeded.Load(), "failed": s.imports.Failed.Load()},
		"sources":        s.pollers.Status(),
		"uptime_seconds": int(time.Since(s.started).Seconds()),
	})
}

// --- events ---

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	writeJSON(w, http.StatusOK, s.store.Find(events.Query{
		MinSeverity: model.Severity(q.Get("min_severity")),
		Category:    q.Get("category"),
		SourceID:    q.Get("source"),
		Tag:         q.Get("tag"),
		Fingerprint: q.Get("fingerprint"),
		Text:        q.Get("q"),
		Before:      q.Get("before"),
		Limit:       limit,
	}))
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	ev, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "event not found (it may have been rotated out of memory)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev, "routes": s.dispatcher.Matches(&ev)})
}

// --- sources ---

func sourceID(s *config.Source) string { return s.ID }

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Get()
	out := make([]config.Source, len(c.Sources))
	for i, src := range c.Sources {
		out[i] = config.MaskSource(src)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	var src config.Source
	if !readJSON(w, r, &src) {
		return
	}
	src.ID = model.NewID()
	src.IngestToken = ""
	src.CreatedAt, src.UpdatedAt = time.Time{}, time.Time{}
	if !s.update(w, func(c *config.Config) error {
		c.Sources = append(c.Sources, src)
		return nil
	}) {
		return
	}
	s.writeSource(w, http.StatusCreated, src.ID)
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var src config.Source
	if !readJSON(w, r, &src) {
		return
	}
	if !s.update(w, func(c *config.Config) error {
		i := indexOf(c.Sources, id, sourceID)
		if i < 0 {
			return errNotFound
		}
		old := c.Sources[i]
		config.RestoreSource(&src, old)
		src.ID, src.CreatedAt, src.UpdatedAt = id, old.CreatedAt, time.Now().UTC()
		src.IngestToken = old.IngestToken
		c.Sources[i] = src
		return nil
	}) {
		return
	}
	s.writeSource(w, http.StatusOK, id)
}

func (s *Server) writeSource(w http.ResponseWriter, status int, id string) {
	c := s.cfg.Get()
	if i := indexOf(c.Sources, id, sourceID); i >= 0 {
		writeJSON(w, status, config.MaskSource(c.Sources[i]))
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.update(w, func(c *config.Config) error {
		i := indexOf(c.Sources, id, sourceID)
		if i < 0 {
			return errNotFound
		}
		c.Sources = slices.Delete(c.Sources, i, i+1)
		for j := range c.Rules {
			c.Rules[j].SourceIDs = slices.DeleteFunc(c.Rules[j].SourceIDs, func(v string) bool { return v == id })
		}
		for j := range c.Routes {
			c.Routes[j].SourceIDs = slices.DeleteFunc(c.Routes[j].SourceIDs, func(v string) bool { return v == id })
		}
		return nil
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.update(w, func(c *config.Config) error {
		i := indexOf(c.Sources, id, sourceID)
		if i < 0 {
			return errNotFound
		}
		if c.Sources[i].Type != config.SourcePush {
			return fmt.Errorf("only push sources have ingest tokens")
		}
		c.Sources[i].IngestToken = model.NewToken()
		return nil
	}) {
		return
	}
	s.writeSource(w, http.StatusOK, id)
}

// previewEvent is a classified sample returned by the test endpoints.
type previewEvent struct {
	Event   model.Event `json:"event"`
	Dropped bool        `json:"dropped"`
	Routes  []string    `json:"routes"`
}

func (s *Server) preview(evs []model.Event, engine *classify.Engine) []previewEvent {
	out := make([]previewEvent, 0, min(len(evs), 50))
	for _, ev := range evs {
		if len(out) == 50 {
			break
		}
		kept := engine.Classify(&ev)
		pe := previewEvent{Event: ev, Dropped: !kept, Routes: []string{}}
		if kept {
			if r := s.dispatcher.Matches(&ev); r != nil {
				pe.Routes = r
			}
		}
		out = append(out, pe)
	}
	return out
}

// testSource performs one fetch for a (possibly unsaved) pull source and
// returns classified samples without submitting them.
func (s *Server) testSource(w http.ResponseWriter, r *http.Request) {
	var src config.Source
	if !readJSON(w, r, &src) {
		return
	}
	if src.Type != config.SourcePull {
		writeError(w, http.StatusBadRequest, "only pull sources can be test-fetched; send a sample to the ingest URL instead")
		return
	}
	if src.ID != "" {
		c := s.cfg.Get()
		if i := indexOf(c.Sources, src.ID, sourceID); i >= 0 {
			config.RestoreSource(&src, c.Sources[i])
		}
	}
	if src.ID == "" {
		src.ID = "test"
	}
	draft := config.Config{Sources: []config.Source{src}}
	if err := config.Normalize(&draft); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	src = draft.Sources[0]
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(src.Pull.TimeoutSeconds+5)*time.Second)
	defer cancel()
	now := time.Now()
	evs, err := poller.Fetch(ctx, &src, now.Add(-time.Duration(src.Pull.IntervalSeconds)*time.Second), now, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "fetch failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(evs), "events": s.preview(evs, s.pipeline.Engine())})
}

// --- rules ---

func ruleID(r *config.Rule) string { return r.ID }

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.Get().Rules)
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	var rule config.Rule
	if !readJSON(w, r, &rule) {
		return
	}
	rule.ID = model.NewID()
	if s.update(w, func(c *config.Config) error {
		c.Rules = append(c.Rules, rule)
		return nil
	}) {
		writeJSON(w, http.StatusCreated, s.findRule(rule.ID))
	}
}

func (s *Server) updateRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var rule config.Rule
	if !readJSON(w, r, &rule) {
		return
	}
	rule.ID = id
	if s.update(w, func(c *config.Config) error {
		i := indexOf(c.Rules, id, ruleID)
		if i < 0 {
			return errNotFound
		}
		c.Rules[i] = rule
		return nil
	}) {
		writeJSON(w, http.StatusOK, s.findRule(id))
	}
}

func (s *Server) findRule(id string) *config.Rule {
	c := s.cfg.Get()
	if i := indexOf(c.Rules, id, ruleID); i >= 0 {
		return &c.Rules[i]
	}
	return nil
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.update(w, func(c *config.Config) error {
		i := indexOf(c.Rules, id, ruleID)
		if i < 0 {
			return errNotFound
		}
		c.Rules = slices.Delete(c.Rules, i, i+1)
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) reorderRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if s.update(w, func(c *config.Config) error {
		if len(body.IDs) != len(c.Rules) {
			return fmt.Errorf("ids must list every rule exactly once")
		}
		out := make([]config.Rule, 0, len(c.Rules))
		for _, id := range body.IDs {
			i := indexOf(c.Rules, id, ruleID)
			if i < 0 || slices.ContainsFunc(out, func(r config.Rule) bool { return r.ID == id }) {
				return fmt.Errorf("ids must list every rule exactly once")
			}
			out = append(out, c.Rules[i])
		}
		c.Rules = out
		return nil
	}) {
		writeJSON(w, http.StatusOK, s.cfg.Get().Rules)
	}
}

// testRules parses a sample payload and classifies it, optionally against
// a draft rule set, without storing anything.
func (s *Server) testRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sample      string         `json:"sample"`
		ContentType string         `json:"content_type"`
		SourceID    string         `json:"source_id"`
		Rules       *[]config.Rule `json:"rules"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	c := s.cfg.Get()
	src := config.Source{ID: "test", Name: "test", Type: config.SourcePush, Parse: config.Parse{Format: config.FormatAuto}}
	if i := indexOf(c.Sources, body.SourceID, sourceID); i >= 0 {
		src = c.Sources[i]
	}
	engine := s.pipeline.Engine()
	if body.Rules != nil {
		draft := config.Config{Rules: *body.Rules}
		if err := config.Normalize(&draft); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		engine = classify.New(draft.Rules)
	}
	evs, err := ingest.Parse([]byte(body.Sample), body.ContentType, &src, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(evs), "events": s.preview(evs, engine)})
}

// --- notifiers ---

func notifierID(n *config.Notifier) string { return n.ID }

func (s *Server) listNotifierTypes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, notify.Plugins())
}

func (s *Server) listNotifiers(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Get()
	out := make([]config.Notifier, len(c.Notifiers))
	for i, n := range c.Notifiers {
		out[i] = notify.Mask(n)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createNotifier(w http.ResponseWriter, r *http.Request) {
	var n config.Notifier
	if !readJSON(w, r, &n) {
		return
	}
	n.ID = model.NewID()
	if err := notify.Validate(n); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.update(w, func(c *config.Config) error {
		c.Notifiers = append(c.Notifiers, n)
		return nil
	}) {
		writeJSON(w, http.StatusCreated, notify.Mask(n))
	}
}

func (s *Server) updateNotifier(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var n config.Notifier
	if !readJSON(w, r, &n) {
		return
	}
	n.ID = id
	if s.update(w, func(c *config.Config) error {
		i := indexOf(c.Notifiers, id, notifierID)
		if i < 0 {
			return errNotFound
		}
		if n.Settings == nil {
			n.Settings = map[string]string{}
		}
		notify.Restore(&n, c.Notifiers[i])
		if err := notify.Validate(n); err != nil {
			return err
		}
		c.Notifiers[i] = n
		return nil
	}) {
		writeJSON(w, http.StatusOK, notify.Mask(n))
	}
}

func (s *Server) deleteNotifier(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.update(w, func(c *config.Config) error {
		i := indexOf(c.Notifiers, id, notifierID)
		if i < 0 {
			return errNotFound
		}
		c.Notifiers = slices.Delete(c.Notifiers, i, i+1)
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// testNotifier sends a sample message through a (possibly unsaved)
// notifier configuration.
func (s *Server) testNotifier(w http.ResponseWriter, r *http.Request) {
	var n config.Notifier
	if !readJSON(w, r, &n) {
		return
	}
	if n.Settings == nil {
		n.Settings = map[string]string{}
	}
	if n.ID != "" {
		c := s.cfg.Get()
		if i := indexOf(c.Notifiers, n.ID, notifierID); i >= 0 {
			notify.Restore(&n, c.Notifiers[i])
		}
	}
	if n.Name == "" {
		n.Name = "test"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := s.dispatcher.Test(ctx, n); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- routes ---

func routeID(r *config.Route) string { return r.ID }

func (s *Server) listRoutes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.Get().Routes)
}

func (s *Server) createRoute(w http.ResponseWriter, r *http.Request) {
	var rt config.Route
	if !readJSON(w, r, &rt) {
		return
	}
	rt.ID = model.NewID()
	if s.update(w, func(c *config.Config) error {
		c.Routes = append(c.Routes, rt)
		return nil
	}) {
		writeJSON(w, http.StatusCreated, s.findRoute(rt.ID))
	}
}

func (s *Server) updateRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var rt config.Route
	if !readJSON(w, r, &rt) {
		return
	}
	rt.ID = id
	if s.update(w, func(c *config.Config) error {
		i := indexOf(c.Routes, id, routeID)
		if i < 0 {
			return errNotFound
		}
		c.Routes[i] = rt
		return nil
	}) {
		writeJSON(w, http.StatusOK, s.findRoute(id))
	}
}

func (s *Server) findRoute(id string) *config.Route {
	c := s.cfg.Get()
	if i := indexOf(c.Routes, id, routeID); i >= 0 {
		return &c.Routes[i]
	}
	return nil
}

func (s *Server) deleteRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.update(w, func(c *config.Config) error {
		i := indexOf(c.Routes, id, routeID)
		if i < 0 {
			return errNotFound
		}
		c.Routes = slices.Delete(c.Routes, i, i+1)
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, http.StatusOK, s.dispatcher.Deliveries(limit))
}

// --- settings ---

func maskSettings(st config.Settings) config.Settings {
	st.AI.APIKey = config.MaskValue(st.AI.APIKey)
	return st
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, maskSettings(s.cfg.Get().Settings))
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var st config.Settings
	if !readJSON(w, r, &st) {
		return
	}
	if s.update(w, func(c *config.Config) error {
		st.AI.APIKey = config.Restore(st.AI.APIKey, c.Settings.AI.APIKey)
		c.Settings = st
		return nil
	}) {
		writeJSON(w, http.StatusOK, maskSettings(s.cfg.Get().Settings))
	}
}

// testAI classifies one event (stored, or built from a message) with Claude.
func (s *Server) testAI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EventID string `json:"event_id"`
		Message string `json:"message"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var ev model.Event
	if body.EventID != "" {
		var ok bool
		if ev, ok = s.store.Get(body.EventID); !ok {
			writeError(w, http.StatusNotFound, "event not found")
			return
		}
	} else {
		if body.Message == "" {
			writeError(w, http.StatusBadRequest, "message or event_id is required")
			return
		}
		ev = model.Event{ID: "test", SourceID: "test", SourceName: "test", Message: body.Message, ReceivedAt: time.Now().UTC(), Timestamp: time.Now().UTC()}
		s.pipeline.Engine().Classify(&ev)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	res, err := s.ai.Classify(ctx, &ev)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

// --- import / export ---

func (s *Server) exportConfig(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Get()
	if r.URL.Query().Get("include_secrets") != "true" {
		for i := range c.Sources {
			c.Sources[i] = config.MaskSource(c.Sources[i])
		}
		for i := range c.Notifiers {
			c.Notifiers[i] = notify.Mask(c.Notifiers[i])
		}
		c.Settings = maskSettings(c.Settings)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="laya-config-%s.json"`, time.Now().UTC().Format("20060102-150405")))
	writeJSON(w, http.StatusOK, c)
}

// importConfig replaces the whole configuration. Masked secrets are kept
// from the current config when IDs match.
func (s *Server) importConfig(w http.ResponseWriter, r *http.Request) {
	var next config.Config
	if !readJSON(w, r, &next) {
		return
	}
	for _, n := range next.Notifiers {
		if _, ok := notify.PluginFor(n.Type); !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("notifier %q: unknown type %q", n.Name, n.Type))
			return
		}
	}
	if s.update(w, func(c *config.Config) error {
		for i := range next.Sources {
			if j := indexOf(c.Sources, next.Sources[i].ID, sourceID); j >= 0 {
				config.RestoreSource(&next.Sources[i], c.Sources[j])
			}
		}
		for i := range next.Notifiers {
			if j := indexOf(c.Notifiers, next.Notifiers[i].ID, notifierID); j >= 0 {
				notify.Restore(&next.Notifiers[i], c.Notifiers[j])
			}
		}
		next.Settings.AI.APIKey = config.Restore(next.Settings.AI.APIKey, c.Settings.AI.APIKey)
		next.Version = c.Version
		*c = next
		return nil
	}) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
