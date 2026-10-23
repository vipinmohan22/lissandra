package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminTokenStartupLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-token.json")
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	admin, err := openAdmin(log, path, "")
	if err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	if !strings.Contains(logged, `"admin_token":"`) || !strings.Contains(logged, "setup wizard") {
		t.Fatalf("generated: %s", logged)
	}
	// The token Lissandra logged signs in, and the wizard replaces it.
	var line struct {
		Token string `json:"admin_token"`
	}
	if err := json.Unmarshal([]byte(logged), &line); err != nil || !admin.Check(line.Token) {
		t.Fatalf("the logged token doesn't sign in: %v", err)
	}
	next := strings.Repeat("0123456789abcdef", 3)
	if _, err := admin.Rotate(line.Token, next); err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	if _, err := openAdmin(log, path, ""); err != nil {
		t.Fatal(err)
	}
	if logged := buf.String(); strings.Contains(logged, "admin_token") || strings.Contains(logged, next) || !strings.Contains(logged, "admin token: set in Lissandra") {
		t.Errorf("stored: %s", logged)
	}

	buf.Reset()
	if _, err := openAdmin(log, path, "env-0x7f3a"); err != nil {
		t.Fatal(err)
	}
	if logged := buf.String(); !strings.Contains(logged, "shorter than 16") || !strings.Contains(logged, "is ignored while LISSANDRA_ADMIN_TOKEN is set") || strings.Contains(logged, "env-0x7f3a") {
		t.Errorf("env with a stored token: %s", logged)
	}
}
