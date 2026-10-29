package importer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

var start = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func engine() *classify.Engine {
	c := config.Defaults()
	config.Normalize(c)
	return classify.New(c.Rules)
}

func src() config.Source {
	return config.Source{Name: "file upload", Type: config.SourcePush, Parse: config.Parse{Format: config.FormatAuto}}
}

func runImport(t *testing.T, input io.Reader, limits Limits, opts Options, deps Deps) (*Import, error) {
	t.Helper()
	im := New("app.log", limits, start)
	if opts.Source.Name == "" {
		opts.Source = src()
	}
	if deps.Classify == nil {
		deps.Classify = engine().Classify
	}
	err := Run(context.Background(), im, input, opts, deps)
	return im, err
}

func logLines(n int, every func(i int) string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(every(i))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestReportAggregates(t *testing.T) {
	input := "2026-10-11T10:00:00Z INFO started\n" +
		"2026-10-11T10:00:05Z ERROR payment failed: connection refused to payments:8443\n" +
		"2026-10-11T10:00:06Z ERROR payment failed: connection refused to payments:8444\n" +
		"2026-10-11T10:01:00Z WARN login failed for user bob\n" +
		"{\"msg\":\"GET /healthz 200\"}\n"
	routes := func(ev *model.Event) []string {
		if ev.Severity.AtLeast(model.SeverityError) {
			return []string{"oncall"}
		}
		return nil
	}
	im, err := runImport(t, strings.NewReader(input), DefaultLimits, Options{FileName: "app.log"}, Deps{Routes: routes})
	if err != nil {
		t.Fatal(err)
	}
	r := im.Report(true)
	if r.Status != StatusDone || r.Truncated || r.Events != 5 || r.Lines != 5 || r.Parse.Mode != "text-anchored" {
		t.Fatalf("report %+v", r)
	}
	if r.BySeverity[model.SeverityError] != 2 || r.ByCategory["network"] != 2 || r.ByRule["Network"] != 2 {
		t.Errorf("counts %v %v %v", r.BySeverity, r.ByCategory, r.ByRule)
	}
	if len(r.Routes) != 1 || r.Routes[0] != (RouteHits{Name: "oncall", Events: 2, Patterns: 1}) {
		t.Errorf("routes %+v (two errors share one pattern)", r.Routes)
	}
	if !r.FirstTimestamp.Equal(time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)) || !r.LastTimestamp.Equal(time.Date(2026, 10, 11, 10, 1, 0, 0, time.UTC)) {
		t.Errorf("time range %v %v", r.FirstTimestamp, r.LastTimestamp)
	}
	if r.Parse.TimeInferred != 1 { // the healthz line inherits 10:01
		t.Errorf("parse stats %+v", r.Parse)
	}
	top := r.TopPatterns
	if len(top) != 4 || top[0].Severity != model.SeverityError || top[0].Count != 2 || top[0].FirstLine != 2 || top[0].Retained != 2 {
		t.Errorf("top patterns %+v", top)
	}
	if r.Timeline == nil || len(r.Timeline.Points) != 2 || r.Timeline.Step != 60 {
		t.Errorf("timeline %+v", r.Timeline)
	}
	evs, total := im.Events(Query{MinSeverity: model.SeverityError})
	if total != 2 || evs[0].Line != 2 || evs[0].File != "app.log" || evs[0].ImportID != r.ID {
		t.Errorf("events %d %+v", total, evs)
	}
	if ev, ok := im.Event(evs[1].ID); !ok || ev.Line != 3 {
		t.Errorf("event lookup %v %+v", ok, ev)
	}
}

func TestRetentionBudgets(t *testing.T) {
	// 2000 info lines then 50 errors then 2000 more info: with room for 100
	// events, every error must be kept, plus samples of the info pattern.
	input := logLines(4050, func(i int) string {
		if i >= 2000 && i < 2050 {
			return fmt.Sprintf("2026-10-11T10:00:00Z ERROR payment failed order %d", i)
		}
		return fmt.Sprintf("2026-10-11T10:00:00Z INFO request %d ok", i)
	})
	im, err := runImport(t, strings.NewReader(input), Limits{Events: 100, Bytes: 1 << 20}, Options{}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	r := im.Report(false)
	if r.Retained != 100 || r.RetainedBytes > 1<<20 {
		t.Fatalf("retained %d (%d bytes)", r.Retained, r.RetainedBytes)
	}
	if _, total := im.Events(Query{MinSeverity: model.SeverityError}); total != 50 {
		t.Errorf("kept %d of 50 errors", total)
	}
	all, _ := im.Events(Query{Limit: -1})
	for i := 1; i < len(all); i++ {
		if all[i].Line <= all[i-1].Line {
			t.Fatalf("events not in file order at %d", i)
		}
	}

	// Byte budget: big messages.
	big := logLines(500, func(i int) string {
		return fmt.Sprintf("2026-10-11T10:00:00Z ERROR %d %s", i, strings.Repeat("x", 20000))
	})
	im, _ = runImport(t, strings.NewReader(big), Limits{Events: 5000, Bytes: 200 << 10}, Options{}, Deps{})
	r = im.Report(false)
	if r.RetainedBytes > 200<<10 || r.Retained == 0 {
		t.Errorf("byte budget: %d bytes, %d events", r.RetainedBytes, r.Retained)
	}
	evs, _ := im.Events(Query{Limit: 1})
	if len(evs[0].Message) > retainedMsgBytes+4 {
		t.Errorf("retained message not compacted: %d bytes", len(evs[0].Message))
	}

	// Limits{} keeps nothing.
	im, _ = runImport(t, strings.NewReader(input), Limits{}, Options{}, Deps{})
	if r := im.Report(false); r.Retained != 0 || r.Events != 4050 {
		t.Errorf("no retention: %+v", r)
	}
}

func TestRetainerInvariants(t *testing.T) {
	r := retainer{maxCount: 50, maxBytes: 40 << 10}
	sevs := model.Severities
	for i := 0; i < 5000; i++ {
		ev := model.Event{Message: strings.Repeat("m", (i*37)%3000), Severity: sevs[(i*7)%len(sevs)], Fingerprint: fmt.Sprint(i % 13)}
		r.add(&ev, i%97 == 0)
		if r.count() > r.maxCount || r.bytes() > r.maxBytes {
			t.Fatalf("budget exceeded at %d: %d events, %d bytes", i, r.count(), r.bytes())
		}
		var gc int
		var gb int64
		for rank := range r.general {
			gc += len(r.general[rank])
			for _, it := range r.general[rank] {
				gb += it.cost
			}
			if gb != sumBytes(r.gBytes[:rank+1]) {
				t.Fatalf("byte accounting drifted at %d", i)
			}
		}
		if gc != r.gCount {
			t.Fatalf("count accounting drifted at %d", i)
		}
	}
	// The general pool favours severe events.
	if len(r.general[model.SeverityCritical.Rank()]) == 0 {
		t.Error("no critical events kept")
	}
}

func sumBytes(b []int64) int64 {
	var n int64
	for _, v := range b {
		n += v
	}
	return n
}

func TestTimelineOutliersAndCoarsening(t *testing.T) {
	agg := newTimeAgg()
	base := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < 5000; i++ {
		agg.add(base+int64(i), 2) // ~83 minutes of activity
	}
	agg.add(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), 5) // device without a clock
	agg.add(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), 5) // bogus epoch
	tl := agg.snapshot()
	if tl.Before != 1 || tl.After != 1 {
		t.Errorf("outliers before=%d after=%d", tl.Before, tl.After)
	}
	var total int64
	for _, p := range tl.Points {
		total += p.Total
	}
	if total != 5000 || len(tl.Points) > timelinePoints || tl.Step != 60 {
		t.Errorf("charted %d events in %d points, step %d", total, len(tl.Points), tl.Step)
	}

	// Natural edges are never cut (the partial last minute stays).
	agg = newTimeAgg()
	for i := 0; i < 3000; i++ {
		agg.add(base+int64(i)*2, 2)
	}
	agg.add(base+6000, 2)
	if tl := agg.snapshot(); tl.Before != 0 || tl.After != 0 {
		t.Errorf("natural tail treated as outlier: %+v", tl.After)
	}

	// Spans of years coarsen but stay bounded and exact.
	agg = newTimeAgg()
	for d := 0; d < 3*365; d++ {
		for h := 0; h < 24; h++ {
			agg.add(base+int64(d)*86400+int64(h)*3600, 1)
		}
	}
	if len(agg.buckets) > maxTimelineKeys || agg.res() < 3600 {
		t.Errorf("not coarsened: %d buckets at %ds", len(agg.buckets), agg.res())
	}
	tl = agg.snapshot()
	total = 0
	for _, p := range tl.Points {
		total += p.Total
	}
	if total+tl.Before+tl.After != 3*365*24 || len(tl.Points) > timelinePoints {
		t.Errorf("years: %d events in %d points (step %d)", total, len(tl.Points), tl.Step)
	}
}

func TestPatternEviction(t *testing.T) {
	im := New("x", DefaultLimits, start)
	for i := 0; i < maxPatterns+10; i++ {
		ev := model.Event{Message: "m", Fingerprint: fmt.Sprint("fp", i), Severity: model.SeverityInfo, Category: "c"}
		if i == 7 {
			ev.Severity = model.SeverityError
		}
		im.add(&ev, true, nil)
	}
	if len(im.patterns) > maxPatterns || im.patterns["fp7"] == nil {
		t.Errorf("table %d, error pattern kept=%v", len(im.patterns), im.patterns["fp7"] != nil)
	}
	if im.report.OtherPatterns == 0 {
		t.Error("evicted pattern counts should go to other")
	}
	im.finish(StatusDone, "", "", time.Now())
	if len(im.patterns) > keptPatterns {
		t.Errorf("patterns not trimmed after finish: %d", len(im.patterns))
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestStopReasonsKeepPartialResults(t *testing.T) {
	lines := logLines(5000, func(i int) string { return fmt.Sprintf("2026-10-11T10:00:00Z ERROR thing %d failed", i) })

	im, err := runImport(t, strings.NewReader(lines), DefaultLimits, Options{MaxBytes: 64 << 10}, Deps{})
	if r := im.Report(false); err != nil || r.Status != StatusDone || !r.Truncated || r.StopReason != StopDecompressedLimit || r.Events == 0 {
		t.Errorf("size limit: %v %+v", err, r)
	}

	stop := &StopError{Reason: StopUploadLimit, Err: errors.New("too big")}
	im, err = runImport(t, &failingReader{data: []byte(lines[:10000]), err: stop}, DefaultLimits, Options{}, Deps{})
	if r := im.Report(false); err != nil || r.StopReason != StopUploadLimit || r.Events == 0 {
		t.Errorf("upload limit: %v %+v", err, r)
	}

	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	im = New("x", DefaultLimits, start)
	err = Run(ctx, im, strings.NewReader(lines), Options{Source: src()}, Deps{Classify: func(ev *model.Event) bool {
		if n++; n == 600 {
			cancel()
		}
		return true
	}})
	if r := im.Report(false); err != nil || r.StopReason != StopClientAborted || r.Events == 0 {
		t.Errorf("abort: %v %+v", err, r)
	}

	im = New("x", DefaultLimits, start)
	err = Run(context.Background(), im, strings.NewReader(lines), Options{Source: src(), Deadline: time.Now().Add(-time.Second)}, Deps{Classify: engine().Classify})
	if r := im.Report(false); err != nil || r.StopReason != StopDeadline {
		t.Errorf("deadline: %v %+v", err, r)
	}

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(lines))
	w.Close()
	im, err = runImport(t, bytes.NewReader(gz.Bytes()[:gz.Len()/2]), DefaultLimits, Options{}, Deps{})
	if r := im.Report(false); err != nil || r.StopReason != StopGzipTruncated || !r.Compressed || r.Events == 0 {
		t.Errorf("truncated gzip: %v %+v", err, r)
	}

	im, err = runImport(t, strings.NewReader("PK\x03\x04zip"), DefaultLimits, Options{}, Deps{})
	if r := im.Report(false); err == nil || r.Status != StatusFailed || r.StopReason != StopUnsupported {
		t.Errorf("unsupported: %v %+v", err, r)
	}

	im, err = runImport(t, strings.NewReader("[{\"msg\":\"a\"},{\"msg\": nope}]"), DefaultLimits, Options{Format: config.FormatJSON}, Deps{})
	if r := im.Report(false); err != nil || r.StopReason != StopSyntax || r.Events != 1 {
		t.Errorf("syntax: %v %+v", err, r)
	}
}

func TestPanicLeavesTerminalState(t *testing.T) {
	im := New("x", DefaultLimits, start)
	err := Run(context.Background(), im, strings.NewReader("a\nb\n"), Options{Source: src()}, Deps{Classify: func(*model.Event) bool { panic("boom") }})
	r := im.Report(true)
	if err == nil || r.Status != StatusFailed || r.StopReason != StopInternal || !strings.Contains(r.Error, "boom") {
		t.Errorf("panic: %v %+v", err, r)
	}
	select {
	case <-im.Done():
	default:
		t.Error("done channel not closed")
	}
}

func TestDecompressionBombStops(t *testing.T) {
	var gz bytes.Buffer
	w, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for i := 0; i < (ratioFloor+(8<<20))/len(line); i++ {
		w.Write(line)
	}
	w.Close()
	im, err := runImport(t, bytes.NewReader(gz.Bytes()), Limits{}, Options{}, Deps{Classify: func(*model.Event) bool { return true }})
	if r := im.Report(false); err != nil || r.StopReason != StopRatioLimit || r.Bytes > ratioFloor+(2<<20) {
		t.Errorf("bomb: %v reason=%s bytes=%d (compressed %d)", err, r.StopReason, r.Bytes, gz.Len())
	}
}

func TestParallelMatchesSequential(t *testing.T) {
	input := logLines(3000, func(i int) string {
		return fmt.Sprintf("2026-10-11T10:%02d:00Z %s event %d %s", i%60, []string{"INFO", "WARN", "ERROR"}[i%3], i, []string{"timeout", "deadlock", "ok"}[i%3])
	})
	seq, _ := runImport(t, strings.NewReader(input), DefaultLimits, Options{Workers: 1}, Deps{})
	par, _ := runImport(t, strings.NewReader(input), DefaultLimits, Options{Workers: 4}, Deps{})
	a, b := seq.Report(true), par.Report(true)
	if fmt.Sprint(a.BySeverity, a.ByCategory, a.ByRule, a.Events) != fmt.Sprint(b.BySeverity, b.ByCategory, b.ByRule, b.Events) {
		t.Errorf("parallel differs:\n%v\n%v", a, b)
	}
	ea, _ := seq.Events(Query{Limit: -1})
	eb, _ := par.Events(Query{Limit: -1})
	if len(ea) != len(eb) {
		t.Fatalf("retained %d vs %d", len(ea), len(eb))
	}
	for i := range ea {
		if ea[i].Line != eb[i].Line || ea[i].Category != eb[i].Category {
			t.Fatalf("event %d differs", i)
		}
	}
}

func TestYieldsToLiveIngest(t *testing.T) {
	var calls atomic.Int32
	busy := func() bool { return calls.Add(1) <= 3 }
	im, err := runImport(t, strings.NewReader("a\nb\n"), DefaultLimits, Options{}, Deps{Busy: busy})
	if err != nil || im.Report(false).YieldedMs < 100 {
		t.Errorf("yield: %v %d ms", err, im.Report(false).YieldedMs)
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry(RegistryConfig{Keep: 2, Limits: Limits{Events: 100, Bytes: 1 << 20}, RetainedBytes: 1 << 20})
	a, err := reg.Begin("", "a.log", start)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Begin("", "b.log", start); !errors.Is(err, ErrBusy) {
		t.Errorf("second concurrent import: %v", err)
	}
	if err := reg.Delete(a.ID()); !errors.Is(err, ErrRunning) {
		t.Errorf("delete running: %v", err)
	}
	Run(context.Background(), a, strings.NewReader("x\n"), Options{Source: src()}, Deps{Classify: engine().Classify})
	reg.Done(a)

	if _, err := reg.Begin("NOTHEX", "c", start); !errors.Is(err, ErrBadID) {
		t.Errorf("bad id: %v", err)
	}
	b, err := reg.Begin("0123456789abcdef", "b.log", start)
	if err != nil || b.ID() != "0123456789abcdef" {
		t.Fatalf("client id: %v", err)
	}
	Fail(b, "nope")
	reg.Done(b)
	if _, err := reg.Begin("0123456789abcdef", "dup", start); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate id: %v", err)
	}
	c, _ := reg.Begin("", "c.log", start)
	Run(context.Background(), c, strings.NewReader("y\n"), Options{Source: src()}, Deps{Classify: engine().Classify})
	reg.Done(c)
	if list := reg.List(); len(list) != 2 || list[0].ID != c.ID() {
		t.Errorf("keep 2 newest first: %+v", list)
	}
	if reg.Succeeded.Load() != 2 || reg.Failed.Load() != 1 || reg.Active() != 0 {
		t.Errorf("counters %d %d %d", reg.Succeeded.Load(), reg.Failed.Load(), reg.Active())
	}
}

func TestRegistryByteBudget(t *testing.T) {
	reg := NewRegistry(RegistryConfig{Keep: 5, Limits: Limits{Events: 1000, Bytes: 1 << 20}, RetainedBytes: 600 << 10})
	body := logLines(300, func(i int) string { return fmt.Sprintf("ERROR %d %s", i, strings.Repeat("y", 1500)) })
	var ids []string
	for i := 0; i < 3; i++ {
		im, err := reg.Begin("", fmt.Sprint(i), start)
		if err != nil {
			t.Fatal(err)
		}
		Run(context.Background(), im, strings.NewReader(body), Options{Source: src()}, Deps{Classify: engine().Classify})
		reg.Done(im)
		ids = append(ids, im.ID())
	}
	var total int64
	for _, rep := range reg.List() {
		total += rep.RetainedBytes
	}
	if total > 600<<10 {
		t.Errorf("registry retains %d bytes", total)
	}
	oldest, _ := reg.Get(ids[0])
	if r := oldest.Report(false); !r.EventsEvicted || r.Events != 300 {
		t.Errorf("oldest import should lose events but keep its report: %+v", r)
	}
	newest, _ := reg.Get(ids[2])
	if newest.Report(false).EventsEvicted {
		t.Error("newest import should keep its events")
	}
}

func TestRetainedStringsOwnTheirBytes(t *testing.T) {
	big := strings.Repeat("e", 1<<20)
	ev := model.Event{Message: big[:20], Level: big[:5]}
	c := compact(ev)
	for name, s := range map[string]string{"message": c.Message, "level": c.Level} {
		if unsafe.StringData(s) == unsafe.StringData(big) {
			t.Errorf("compacted %s still points into the original record", name)
		}
	}
	im := New("x", DefaultLimits, start)
	pe := model.Event{Message: big[:30] + "\n" + big[30:], Fingerprint: "f", Severity: model.SeverityError}
	im.add(&pe, true, nil)
	if p := im.patterns["f"]; unsafe.StringData(p.Sample) == unsafe.StringData(pe.Message) {
		t.Error("pattern sample pins the full message")
	}

	// Levels and numbers count toward the budget.
	huge := strings.Repeat("9", 100<<10)
	if n := cost(&model.Event{Level: huge}); n < 100<<10 {
		t.Errorf("level not counted: %d", n)
	}
	if n := cost(&model.Event{Fields: map[string]any{"n": json.Number(huge)}}); n < 100<<10 {
		t.Errorf("json.Number not counted: %d", n)
	}
}

func TestOutputErrorDoesNotReplayEvents(t *testing.T) {
	var calls int
	im, err := runImport(t, strings.NewReader(logLines(1000, func(i int) string { return fmt.Sprintf("2026-10-11T10:00:00Z INFO line %d", i) })), Limits{}, Options{}, Deps{
		OnEvent: func(model.Event) error {
			if calls++; calls == 50 {
				return errors.New("disk full")
			}
			return nil
		},
	})
	r := im.Report(false)
	if err != nil || calls != 50 || r.Events != 50 || r.StopReason != StopOutput || !strings.Contains(r.Error, "disk full") {
		t.Errorf("err=%v calls=%d events=%d reason=%s error=%q", err, calls, r.Events, r.StopReason, r.Error)
	}
}

func TestWorkerPanicLeavesTerminalState(t *testing.T) {
	im := New("x", DefaultLimits, start)
	input := logLines(600, func(i int) string { return fmt.Sprint("line ", i) })
	err := Run(context.Background(), im, strings.NewReader(input), Options{Source: src(), Workers: 4}, Deps{Classify: func(*model.Event) bool { panic("boom") }})
	if r := im.Report(false); err == nil || r.Status != StatusFailed || r.StopReason != StopInternal || !strings.Contains(r.Error, "boom") {
		t.Errorf("worker panic: %v %+v", err, r)
	}
}

func TestTruncatedGzipJSONIsReportedAsTruncated(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("[\n")
	for i := 0; i < 2000; i++ {
		if i > 0 {
			doc.WriteString(",\n")
		}
		fmt.Fprintf(&doc, `{"level":"error","message":"request %d failed","trace":"%064d"}`, i, i*7919)
	}
	doc.WriteString("\n]\n")
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(doc.String()))
	w.Close()
	cut := gz.Bytes()[:gz.Len()/2]
	im, err := runImport(t, bytes.NewReader(cut), Limits{}, Options{}, Deps{})
	if r := im.Report(false); err != nil || r.StopReason != StopGzipTruncated || r.Events == 0 {
		t.Errorf("err=%v reason=%s events=%d error=%q", err, r.StopReason, r.Events, r.Error)
	}
}

func TestInputExactlyAtTheLimit(t *testing.T) {
	input := logLines(1000, func(i int) string { return fmt.Sprintf("2026-10-11T10:00:00Z INFO line %d", i) })
	for _, max := range []int64{int64(len(input)), int64(len(input)) + 1} {
		im, err := runImport(t, strings.NewReader(input), Limits{}, Options{MaxBytes: max}, Deps{})
		if r := im.Report(false); err != nil || r.Truncated || r.Events != 1000 {
			t.Errorf("max=%d: err=%v truncated=%v reason=%s events=%d", max, err, r.Truncated, r.StopReason, r.Events)
		}
	}
	im, _ := runImport(t, strings.NewReader(input), Limits{}, Options{MaxBytes: int64(len(input)) - 1}, Deps{})
	if r := im.Report(false); !r.Truncated || r.StopReason != StopDecompressedLimit {
		t.Errorf("one byte over: %+v", r)
	}
	// Tiny input exactly at the limit.
	im, _ = runImport(t, strings.NewReader("tiny\n"), Limits{}, Options{MaxBytes: 5}, Deps{})
	if r := im.Report(false); r.Truncated || r.Events != 1 {
		t.Errorf("tiny: truncated=%v events=%d", r.Truncated, r.Events)
	}
}

func TestTimelineKeepsContiguousTail(t *testing.T) {
	agg := newTimeAgg()
	base := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < 10000; i++ {
		agg.add(base+int64(i%60), 2)
	}
	for m := 1; m <= 40; m++ {
		agg.add(base+int64(m)*60, 5) // a crash loop's tail, once a minute
	}
	agg.add(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), 5)
	tl := agg.snapshot()
	if tl.After != 0 || tl.Before != 1 || !tl.End.After(time.Unix(base+40*60, 0)) {
		t.Errorf("tail cut: before=%d after=%d end=%v", tl.Before, tl.After, tl.End)
	}
}
