package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/vipinmohan22/lissandra/internal/notify"
)

// The alert decision layer's API: pending digests and what Lissandra is
// learning (/api/alerting), "Send now", and one delivery with its digest.
func (s *Server) alertingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/alerting", s.getAlerting)
	mux.HandleFunc("POST /api/alerting/digests/{notifier_id}/send", s.sendDigest)
	mux.HandleFunc("GET /api/deliveries/{id}", s.getDelivery)
}

func (s *Server) getAlerting(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.dispatcher.AlertingStatus())
}

// sendDigest sends a channel's pending digest now, with reason "manual".
func (s *Server) sendDigest(w http.ResponseWriter, r *http.Request) {
	id, err := s.dispatcher.FlushDigest(r.PathValue("notifier_id"))
	switch {
	case errors.Is(err, notify.ErrNotifierDisabled):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, notify.ErrQueueFull):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
	}
}

// getDelivery returns one delivery record and, for one of the last 50
// digests, the digest in full (else null).
func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) {
	del, dg, ok := s.dispatcher.DeliveryDetail(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "delivery not found (only the last 500 are kept)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivery": del, "digest": dg})
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	kind := q.Get("kind")
	if kind != "" && kind != notify.KindAlert && kind != notify.KindDigest {
		writeError(w, http.StatusBadRequest, `kind must be "alert" or "digest"`)
		return
	}
	writeJSON(w, http.StatusOK, s.dispatcher.Deliveries(limit, kind))
}

// decisions is what an event did on each channel: [] when its routes
// reached no enabled channel, null when nothing is known (it matched no
// route, or it is older than the last 5,000 events).
func (s *Server) decisions(eventID string) []notify.Decision {
	return s.dispatcher.Decisions(eventID)
}
