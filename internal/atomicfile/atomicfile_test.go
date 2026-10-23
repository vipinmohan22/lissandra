package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteModesAndDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	path := filepath.Join(dir, "state.json")
	if err := Write(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "two" {
		t.Errorf("contents = %q", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", di.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
}

func TestWriteFailedRenameLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory where the file should go: the rename fails.
	path := filepath.Join(dir, "state.json")
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("data"), 0o600); err == nil {
		t.Fatal("renaming over a directory should fail")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Errorf("temporary files left: %v", entries)
	}
}
