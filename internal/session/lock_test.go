package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSessionLockHelperProcess(t *testing.T) {
	dir := os.Getenv("HARNESS_SESSION_LOCK_HELPER")
	if dir == "" {
		return
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer lock.Close()
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestAcquireLockAcrossProcessesAndRecoversAfterExit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionLockHelperProcess$")
	cmd.Env = append(os.Environ(), "HARNESS_SESSION_LOCK_HELPER="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		_ = stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("helper ready = %q, %v", line, err)
	}

	_, err = AcquireLock(dir)
	if !errors.Is(err, ErrLocked) {
		_ = stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("AcquireLock active helper error = %v, want ErrLocked", err)
	}
	var locked *LockedError
	if !errors.As(err, &locked) || locked.PID != cmd.Process.Pid {
		_ = stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("AcquireLock active helper error = %#v, want owner PID %d", err, cmd.Process.Pid)
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("close helper stdin: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait helper: %v", err)
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after helper exit: %v", err)
	}
	defer lock.Close()
}

func TestAcquireLockRejectsActiveSessionAndRecoversAfterClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock first: %v", err)
	}

	_, err = AcquireLock(dir)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("AcquireLock active error = %v, want ErrLocked", err)
	}
	var locked *LockedError
	if !errors.As(err, &locked) || locked.PID != os.Getpid() {
		t.Fatalf("AcquireLock active error = %#v, want owner PID %d", err, os.Getpid())
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close first lock: %v", err)
	}
	second, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after close: %v", err)
	}
	defer second.Close()

	data, err := os.ReadFile(filepath.Join(dir, lockFile))
	if err != nil {
		t.Fatalf("read lock metadata: %v", err)
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock metadata = %q, want PID %d", data, os.Getpid())
	}
}
