// Package atomicfile replaces files so that a reader, or Lissandra after a
// crash, sees either the old contents or the new ones, never a mix.
package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
)

// Write replaces path with data and sets its mode to perm. It writes a
// temporary file in the same directory, syncs it and renames it over path.
// Missing directories are created with mode 0700.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	tmp, err := os.CreateTemp(dir, "."+strings.TrimSuffix(base, ext)+"-*"+ext)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
