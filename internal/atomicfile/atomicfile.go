// Package atomicfile replaces files through a temporary file in the same directory.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Replace writes a temporary file and renames it over path only after write and
// Close succeed. The caller owns directory creation, permissions, and syncing;
// write must not close the file. Temporary files are removed on every exit path.
func Replace(path, pattern string, write func(*os.File) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer os.Remove(f.Name())
	if err := write(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	return nil
}
