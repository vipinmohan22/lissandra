package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine"
	"github.com/vipinmohan22/lissandra/internal/importer"
	"github.com/vipinmohan22/lissandra/internal/ingest"
	"github.com/vipinmohan22/lissandra/internal/model"
	"github.com/vipinmohan22/lissandra/internal/notify"
)

const analyzeUsage = `Usage: lissandra analyze [flags] FILE...

Parse and classify log files locally with the same parsing and rules as the
server, and print a report. FILE may be "-" for standard input; gzip files are
detected automatically. Nothing is stored or sent anywhere: no notifications,
and no network access unless -engine is given.

With -engine URL, the Laya engine (laya-serve) classifies what the rules
leave, once per message pattern, using the config's categories and engine
settings; only the log text of those patterns is sent to that URL.

Rules come from -config, else $LISSANDRA_CONFIG, else
$LISSANDRA_DATA_DIR/config.json (./data/config.json) if it exists, else the
built-in defaults. A config exported from Settings → Backup works too.

Flags:
`

// runAnalyze implements "lissandra analyze". It returns the process exit
// code: 0 on success, 1 on errors, 2 when -fail-on matched an event.
func runAnalyze(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// The command runs as its user, on their files: -engine-key
	// ${file:~/key} may name any of them.
	config.AllowFileRefsAnywhere()
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, analyzeUsage)
		fs.PrintDefaults()
	}
	cfgPath := fs.String("config", "", "Lissandra config.json with rules, routes and sources")
	sourceKey := fs.String("source", "", "name or id of a source in the config whose parse settings and tags to use")
	format := fs.String("format", "", "parse format: auto, text, ndjson, json or csv (default: the source's, else auto)")
	tz := fs.String("tz", "UTC", "time zone for timestamps that don't include one, e.g. Europe/Berlin or Local")
	output := fs.String("output", "text", "text (summary), json (one report per file) or ndjson (every classified event)")
	minSev := fs.String("min-severity", "", "with -output ndjson, only print events at or above this severity")
	failOn := fs.String("fail-on", "", "exit with status 2 if any event is at or above this severity")
	engineURL := fs.String("engine", "", "laya-serve URL(s), comma-separated: classify what the rules leave with the Laya engine")
	engineModel := fs.String("engine-model", "", "engine checkpoint (default: the config's, else english)")
	engineKey := fs.String("engine-key", "", "engine API key (default $LISSANDRA_ENGINE_API_KEY)")
	files, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if len(files) == 0 {
		fs.Usage()
		return 1
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "lissandra analyze: "+format+"\n", a...)
		return 1
	}
	switch *output {
	case "text", "json", "ndjson":
	default:
		return fail("unknown -output %q", *output)
	}
	switch *format {
	case "", config.FormatAuto, config.FormatText, config.FormatNDJSON, config.FormatJSON, ingest.FormatCSV:
	default:
		return fail("unknown -format %q", *format)
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return fail("unknown -tz %q", *tz)
	}
	var min, failAt model.Severity
	for _, f := range []struct {
		val  *string
		dst  *model.Severity
		name string
	}{{minSev, &min, "min-severity"}, {failOn, &failAt, "fail-on"}} {
		if *f.val == "" {
			continue
		}
		s, ok := model.ParseSeverity(*f.val)
		if !ok {
			return fail("unknown -%s %q", f.name, *f.val)
		}
		*f.dst = s
	}

	cfg, origin, err := loadAnalyzeConfig(*cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Fprintf(stderr, "rules: %s (%d rules, %d routes)\n", origin, len(cfg.Rules), len(cfg.Routes))
	var source *config.Source
	if *sourceKey != "" {
		for i := range cfg.Sources {
			if cfg.Sources[i].ID == *sourceKey || cfg.Sources[i].Name == *sourceKey {
				source = &cfg.Sources[i]
				break
			}
		}
		if source == nil {
			return fail("no source %q in the config", *sourceKey)
		}
	}
	// Only rules, route filters and (with -engine) the Laya engine are used;
	// notifiers and pollers are never constructed.
	rules := classify.New(cfg.Rules)
	routes := notify.RouteMatcher(cfg)
	classifyFn := rules.Classify
	workers := runtime.GOMAXPROCS(0)
	if *engineURL != "" {
		// In order, so the engine is asked about each pattern's first line
		// and runs give the same answers (the engine is the bottleneck).
		workers = 1
		laya, err := analyzeEngine(cfg, source, *engineURL, *engineModel, *engineKey, stderr)
		if err != nil {
			return fail("%v", err)
		}
		classifyFn = func(ev *model.Event) bool {
			if !rules.Classify(ev) {
				return false
			}
			if laya.Eligible(ev) {
				ctx, cancel := context.WithTimeout(context.Background(), laya.MaxWait())
				laya.Enrich(ctx, ev)
				cancel()
			}
			return true
		}
	}

	out := bufio.NewWriter(stdout)
	defer out.Flush()
	enc := json.NewEncoder(jsonSafeWriter{out})
	enc.SetEscapeHTML(false)

	code := 0
	for _, path := range files {
		rep, err := analyzeOne(path, stdin, source, *format, loc, classifyFn, routes, *output, min, enc, workers)
		if err != nil {
			fmt.Fprintf(stderr, "lissandra analyze: %s: %s\n", terminalSafe(path), terminalSafe(err.Error()))
			code = 1
			if rep == nil {
				continue
			}
		}
		switch *output {
		case "json":
			if err := enc.Encode(rep); err != nil {
				return fail("%v", err)
			}
		case "text":
			writeTextReport(out, *rep)
		}
		if failAt != "" && code == 0 && maxSeverity(*rep).AtLeast(failAt) {
			code = 2
		}
	}
	return code
}

// analyzeEngine connects to the Laya engine for -engine and checks that the
// checkpoint is loaded before any file is read.
func analyzeEngine(cfg *config.Config, source *config.Source, urls, checkpoint, key string, stderr io.Writer) (*engine.Classifier, error) {
	e := &cfg.Settings.Engine
	e.Enabled, e.URL = true, urls
	e.APIKey = key
	if key == "" {
		e.APIKey = "${env:LISSANDRA_ENGINE_API_KEY}"
	}
	if checkpoint != "" {
		e.Model = checkpoint
	}
	if err := config.Normalize(cfg); err != nil {
		return nil, fmt.Errorf("-engine: %w", err)
	}
	laya := engine.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	laya.Configure(cfg)
	laya.CheckHealth() // know the engine's checkpoints before the first events
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	eps, err := laya.Probe(ctx)
	if err != nil {
		return nil, fmt.Errorf("-engine: %w", err)
	}
	for _, ep := range eps {
		switch {
		case !ep.OK || ep.Error != "":
			return nil, fmt.Errorf("-engine: %s: %s", ep.URL, ep.Error)
		case !ep.ModelLoaded:
			return nil, fmt.Errorf("-engine: %s has not loaded checkpoint %q (loaded: %s)", ep.URL, e.Model, strings.Join(ep.Loaded, ", "))
		case source != nil && source.EngineModel != "" && slices.Contains(ep.Missing, source.EngineModel):
			// The file is read as this source: its checkpoint is the one used.
			return nil, fmt.Errorf("-engine: %s has not loaded checkpoint %q, which source %q uses (loaded: %s)", ep.URL, source.EngineModel, source.Name, strings.Join(ep.Loaded, ", "))
		case len(ep.Missing) > 0:
			fmt.Fprintf(stderr, "engine: %s has not loaded %s, used by some sources; their events keep the rules' result\n", ep.URL, strings.Join(ep.Missing, ", "))
		}
	}
	fmt.Fprintf(stderr, "engine: %s, checkpoint %s, %d categories\n", strings.Join(laya.Status().URLs, ", "), e.Model, len(cfg.Settings.Categories))
	return laya, nil
}

// parseInterspersed parses flags that may appear before, between or after
// file arguments ("--" ends flag parsing).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var files []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return files, nil
		}
		// fs.Parse consumes a "--" terminator; if the arg before the
		// remaining ones was "--", everything left is a file.
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(files, rest...), nil
		}
		files = append(files, rest[0])
		args = rest[1:]
	}
}

// loadAnalyzeConfig finds the rules to use and says where they came from.
func loadAnalyzeConfig(flagPath string) (*config.Config, string, error) {
	candidates := []struct {
		path     string
		explicit bool
	}{{flagPath, true}, {os.Getenv("LISSANDRA_CONFIG"), true}, {filepath.Join(env("LISSANDRA_DATA_DIR", "./data"), "config.json"), false}}
	for _, c := range candidates {
		if c.path == "" {
			continue
		}
		if !c.explicit {
			if _, err := os.Stat(c.path); err != nil {
				continue
			}
		}
		cfg, err := config.Load(c.path)
		if err != nil {
			return nil, "", err
		}
		return cfg, c.path, nil
	}
	cfg := config.Defaults()
	if err := config.Normalize(cfg); err != nil {
		return nil, "", err
	}
	return cfg, "built-in defaults", nil
}

// analyzeOne analyzes one file. The report is returned even when the file
// was only partly read; it is nil if the file couldn't be opened.
func analyzeOne(path string, stdin io.Reader, source *config.Source, format string, loc *time.Location,
	classifyFn func(*model.Event) bool, routes func(*model.Event) []string, output string, min model.Severity, enc *json.Encoder, workers int) (*importer.Report, error) {
	name := filepath.Base(path)
	var r io.Reader
	if path == "-" {
		name, r = "stdin", stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	src := config.Source{Name: name, Type: config.SourcePush, Parse: config.Parse{Format: config.FormatAuto}}
	if source != nil {
		src = *source
	}
	im := importer.New(name, importer.Limits{}, time.Now()) // streaming only: retain nothing
	deps := importer.Deps{Classify: classifyFn, Routes: routes}
	if output == "ndjson" {
		deps.OnEvent = func(ev model.Event) error {
			if min != "" && !ev.Severity.AtLeast(min) {
				return nil
			}
			return enc.Encode(ev)
		}
	}
	err := importer.Run(context.Background(), im, r, importer.Options{
		FileName:       name,
		Source:         src,
		Format:         format,
		Location:       loc,
		MaxRecordBytes: -1,
		Workers:        workers,
	}, deps)
	rep := im.Report(true)
	if err == nil && rep.Truncated {
		err = errors.New(rep.Error)
	}
	return &rep, err
}

func maxSeverity(rep importer.Report) model.Severity {
	var max model.Severity
	for sev, n := range rep.BySeverity {
		if n > 0 && sev.Rank() > max.Rank() {
			max = sev
		}
	}
	return max
}

// terminalSafe escapes control and bidi-formatting characters, so hostile
// log content can't drive the terminal (OSC sequences, hidden text).
func terminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '�'
		}
		return r
	}, s)
}

// jsonSafeWriter escapes characters that encoding/json passes through but a
// terminal acts on (C1 controls such as an 8-bit CSI, bidi and other format
// characters) as \uXXXX. Each Write is one whole JSON value, and these
// characters can only occur inside its strings, so the JSON is unchanged.
type jsonSafeWriter struct{ w io.Writer }

func (j jsonSafeWriter) Write(p []byte) (int, error) {
	var buf []byte
	last := 0
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRune(p[i:])
		if r >= 0x80 && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			buf = append(buf, p[last:i]...)
			for _, u := range utf16.Encode([]rune{r}) {
				buf = fmt.Appendf(buf, `\u%04x`, u)
			}
			last = i + size
		}
		i += size
	}
	if buf == nil {
		return j.w.Write(p)
	}
	if _, err := j.w.Write(append(buf, p[last:]...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// writeTextReport prints a terminal-friendly summary of rep.
func writeTextReport(w io.Writer, rep importer.Report) {
	fmt.Fprintf(w, "%s: %d lines, %d events (%d dropped by rules), %s, %s\n",
		terminalSafe(rep.FileName), rep.Lines, rep.Events, rep.Dropped, humanBytes(rep.Bytes), describeParse(rep))
	switch {
	case rep.Status == importer.StatusFailed:
		fmt.Fprintf(w, "  FAILED: %s\n", terminalSafe(rep.Error))
	case rep.Truncated:
		fmt.Fprintf(w, "  STOPPED EARLY (%s): %s — results cover the part read\n", rep.StopReason, terminalSafe(rep.Error))
	}
	ts := rep.Parse
	switch {
	case ts.TimeParsed > 0:
		fmt.Fprintf(w, "  time range  %s → %s  (%s); %d timestamped, %d inferred, %d without\n",
			rep.FirstTimestamp.In(zone(rep)).Format(time.RFC3339), rep.LastTimestamp.In(zone(rep)).Format(time.RFC3339),
			humanDuration(rep.LastTimestamp.Sub(rep.FirstTimestamp)), ts.TimeParsed, ts.TimeInferred, ts.TimeMissing)
	case rep.Events > 0:
		fmt.Fprintln(w, "  time range  — (no timestamps recognised)")
	}
	var sev []string
	for i := len(model.Severities) - 1; i >= 0; i-- {
		s := model.Severities[i]
		if n := rep.BySeverity[s]; n > 0 {
			sev = append(sev, fmt.Sprintf("%s %d", s, n))
		}
	}
	if len(sev) > 0 {
		fmt.Fprintf(w, "  severity    %s\n", strings.Join(sev, " · "))
	}
	if cats := sortedCounts(rep.ByCategory); len(cats) > 0 {
		fmt.Fprintf(w, "  categories  %s\n", terminalSafe(strings.Join(cats, " · ")))
	}
	if rep.EngineEvents > 0 {
		fmt.Fprintf(w, "  engine      answered for %d events, applied to %d\n", rep.EngineEvents, rep.EngineApplied)
	}
	if len(rep.Routes) > 0 {
		var parts []string
		for _, r := range rep.Routes {
			parts = append(parts, fmt.Sprintf("%s %d events / %d patterns", r.Name, r.Events, r.Patterns))
		}
		fmt.Fprintf(w, "  would alert %s\n", terminalSafe(strings.Join(parts, " · ")))
	}
	if len(rep.TopPatterns) > 0 {
		fmt.Fprintln(w, "  top issues")
		for _, p := range rep.TopPatterns {
			line := ""
			if p.FirstLine > 0 {
				line = fmt.Sprintf("line %d", p.FirstLine)
			}
			fmt.Fprintf(w, "    %-8s %7d×  %-13s %-11s %s\n", strings.ToUpper(string(p.Severity)), p.Count,
				terminalSafe(p.Category), line, terminalSafe(model.Truncate(p.Sample, 100)))
		}
	}
	fmt.Fprintln(w)
}

func zone(rep importer.Report) *time.Location {
	if loc, err := time.LoadLocation(rep.TimeZone); err == nil {
		return loc
	}
	return time.UTC
}

func describeParse(rep importer.Report) string {
	if rep.Parse.Mode == "" {
		return "not parsed"
	}
	parts := []string{rep.Parse.Mode}
	if rep.Compressed {
		parts = append(parts, "gzip")
	}
	if rep.Parse.Encoding != "" && rep.Parse.Encoding != "utf-8" {
		parts = append(parts, rep.Parse.Encoding)
	}
	return "parsed as " + strings.Join(parts, ", ")
}

func sortedCounts(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return out
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1f days", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1f hours", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f minutes", d.Minutes())
	}
	return d.Round(time.Second).String()
}
