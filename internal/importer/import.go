// Package importer analyzes uploaded log files: it streams a file through
// parsing and classification and builds a report (counts, a timeline by
// log time, top issues, the alert routes that would have fired) plus a
// bounded, severity-prioritized sample of events to browse. Imports are
// kept apart from live events and never send notifications.
package importer

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vipinmohan22/lissandra/internal/ingest"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// Import statuses.
const (
	StatusRunning = "running"
	StatusDone    = "done"   // possibly Truncated, with a StopReason
	StatusFailed  = "failed" // nothing usable (unsupported input, internal error)
)

// Stop reasons for truncated or failed imports.
const (
	StopUploadLimit       = "upload_limit"
	StopDecompressedLimit = "decompressed_limit"
	StopRatioLimit        = "ratio_limit"
	StopClientAborted     = "client_aborted"
	StopDeadline          = "deadline"
	StopGzipTruncated     = "gzip_truncated"
	StopSyntax            = "syntax_error"
	StopRecordTooLarge    = "record_too_large"
	StopReadError         = "read_error"
	StopUnsupported       = "unsupported"
	StopInvalid           = "invalid_request"
	StopInternal          = "internal"
	StopOutput            = "output_error" // lissandra analyze could not write its output
)

const (
	maxPatterns         = 20000 // distinct fingerprints tracked while running
	keptPatterns        = 300   // kept after the import finishes
	topPatternsInReport = 25
	maxRouteFingerprint = 20000 // distinct fingerprints counted per route
	retainedMsgBytes    = 8 << 10
	retainedFieldsBytes = 16 << 10
	samplesPerPattern   = 2
)

// Limits bounds what an import keeps in memory for browsing.
type Limits struct {
	Events int   // retained events (0 keeps none)
	Bytes  int64 // approximate retained bytes
}

// DefaultLimits are used by the server.
var DefaultLimits = Limits{Events: 5000, Bytes: 16 << 20}

// Report summarizes one analyzed file.
type Report struct {
	ID         string    `json:"id"`
	FileName   string    `json:"file_name"`
	SourceID   string    `json:"source_id,omitempty"`
	SourceName string    `json:"source_name"`
	Status     string    `json:"status"`
	Truncated  bool      `json:"truncated"`
	StopReason string    `json:"stop_reason,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	TimeZone   string    `json:"time_zone"`
	Compressed bool      `json:"compressed"`

	Parse         ingest.StreamStats `json:"parse"`
	Bytes         int64              `json:"bytes"` // decompressed bytes read
	Lines         int                `json:"lines"`
	Events        int64              `json:"events"`
	Dropped       int64              `json:"dropped"` // dropped by rules
	Retained      int                `json:"retained"`
	RetainedBytes int64              `json:"retained_bytes"`
	EventsEvicted bool               `json:"events_evicted,omitempty"` // retained events freed to save memory
	YieldedMs     int64              `json:"yielded_ms,omitempty"`     // paused so live ingest could catch up

	FirstTimestamp time.Time `json:"first_timestamp,omitzero"`
	LastTimestamp  time.Time `json:"last_timestamp,omitzero"`

	BySeverity map[model.Severity]int64 `json:"by_severity"`
	ByCategory map[string]int64         `json:"by_category"`
	ByRule     map[string]int64         `json:"by_rule"`
	Routes     []RouteHits              `json:"routes"`

	DistinctPatterns int   `json:"distinct_patterns"`
	OtherPatterns    int64 `json:"other_pattern_events,omitempty"` // events of patterns evicted from the table

	// Events the engine answered for (from its cache or asked during
	// the analysis), and those whose category or severity it set.
	EngineEvents  int64 `json:"engine_events,omitempty"`
	EngineApplied int64 `json:"engine_applied,omitempty"`

	// Detail fields, filled by Import.Report(true).
	Timeline    *Timeline `json:"timeline,omitempty"`
	TopPatterns []Pattern `json:"top_patterns,omitempty"`
}

// RouteHits counts events an alert route would have matched. Patterns is
// the number of distinct fingerprints: with throttling on, roughly the
// number of alerts the route would have sent.
type RouteHits struct {
	Name     string `json:"name"`
	Events   int64  `json:"events"`
	Patterns int    `json:"patterns"`
}

// Pattern groups events sharing a fingerprint.
type Pattern struct {
	Fingerprint string         `json:"fingerprint"`
	Sample      string         `json:"sample"`
	Category    string         `json:"category"`
	Severity    model.Severity `json:"severity"`
	Count       int64          `json:"count"`
	FirstLine   int            `json:"first_line,omitempty"`
	FirstSeen   time.Time      `json:"first_seen,omitzero"` // log time
	LastSeen    time.Time      `json:"last_seen,omitzero"`
	Retained    int            `json:"retained"` // events of this pattern available to browse

	samples int // protected samples kept
}

// Import accumulates the analysis of one file. One writer (Run) calls add;
// readers may call Report, Events and Event concurrently.
type Import struct {
	mu       sync.RWMutex
	report   Report
	start    time.Time
	patterns map[string]*Pattern
	routes   map[string]*routeAcc
	timeline timeAgg
	final    *Timeline // computed when done
	keep     retainer
	sorted   []model.Event // retained events in file order, set when done
	done     chan struct{}
	doneOnce sync.Once
}

type routeAcc struct {
	events int64
	fps    map[string]struct{}
}

// New returns an empty import. Events without a timestamp are given start.
func New(fileName string, limits Limits, start time.Time) *Import {
	return newImport(model.NewID(), fileName, limits, start)
}

func newImport(id, fileName string, limits Limits, start time.Time) *Import {
	start = start.UTC()
	return &Import{
		report: Report{
			ID: id, FileName: fileName, Status: StatusRunning, StartedAt: start, TimeZone: "UTC",
			BySeverity: map[model.Severity]int64{}, ByCategory: map[string]int64{}, ByRule: map[string]int64{},
		},
		start:    start,
		patterns: map[string]*Pattern{},
		routes:   map[string]*routeAcc{},
		timeline: newTimeAgg(),
		keep:     retainer{maxCount: limits.Events, maxBytes: limits.Bytes},
		done:     make(chan struct{}),
	}
}

// ID returns the import's identifier.
func (im *Import) ID() string { return im.report.ID }

// Done is closed when the import finishes.
func (im *Import) Done() <-chan struct{} { return im.done }

func (im *Import) update(fn func(r *Report)) {
	im.mu.Lock()
	fn(&im.report)
	im.mu.Unlock()
}

// add records one classified event. kept is false when a rule dropped it;
// routes lists the alert routes the event matches.
func (im *Import) add(ev *model.Event, kept bool, routes []string) {
	im.mu.Lock()
	defer im.mu.Unlock()
	r := &im.report
	if ev.Line > r.Lines {
		r.Lines = ev.Line // progress while streaming; Run sets the final count
	}
	if !kept {
		r.Dropped++
		return
	}
	r.Events++
	r.BySeverity[ev.Severity]++
	r.ByCategory[ev.Category]++
	if ev.Engine != nil {
		r.EngineEvents++
		if ev.Engine.Applied {
			r.EngineApplied++
		}
	}
	for _, name := range ev.MatchedRules {
		r.ByRule[name]++
	}
	for _, name := range routes {
		acc := im.routes[name]
		if acc == nil {
			acc = &routeAcc{fps: map[string]struct{}{}}
			im.routes[name] = acc
		}
		acc.events++
		if len(acc.fps) < maxRouteFingerprint {
			acc.fps[ev.Fingerprint] = struct{}{}
		}
	}
	timestamped := !ev.Timestamp.IsZero()
	if timestamped {
		if r.FirstTimestamp.IsZero() || ev.Timestamp.Before(r.FirstTimestamp) {
			r.FirstTimestamp = ev.Timestamp
		}
		if ev.Timestamp.After(r.LastTimestamp) {
			r.LastTimestamp = ev.Timestamp
		}
		im.timeline.add(ev.Timestamp.Unix(), ev.Severity.Rank())
	}

	p := im.patterns[ev.Fingerprint]
	if p == nil {
		if len(im.patterns) >= maxPatterns {
			im.evictPatterns()
		}
		p = &Pattern{Fingerprint: ev.Fingerprint, Sample: strings.Clone(model.Truncate(firstLine(ev.Message), 300)), Category: ev.Category, FirstLine: ev.Line}
		im.patterns[ev.Fingerprint] = p
	}
	p.Count++
	if ev.Severity.Rank() > p.Severity.Rank() {
		p.Severity = ev.Severity
	}
	if timestamped {
		if p.FirstSeen.IsZero() || ev.Timestamp.Before(p.FirstSeen) {
			p.FirstSeen = ev.Timestamp
		}
		if ev.Timestamp.After(p.LastSeen) {
			p.LastSeen = ev.Timestamp
		}
	}
	if im.keep.add(ev, p.samples < samplesPerPattern) {
		p.samples++
	}
	r.Retained, r.RetainedBytes = im.keep.count(), im.keep.bytes()
}

// evictPatterns drops the half of the pattern table with the lowest counts
// (sparing error-or-worse patterns when possible) to make room.
func (im *Import) evictPatterns() {
	list := make([]*Pattern, 0, len(im.patterns))
	for _, p := range im.patterns {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		ei, ej := list[i].Severity.AtLeast(model.SeverityError), list[j].Severity.AtLeast(model.SeverityError)
		if ei != ej {
			return ej // non-errors sort first and are evicted first
		}
		return list[i].Count < list[j].Count
	})
	for _, p := range list[:len(list)/2] {
		im.report.OtherPatterns += p.Count
		delete(im.patterns, p.Fingerprint)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// finish marks the import complete and releases working memory. Only the
// first call has an effect.
func (im *Import) finish(status, stopReason, errMsg string, finished time.Time) {
	defer im.doneOnce.Do(func() { close(im.done) })
	im.mu.Lock()
	defer im.mu.Unlock()
	r := &im.report
	if r.Status != StatusRunning {
		return
	}
	// Mark failed first, so a panic below still leaves a terminal state.
	r.Status, r.StopReason, r.Error, r.FinishedAt = StatusFailed, StopInternal, "internal error while finishing", finished
	r.DistinctPatterns = len(im.patterns)
	r.Routes = im.routeHits()
	im.routes = nil
	im.sorted = im.keep.ordered()
	t := im.timeline.snapshot()
	im.final = &t
	im.timeline = timeAgg{}
	im.trimPatterns()

	r.Status, r.StopReason, r.Error = status, stopReason, errMsg
	r.Truncated = status == StatusDone && stopReason != ""
}

// trimPatterns keeps the most useful patterns once the import is done.
func (im *Import) trimPatterns() {
	if len(im.patterns) <= keptPatterns {
		return
	}
	for _, p := range im.sortedPatterns()[keptPatterns:] {
		delete(im.patterns, p.Fingerprint)
	}
}

// dropEvents frees retained events, keeping the report. It returns the
// approximate bytes freed.
func (im *Import) dropEvents() int64 {
	im.mu.Lock()
	defer im.mu.Unlock()
	freed := im.keep.bytes()
	if im.keep.count() == 0 {
		return 0
	}
	im.keep = retainer{}
	im.sorted = nil
	im.report.Retained, im.report.RetainedBytes, im.report.EventsEvicted = 0, 0, true
	return freed
}

// retainedBytes reports the approximate memory held by retained events.
func (im *Import) retainedBytes() int64 {
	im.mu.RLock()
	defer im.mu.RUnlock()
	return im.keep.bytes()
}

// Running reports whether the import is still in progress.
func (im *Import) Running() bool { return im.status() == StatusRunning }

func (im *Import) status() string {
	im.mu.RLock()
	defer im.mu.RUnlock()
	return im.report.Status
}

// Report returns a snapshot. With detail it includes the timeline and top
// patterns.
func (im *Import) Report(detail bool) Report {
	im.mu.RLock()
	defer im.mu.RUnlock()
	r := im.report
	r.BySeverity = copyMap(r.BySeverity)
	r.ByCategory = copyMap(r.ByCategory)
	r.ByRule = copyMap(r.ByRule)
	if im.routes != nil {
		r.DistinctPatterns = len(im.patterns)
		r.Routes = im.routeHits()
	} else {
		r.Routes = append([]RouteHits{}, r.Routes...)
	}
	if detail {
		var t Timeline
		if im.final != nil {
			t = *im.final
		} else if im.timeline.buckets != nil {
			t = im.timeline.snapshot()
		}
		r.Timeline = &t
		r.TopPatterns = im.patternsLocked(PatternQuery{Limit: topPatternsInReport})
	}
	return r
}

func (im *Import) routeHits() []RouteHits {
	out := make([]RouteHits, 0, len(im.routes))
	for name, acc := range im.routes {
		out = append(out, RouteHits{Name: name, Events: acc.events, Patterns: len(acc.fps)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Events != out[j].Events {
			return out[i].Events > out[j].Events
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sortedPatterns orders patterns: warnings and worse first (most severe
// first), then by volume, then file order.
func (im *Import) sortedPatterns() []*Pattern {
	list := make([]*Pattern, 0, len(im.patterns))
	for _, p := range im.patterns {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		aw, bw := a.Severity.AtLeast(model.SeverityWarning), b.Severity.AtLeast(model.SeverityWarning)
		if aw != bw {
			return aw
		}
		if aw && a.Severity != b.Severity {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.FirstLine < b.FirstLine
	})
	return list
}

// PatternQuery filters patterns.
type PatternQuery struct {
	MinSeverity model.Severity
	Category    string
	Fingerprint string
	Text        string
	Limit       int
}

// Patterns returns matching patterns in report order.
func (im *Import) Patterns(q PatternQuery) []Pattern {
	im.mu.RLock()
	defer im.mu.RUnlock()
	return im.patternsLocked(q)
}

func (im *Import) patternsLocked(q PatternQuery) []Pattern {
	if q.Limit <= 0 || q.Limit > keptPatterns {
		q.Limit = keptPatterns
	}
	text := strings.ToLower(q.Text)
	retained := map[string]int{}
	im.keep.each(func(ev *model.Event) { retained[ev.Fingerprint]++ })
	out := []Pattern{}
	for _, p := range im.sortedPatterns() {
		if q.MinSeverity != "" && !p.Severity.AtLeast(q.MinSeverity) ||
			q.Category != "" && p.Category != q.Category ||
			q.Fingerprint != "" && p.Fingerprint != q.Fingerprint ||
			text != "" && !strings.Contains(strings.ToLower(p.Sample), text) {
			continue
		}
		cp := *p
		cp.Retained = retained[p.Fingerprint]
		out = append(out, cp)
		if len(out) == q.Limit {
			break
		}
	}
	return out
}

// Query filters retained events.
type Query struct {
	MinSeverity model.Severity
	Category    string
	Fingerprint string
	Text        string
	Offset      int
	Limit       int // 0 means 200 (max 1000); negative means no limit
}

// Events returns retained events matching q in file order, and how many
// match in total.
func (im *Import) Events(q Query) ([]model.Event, int) {
	if q.Limit == 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	text := strings.ToLower(q.Text)
	im.mu.RLock()
	defer im.mu.RUnlock()
	all := im.sorted
	if im.final == nil { // still running: order what is kept so far
		all = im.keep.ordered()
	}
	out := []model.Event{}
	total := 0
	for i := range all {
		ev := &all[i]
		if q.MinSeverity != "" && !ev.Severity.AtLeast(q.MinSeverity) ||
			q.Category != "" && ev.Category != q.Category ||
			q.Fingerprint != "" && ev.Fingerprint != q.Fingerprint ||
			text != "" && !strings.Contains(strings.ToLower(ev.Message), text) {
			continue
		}
		if total >= q.Offset && (q.Limit < 0 || len(out) < q.Limit) {
			out = append(out, *ev)
		}
		total++
	}
	return out, total
}

// Event returns a retained event by ID.
func (im *Import) Event(id string) (model.Event, bool) {
	im.mu.RLock()
	defer im.mu.RUnlock()
	var found *model.Event
	im.keep.each(func(ev *model.Event) {
		if found == nil && ev.ID == id {
			found = ev
		}
	})
	if found == nil {
		return model.Event{}, false
	}
	return *found, true
}
