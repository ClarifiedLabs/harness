package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
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

	status, err := ProbeActivity(dir)
	if err != nil || status != ActivityActive {
		_ = stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("ProbeActivity active helper = %q, %v; want active", status, err)
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
	status, err = ProbeActivity(dir)
	if err != nil || status != ActivityInactive {
		t.Fatalf("ProbeActivity after helper exit = %q, %v; want inactive", status, err)
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

	status, err := ProbeActivity(dir)
	if err != nil || status != ActivityActive {
		t.Fatalf("ProbeActivity same-process lock = %q, %v; want active", status, err)
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
	status, err = ProbeActivity(dir)
	if err != nil || status != ActivityInactive {
		t.Fatalf("ProbeActivity after close = %q, %v; want inactive", status, err)
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

func TestProbeActivityMissingLockRemainsMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	status, err := ProbeActivity(dir)
	if err != nil || status != ActivityInactive {
		t.Fatalf("ProbeActivity missing = %q, %v; want inactive", status, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, lockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing lock was created: %v", err)
	}
}

func TestProbeActivityDoesNotChangeUnlockedLockMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lockFile)
	content := []byte("999999\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	status, err := ProbeActivity(dir)
	if err != nil || status != ActivityInactive {
		t.Fatalf("ProbeActivity stale lock = %q, %v; want inactive", status, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("probe changed lock: content %q/%q mode %v/%v size %d/%d mtime %v/%v", content, got, before.Mode(), after.Mode(), before.Size(), after.Size(), before.ModTime(), after.ModTime())
	}
}

func TestProbeActivityRejectsSymlinkAndNonRegularLockPaths(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, lockFile), 0o755); err != nil {
			t.Fatal(err)
		}
		status, err := ProbeActivity(dir)
		if err == nil || status != ActivityUnknown {
			t.Fatalf("ProbeActivity directory = %q, %v; want unknown error", status, err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink privileges vary on Windows")
		}
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		content := []byte("do not touch")
		if err := os.WriteFile(target, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, lockFile)); err != nil {
			t.Fatal(err)
		}
		status, err := ProbeActivity(dir)
		if err == nil || status != ActivityUnknown {
			t.Fatalf("ProbeActivity symlink = %q, %v; want unknown error", status, err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != string(content) {
			t.Fatalf("symlink target changed: %q, %v", got, err)
		}
	})
}
