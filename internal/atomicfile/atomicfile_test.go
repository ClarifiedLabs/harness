package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReplacePublishesOnlyAfterSuccessfulWrite(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state")
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			writeErr := errors.New("write failed")
			var temp *os.File
			err := Replace(path, ".state-*", func(f *os.File) error {
				temp = f
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "old" {
					t.Fatalf("destination changed before publication: %q, %v", data, err)
				}
				if _, err := f.WriteString("new"); err != nil {
					return err
				}
				if fail {
					return writeErr
				}
				return f.Sync()
			})
			want := "new"
			if fail {
				want = "old"
				if !errors.Is(err, writeErr) {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatalf("destination = %q, %v; want %q", data, err, want)
			}
			if _, err := temp.WriteString("closed"); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("temporary file left open: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "state" {
				t.Fatalf("temporary file left behind: %v, %v", entries, err)
			}
		})
	}
}

func TestReplaceRenameFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "destination")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	var temp *os.File
	err := Replace(path, ".state-*", func(f *os.File) error {
		temp = f
		_, err := f.WriteString("new")
		return err
	})
	var pathErr *os.LinkError
	if !errors.As(err, &pathErr) {
		t.Fatalf("rename error = %v", err)
	}
	if _, err := os.Stat(temp.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left behind: %v", err)
	}
	if _, err := temp.WriteString("closed"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("temporary file left open: %v", err)
	}
}
