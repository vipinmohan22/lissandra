package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/importer"
	"github.com/vipinmohan22/layalogs/internal/model"
)

const cliLog = "2026-10-11 10:00:00 INFO started\n" +
	"2026-10-11 10:00:05 ERROR payment failed: connection refused\n" +
	"java.lang.IllegalStateException: boom\n" +
	"\tat com.acme.Pay.charge(Pay.java:42)\n" +
	"2026-10-11 10:00:06 WARN login failed for user bob\n"

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func analyze(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("LAYA_CONFIG", "")
	t.Setenv("LAYA_DATA_DIR", t.TempDir())
	var out, errb bytes.Buffer
	code := runAnalyze(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestAnalyzeTextAndExitCodes(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "app.log", cliLog)
	code, out, errOut := analyze(t, "", f)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"app.log: 5 lines, 3 events", "text-anchored", "severity    error 1 · warning 1 · info 1", "line 2", "rules: built-in defaults"} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("output missing %q:\n%s%s", want, out, errOut)
		}
	}
	if code, _, _ := analyze(t, "", "-fail-on", "error", f); code != 2 {
		t.Errorf("-fail-on error: exit %d", code)
	}
	if code, _, _ := analyze(t, "", "-fail-on", "critical", f); code != 0 {
		t.Errorf("-fail-on critical: exit %d", code)
	}
	if code, _, _ := analyze(t, "", filepath.Join(dir, "missing.log")); code != 1 {
		t.Errorf("missing file: exit %d", code)
	}
	if code, _, _ := analyze(t, "", "-output", "yaml", f); code != 1 {
		t.Errorf("bad flag value: exit %d", code)
	}
	if code, _, _ := analyze(t, ""); code != 1 {
		t.Errorf("no files: exit %d", code)
	}
}

func TestAnalyzeFlagsAfterFilesAndStdin(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "app.log", cliLog)
	code, out, errOut := analyze(t, "", f, "-output", "json", "-tz", "Asia/Kolkata")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var rep importer.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json output: %v\n%s", err, out)
	}
	if rep.TimeZone != "Asia/Kolkata" || rep.FirstTimestamp.UTC().Hour() != 4 { // 10:00 IST = 04:30 UTC
		t.Errorf("tz not applied: %s %v", rep.TimeZone, rep.FirstTimestamp)
	}
	if rep.Retained != 0 {
		t.Error("the CLI should retain no events")
	}

	// "--" makes everything after it a file, even names starting with "-".
	odd := writeFile(t, dir, "-weird.log", "x\n")
	if code, out, _ := analyze(t, "", "--", odd); code != 0 || !strings.Contains(out, "-weird.log") {
		t.Errorf("-- handling: %d %s", code, out)
	}

	code, out, _ = analyze(t, cliLog, "-output", "ndjson", "-min-severity", "warning", "-")
	if code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("ndjson lines: %q", lines)
	}
	var ev model.Event
	json.Unmarshal([]byte(lines[0]), &ev)
	if ev.Severity != model.SeverityError || ev.File != "stdin" || ev.Line != 2 || !strings.Contains(ev.Message, "IllegalStateException") {
		t.Errorf("ndjson event %+v", ev)
	}
}

func TestAnalyzeUsesConfigAndNeverCallsOut(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()

	c := config.Defaults()
	c.Rules = append([]config.Rule{{
		Name: "Payments", Enabled: true, Match: config.MatchAll, Category: "payments",
		Conditions: []config.Condition{{Field: "message", Op: config.OpContains, Value: "payment"}},
	}}, c.Rules...)
	c.Notifiers = []config.Notifier{
		{ID: "n1", Name: "hook", Type: "webhook", Enabled: true, Settings: map[string]string{"url": srv.URL}},
		{ID: "n2", Name: "slack", Type: "slack", Enabled: true, Settings: map[string]string{"webhook_url": srv.URL}},
	}
	c.Routes = []config.Route{{ID: "r1", Name: "errors", Enabled: true, MinSeverity: model.SeverityError, NotifierIDs: []string{"n1", "n2"}}}
	c.Sources = append(c.Sources, config.Source{Name: "pull", Type: config.SourcePull, Enabled: true, Pull: &config.Pull{URL: srv.URL, IntervalSeconds: 5}})
	c.Settings.AI = config.AISettings{Enabled: true, APIKey: "sk-test", OnlyUncategorized: false, MinSeverity: model.SeverityDebug}
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(c)
	dir := t.TempDir()
	cfgPath := writeFile(t, dir, "config.json", string(data))
	f := writeFile(t, dir, "app.log", cliLog)

	code, out, errOut := analyze(t, "", "-config", cfgPath, f)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "payments 1") || !strings.Contains(out, "would alert errors 1 events / 1 patterns") {
		t.Errorf("config rules/routes not used:\n%s", out)
	}
	if !strings.Contains(errOut, "rules: "+cfgPath) {
		t.Errorf("config origin not reported: %s", errOut)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("analyze made %d network calls", n)
	}

	// $LAYA_DATA_DIR/config.json is picked up automatically.
	t.Setenv("LAYA_CONFIG", "")
	dataDir := t.TempDir()
	writeFile(t, dataDir, "config.json", string(data))
	t.Setenv("LAYA_DATA_DIR", dataDir)
	var o, e bytes.Buffer
	runAnalyze([]string{f}, strings.NewReader(""), &o, &e)
	if !strings.Contains(e.String(), filepath.Join(dataDir, "config.json")) {
		t.Errorf("data dir config not used: %s", e.String())
	}

	if code, _, _ := analyze(t, "", "-source", "nope", f); code != 1 {
		t.Errorf("unknown source: exit %d", code)
	}
}

func TestAnalyzeTerminalSafe(t *testing.T) {
	dir := t.TempDir()
	evil := "2026-10-11 10:00:00 ERROR \x1b]52;c;ZXZpbA==\x07clipboard \x1b[2J\u202ehidden\n"
	f := writeFile(t, dir, "evil\x1b[31m.log", evil)
	_, out, errOut := analyze(t, "", f)
	for _, bad := range []string{"\x1b", "\x07", "\u202e"} {
		if strings.Contains(out+errOut, bad) {
			t.Errorf("control sequence %q reached the terminal:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "clipboard") {
		t.Errorf("message text lost: %q", out)
	}

	// JSON output escapes what encoding/json passes through: C1 controls
	// (an 8-bit CSI) and bidi characters. The decoded text is unchanged.
	hidden := "2026-10-11 10:00:00 ERROR a\u202eb\u2066c\u009b2Jd\U000e0041e\n"
	f = writeFile(t, dir, "hidden.log", hidden)
	for _, mode := range []string{"json", "ndjson"} {
		_, out, _ := analyze(t, "", "-output", mode, f)
		for _, bad := range []string{"\u202e", "\u2066", "\u009b", "\U000e0041"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s output contains raw %q", mode, bad)
			}
		}
		if mode == "ndjson" {
			var ev model.Event
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ev); err != nil || !strings.HasSuffix(ev.Message, "a\u202eb\u2066c\u009b2Jd\U000e0041e") {
				t.Errorf("escaped JSON does not decode to the message: %v %q", err, ev.Message)
			}
		}
	}
}

func TestParseInterspersed(t *testing.T) {
	cases := []struct {
		args  []string
		files string
	}{
		{[]string{"a", "-output", "json", "b"}, "a,b"},
		{[]string{"-output", "json", "a"}, "a"},
		{[]string{"a", "--", "-b", "-output"}, "a,-b,-output"},
		{[]string{"-", "-tz", "UTC"}, "-"},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("output", "", "")
		fs.String("tz", "", "")
		files, err := parseInterspersed(fs, c.args)
		if err != nil || strings.Join(files, ",") != c.files {
			t.Errorf("%q: %v %q", c.args, err, files)
		}
	}
}
