//go:build darwin || linux

package inputimage

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLoadRejectsFIFOAndDevicePromptly(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "image.png")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if _, err := Load(Attachment{Path: fifo}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("Load FIFO error = %v", err)
	}

	if _, err := os.Stat("/dev/null"); err == nil {
		if _, err := Load(Attachment{Path: "/dev/null"}); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("Load device error = %v", err)
		}
	}
}
