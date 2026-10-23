package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const (
	envToken = "env-token-0123456789abcdef"
	newToken = "3f9c1a7be04d52c8a6e1f0b9d7c3e5a2b4d6f8091a2c3e4f"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func capture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// stored writes admin-token.json for newToken and returns its path.
func stored(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin-token.json")
	s, gen, err := Open(path, "", discard())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rotate(gen, newToken); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenPrecedence(t *testing.T) {
	s, gen, err := Open(filepath.Join(t.TempDir(), "admin-token.json"), envToken, discard())
	if err != nil || gen != "" {
		t.Fatal(gen, err)
	}
	if st := s.Status(); st.Source != SourceEnv || st.Changeable || !st.Persistent || st.StoredIgnored || !s.Check(envToken) {
		t.Errorf("env alone: %+v", st)
	}

	path := stored(t)
	s, gen, err = Open(path, envToken, discard())
	if err != nil || gen != "" {
		t.Fatal(gen, err)
	}
	st := s.Status()
	if st.Source != SourceEnv || !st.StoredIgnored || st.StoredSetAt == nil || st.SetAt != nil || st.StoredError != "" {
		t.Errorf("env and a file: %+v", st)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the ignored file must be kept: %v", err)
	}

	s, gen, err = Open(path, "", discard())
	if err != nil || gen != "" {
		t.Fatal(gen, err)
	}
	if st := s.Status(); st.Source != SourceStored || !st.Changeable || !st.Persistent || st.SetAt == nil || st.File != path || !s.Check(newToken) {
		t.Errorf("a file alone: %+v", st)
	}

	s, gen, err = Open(filepath.Join(t.TempDir(), "admin-token.json"), "", discard())
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(gen) || !s.Check(gen) {
		t.Errorf("generated token %q", gen)
	}
	if st := s.Status(); st.Source != SourceGenerated || !st.Changeable || st.Persistent {
		t.Errorf("neither: %+v", st)
	}
}

func TestCheck(t *testing.T) {
	s, _, err := Open(stored(t), "", discard())
	if err != nil {
		t.Fatal(err)
	}
	for tok, want := range map[string]bool{newToken: true, newToken + "x": false, "": false, strings.Repeat("a", 257): false} {
		if got := s.Check(tok); got != want {
			t.Errorf("Check(%.20q) = %v", tok, got)
		}
	}

	long := strings.Repeat("0123456789", 30)
	if s := Static(long); !s.Check(long) || s.Check(long[:256]) {
		t.Error("an env token of 300 bytes must keep working")
	}
	env, _, err := Open(stored(t), envToken, discard())
	if err != nil {
		t.Fatal(err)
	}
	if !env.Check(envToken) || env.Check(newToken) {
		t.Error("only LISSANDRA_ADMIN_TOKEN is accepted while it is set")
	}
}

func TestValidateToken(t *testing.T) {
	bad := map[string]string{
		"31 characters":     newToken[:31],
		"whitespace":        newToken[:20] + " " + newToken[21:],
		"non-ASCII":         newToken[:20] + "é" + newToken[22:],
		"9 distinct":        strings.Repeat("012345678", 4),
		"over a quarter":    strings.Repeat("a", 12) + "bcdefghijklmnopqrstu",
		"257 bytes":         strings.Repeat(newToken, 6)[:257],
		"a control byte":    newToken[:20] + "\x01" + newToken[21:],
		"an empty token":    "",
		"a repeated phrase": strings.Repeat("ab", 20),
	}
	for name, tok := range bad {
		if err := ValidateToken(tok); !errors.Is(err, ErrWeakToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, tok := range []string{newToken, strings.Repeat("a", 8) + "bcdefghijklmnopqrstuvwxy", strings.Repeat(newToken, 6)[:256]} {
		if err := ValidateToken(tok); err != nil {
			t.Errorf("%q: %v", tok, err)
		}
	}
}

func TestRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-token.json")
	s, gen, err := Open(path, "", discard())
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Rotate(gen, newToken)
	if err != nil {
		t.Fatal(err)
	}
	if st.Source != SourceStored || st.SetAt == nil || !st.Persistent {
		t.Errorf("status = %+v", st)
	}
	if s.Check(gen) || !s.Check(newToken) {
		t.Error("the generated token must stop working at once")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte(newToken)) {
		t.Error("the file holds the token")
	}
	var f map[string]any
	if err := json.Unmarshal(data, &f); err != nil || f["version"] != 1.0 || f["scheme"] != "hmac-sha256" {
		t.Errorf("file = %s", data)
	}
	if again, _, err := Open(path, "", discard()); err != nil || !again.Check(newToken) || again.Check(gen) {
		t.Errorf("reopened: %v", err)
	}

	if _, err := s.Rotate(newToken, newToken); !errors.Is(err, ErrSameToken) {
		t.Errorf("the same token: %v", err)
	}
	// The logged token is refused for the rest of the run, not only while in use.
	if _, err := s.Rotate(newToken, gen); !errors.Is(err, ErrLoggedToken) || s.Check(gen) || !s.Check(newToken) {
		t.Errorf("the logged token after a rotation: %v", err)
	}
	if _, err := s.Rotate(newToken, "short"); !errors.Is(err, ErrWeakToken) {
		t.Errorf("a weak token: %v", err)
	}
	if _, err := s.Rotate("wrong", strings.ToUpper(newToken)); !errors.Is(err, ErrStale) {
		t.Errorf("a wrong current token: %v", err)
	}
	if _, err := Static(envToken).Rotate(envToken, newToken); !errors.Is(err, ErrEnvManaged) {
		t.Errorf("Static: %v", err)
	}
}

func TestRotateCAS(t *testing.T) {
	s, gen, err := Open(filepath.Join(t.TempDir(), "admin-token.json"), "", discard())
	if err != nil {
		t.Fatal(err)
	}
	next := []string{newToken, strings.ToUpper(newToken)}
	errs := make([]error, len(next))
	var wg sync.WaitGroup
	for i := range next {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Rotate(gen, next[i])
		}()
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = i
		case !errors.Is(err, ErrStale):
			t.Fatalf("rotation %d: %v", i, err)
		}
	}
	if winner < 0 || !s.Check(next[winner]) || s.Check(next[1-winner]) || s.Check(gen) {
		t.Errorf("results %v", errs)
	}
}

func TestRotateWriteFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, gen, err := Open(filepath.Join(dir, "admin-token.json"), "", discard())
	if err != nil {
		t.Fatal(err)
	}
	// The data directory is now a file: writing fails, even as root, which
	// a read-only directory wouldn't stop.
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rotate(gen, newToken); err == nil || strings.Contains(err.Error(), newToken) {
		t.Fatalf("err = %v", err)
	}
	if !s.Check(gen) || s.Check(newToken) || s.Status().Source != SourceGenerated {
		t.Error("a failed save must leave the old token in use")
	}
}

func TestOpenBadFile(t *testing.T) {
	path := stored(t)
	good, _ := os.ReadFile(path)
	var f map[string]any
	json.Unmarshal(good, &f)
	edit := func(k string, v any) []byte {
		m := map[string]any{}
		for k, v := range f {
			m[k] = v
		}
		m[k] = v
		b, _ := json.Marshal(m)
		return b
	}
	for name, data := range map[string][]byte{
		"corrupt JSON":    []byte(`{"version": 1,`),
		"unknown scheme":  edit("scheme", "pbkdf2-sha256"),
		"unknown version": edit("version", 2),
		"short key":       edit("key", "c2hvcnQ="),
		"short digest":    edit("digest", "c2hvcnQ="),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(path, "", discard()); err == nil || !strings.Contains(err.Error(), "fix or delete it, or set LISSANDRA_ADMIN_TOKEN") {
			t.Errorf("%s without env: %v", name, err)
		}
		log, buf := capture()
		s, _, err := Open(path, envToken, log)
		if err != nil || !s.Check(envToken) || !s.Status().StoredIgnored {
			t.Errorf("%s with env: %v", name, err)
		}
		if !strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("%s with env: no warning: %s", name, buf)
		}
		// The UI says why the file can't be used, without what it holds.
		st, _ := json.Marshal(s.Status())
		var shown struct {
			StoredError string `json:"stored_error"`
		}
		json.Unmarshal(st, &shown)
		if shown.StoredError == "" || strings.Contains(shown.StoredError, f["key"].(string)) || strings.Contains(shown.StoredError, f["digest"].(string)) {
			t.Errorf("%s with env: status %s", name, st)
		}
		if err := s.DeleteStored(); err != nil || s.Status().StoredIgnored || s.Status().StoredError != "" {
			t.Errorf("%s with env: delete: %v, %+v", name, err, s.Status())
		}
	}
}

func TestOpenLoosePermissions(t *testing.T) {
	path := stored(t)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	log, buf := capture()
	s, _, err := Open(path, "", log)
	if err != nil || !s.Check(newToken) {
		t.Fatalf("a loose mode must not stop Lissandra: %v", err)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "mode 0666") {
		t.Errorf("log = %s", buf)
	}
}

func TestDeleteStored(t *testing.T) {
	path := stored(t)
	s, _, err := Open(path, "", discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStored(); !errors.Is(err, ErrNotEnv) {
		t.Errorf("stored: %v", err)
	}
	s, _, err = Open(path, envToken, discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStored(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still there: %v", err)
	}
	if st := s.Status(); st.StoredIgnored || st.StoredSetAt != nil || !s.Check(envToken) {
		t.Errorf("status = %+v", st)
	}
	if err := s.DeleteStored(); !errors.Is(err, ErrNoStored) {
		t.Errorf("a second call: %v", err)
	}
	if err := Static(envToken).DeleteStored(); !errors.Is(err, ErrNoStored) {
		t.Errorf("Static: %v", err)
	}

	// A file removed by hand leaves nothing to delete, but a stale status
	// to clear.
	path = stored(t)
	s, _, err = Open(path, envToken, discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStored(); err != nil {
		t.Errorf("after a manual removal: %v", err)
	}
	if st := s.Status(); st.StoredIgnored || st.StoredSetAt != nil {
		t.Errorf("after a manual removal: status = %+v", st)
	}
	if err := s.DeleteStored(); !errors.Is(err, ErrNoStored) {
		t.Errorf("after a manual removal, a second call: %v", err)
	}
}
