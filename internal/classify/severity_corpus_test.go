package classify

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vipinmohan22/layalogs/internal/model"
)

// sample is one line of testdata/severity/*.jsonl: a log line as Laya
// receives it (Level is a level field, when the record has one), the level
// it states ("" when it states none), and, for those, the severity keyword
// guessing should give (when the corpus says).
type sample struct {
	Format string `json:"format"`
	Line   string `json:"line"`
	Level  string `json:"level,omitempty"`
	Stated string `json:"stated"`
	Guess  string `json:"guess,omitempty"`
	// Gap explains a known miss: it is logged, not failed, until fixed.
	Gap string `json:"gap,omitempty"`
}

// TestSeverityCorpus checks severity detection against corpora of real log
// formats. LAYA_SEVERITY_CORPUS names more files (a glob) to check.
func TestSeverityCorpus(t *testing.T) {
	files, _ := filepath.Glob("testdata/severity/*.jsonl")
	if extra := os.Getenv("LAYA_SEVERITY_CORPUS"); extra != "" {
		more, _ := filepath.Glob(extra)
		files = append(files, more...)
	}
	if len(files) == 0 {
		t.Fatal("no corpus")
	}
	n := 0
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for ln := 1; sc.Scan(); ln++ {
			var s sample
			if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
				t.Errorf("%s:%d: %v", f, ln, err)
				continue
			}
			n++
			report := t.Errorf
			if s.Gap != "" {
				report = func(format string, args ...any) { t.Logf("known gap ("+s.Gap+"): "+format, args...) }
			}
			got, stated := detectSeverity(s.Level, s.Line)
			switch {
			case s.Stated != "":
				want, ok := model.ParseSeverity(s.Stated)
				if !ok {
					t.Errorf("%s:%d: bad stated level %q", f, ln, s.Stated)
				} else if got != want || !stated {
					report("%s:%d [%s] states %s, detected %s (stated=%v): %q", f, ln, s.Format, want, got, stated, s.Line)
				}
			case stated:
				report("%s:%d [%s] states no level, detected a stated %s: %q", f, ln, s.Format, got, s.Line)
			case s.Guess != "" && string(got) != s.Guess:
				report("%s:%d [%s] guessed %s, want %s: %q", f, ln, s.Format, got, s.Guess, s.Line)
			}
		}
		fh.Close()
	}
	t.Logf("%d samples", n)
}
