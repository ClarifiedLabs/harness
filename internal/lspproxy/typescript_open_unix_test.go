//go:build unix

package lspproxy

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenTypeScriptPackageFIFOIsNonblocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openTypeScriptPackageFile(path)
	if err != nil {
		t.Fatalf("open FIFO nonblocking: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().IsRegular() || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("opened mode = %v, want named pipe for descriptor validation", info.Mode())
	}
}
