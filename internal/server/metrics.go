package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/model"
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
	metric("lissandra_events_received_total", "counter", "Events accepted into the pipeline.", fmt.Sprintf(" %d", p.Received.Load()))
	metric("lissandra_events_processed_total", "counter", "Events classified and stored.", fmt.Sprintf(" %d", p.Processed.Load()))
	metric("lissandra_events_dropped_total", "counter", "Events dropped by rules.", fmt.Sprintf(" %d", p.Dropped.Load()))
	metric("lissandra_events_rejected_total", "counter", "Events rejected because the queue was full.", fmt.Sprintf(" %d", p.Rejected.Load()))
	metric("lissandra_pipeline_queue_depth", "gauge", "Events waiting for classification.", fmt.Sprintf(" %d", p.QueueDepth()))

	st := s.store.Stats(time.Now())
	var sev []string
	for _, v := range model.Severities {
		sev = append(sev, fmt.Sprintf(`{severity=%q} %d`, v, st.BySeverity[v]))
	}
	metric("lissandra_events_last_hour", "gauge", "Events received in the last hour by severity.", sev...)

	metric("lissandra_notifications_total", "counter", "Notification outcomes.",
		fmt.Sprintf(`{status="sent"} %d`, d.Sent.Load()),
		fmt.Sprintf(`{status="failed"} %d`, d.Failed.Load()),
		fmt.Sprintf(`{status="dropped"} %d`, d.Dropped.Load()),
		fmt.Sprintf(`{status="throttled"} %d`, d.Throttled.Load()))
	metric("lissandra_notification_queue_depth", "gauge", "Notifications waiting for delivery.", fmt.Sprintf(" %d", d.QueueDepth()))

	e := s.classifier
	est := e.Status()
	metric("lissandra_engine_calls_total", "counter", "Requests to the engine (live and file events).", fmt.Sprintf(" %d", e.Calls.Load()))
	metric("lissandra_engine_errors_total", "counter", "Failed engine requests.", fmt.Sprintf(" %d", e.Errors.Load()))
	metric("lissandra_engine_timed_out_total", "counter", "Events whose engine request ran out of time.", fmt.Sprintf(" %d", e.TimedOut.Load()))
	metric("lissandra_engine_cache_hits_total", "counter", "Live events answered from the per-pattern cache or a shared request.", fmt.Sprintf(" %d", e.CacheHits.Load()))
	metric("lissandra_engine_applied_total", "counter", "Live engine answers that changed a category or severity.", fmt.Sprintf(" %d", e.Applied.Load()))
	metric("lissandra_engine_low_confidence_total", "counter", "Live engine category answers below the confidence threshold.", fmt.Sprintf(" %d", e.LowConfidence.Load()))
	metric("lissandra_engine_skipped_total", "counter", "Eligible live events not sent to the engine (busy, paused, checkpoint not loaded, or waited too long).", fmt.Sprintf(" %d", e.Skipped.Load()))
	metric("lissandra_engine_queue_depth", "gauge", "Events waiting for the engine.", fmt.Sprintf(" %d", s.pipeline.EngineQueueDepth()))
	metric("lissandra_engine_requests_completed_total", "counter", "Engine requests answered.", fmt.Sprintf(" %d", e.Completed.Load()))
	metric("lissandra_engine_request_seconds_total", "counter", "Time spent on answered engine requests, queueing on the engine included.", fmt.Sprintf(" %.3f", float64(e.LatencyMsTotal.Load())/1000))
	metric("lissandra_engine_inference_seconds_total", "counter", "Engine compute time of answered requests (X-Inference-Time-Ms).", fmt.Sprintf(" %.3f", float64(e.InferMsTotal.Load())/1000))
	paused := 0
	if !est.PausedUntil.IsZero() {
		paused = 1
	}
	metric("lissandra_engine_paused", "gauge", "1 while all engine requests are paused after errors.", fmt.Sprintf(" %d", paused))
	metric("lissandra_engine_checkpoints_paused", "gauge", "Checkpoints paused alone because the engine answered with another.", fmt.Sprintf(" %d", len(est.PausedCheckpoints)))

	ls := s.learningStats()
	metric("lissandra_rules_learned_total", "counter", "Rules learned from the engine's answers.", fmt.Sprintf(" %d", ls.Learned))
	metric("lissandra_rules_review", "gauge", "Learned rules awaiting review.", fmt.Sprintf(" %d", ls.Review))

	metric("lissandra_imports_total", "counter", "Log file analyses by outcome.",
		fmt.Sprintf(`{status="done"} %d`, s.imports.Succeeded.Load()),
		fmt.Sprintf(`{status="failed"} %d`, s.imports.Failed.Load()))
	metric("lissandra_import_events_total", "counter", "Events classified from uploaded log files.", fmt.Sprintf(" %d", s.imports.Events.Load()))
	metric("lissandra_imports_active", "gauge", "Log file analyses in progress.", fmt.Sprintf(" %d", s.imports.Active()))

	var pulls []string
	for id, ps := range s.pollers.Status() {
		up := 0
		if ps.LastError == "" && !ps.LastSuccess.IsZero() {
			up = 1
		}
		pulls = append(pulls, fmt.Sprintf(`{source_id=%q} %d`, id, up))
	}
	metric("lissandra_pull_source_up", "gauge", "1 if the last poll of a pull source succeeded.", pulls...)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
