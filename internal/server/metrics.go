package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

// handleMetrics writes Prometheus text-format metrics.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	metric := func(name, typ, help string, samples ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, smp := range samples {
			b.WriteString(name + smp + "\n")
		}
	}
	p, d := s.pipeline, s.dispatcher
	metric("laya_events_received_total", "counter", "Events accepted into the pipeline.", fmt.Sprintf(" %d", p.Received.Load()))
	metric("laya_events_processed_total", "counter", "Events classified and stored.", fmt.Sprintf(" %d", p.Processed.Load()))
	metric("laya_events_dropped_total", "counter", "Events dropped by rules.", fmt.Sprintf(" %d", p.Dropped.Load()))
	metric("laya_events_rejected_total", "counter", "Events rejected because the queue was full.", fmt.Sprintf(" %d", p.Rejected.Load()))
	metric("laya_pipeline_queue_depth", "gauge", "Events waiting for classification.", fmt.Sprintf(" %d", p.QueueDepth()))

	st := s.store.Stats(time.Now())
	var sev []string
	for _, v := range model.Severities {
		sev = append(sev, fmt.Sprintf(`{severity=%q} %d`, v, st.BySeverity[v]))
	}
	metric("laya_events_last_hour", "gauge", "Events received in the last hour by severity.", sev...)

	metric("laya_notifications_total", "counter", "Notification outcomes.",
		fmt.Sprintf(`{status="sent"} %d`, d.Sent.Load()),
		fmt.Sprintf(`{status="failed"} %d`, d.Failed.Load()),
		fmt.Sprintf(`{status="dropped"} %d`, d.Dropped.Load()),
		fmt.Sprintf(`{status="throttled"} %d`, d.Throttled.Load()))
	metric("laya_notification_queue_depth", "gauge", "Notifications waiting for delivery.", fmt.Sprintf(" %d", d.QueueDepth()))

	metric("laya_ai_calls_total", "counter", "Claude API classification calls.", fmt.Sprintf(" %d", s.ai.Calls.Load()))
	metric("laya_ai_errors_total", "counter", "Failed Claude API calls.", fmt.Sprintf(" %d", s.ai.Errors.Load()))
	metric("laya_ai_cache_hits_total", "counter", "AI classifications served from cache.", fmt.Sprintf(" %d", s.ai.CacheHits.Load()))

	var pulls []string
	for id, ps := range s.pollers.Status() {
		up := 0
		if ps.LastError == "" && !ps.LastSuccess.IsZero() {
			up = 1
		}
		pulls = append(pulls, fmt.Sprintf(`{source_id=%q} %d`, id, up))
	}
	metric("laya_pull_source_up", "gauge", "1 if the last poll of a pull source succeeded.", pulls...)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
