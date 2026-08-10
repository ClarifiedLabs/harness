package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const lockFile = "session.lock"

// ErrLocked means another process owns the session's advisory lock.
var ErrLocked = errors.New("session is active")

// LockedError reports the session and, when available, the process holding it.
type LockedError struct {
	Dir string
	PID int
}

func (e *LockedError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("session: %s is active in process %d", e.Dir, e.PID)
	}
	return fmt.Sprintf("session: %s is active in another process", e.Dir)
}

func (e *LockedError) Unwrap() error { return ErrLocked }

// Lock is exclusive process ownership of a session directory. The lock file is
// intentionally persistent: unlinking it would allow a new process to lock a
// different inode while the old inode remains locked. Close releases ownership.
type Lock struct {
	file *os.File
}

// AcquireLock takes a non-blocking exclusive lock for dir. The kernel lock is
// authoritative; the PID stored in session.lock is diagnostic metadata only.
func AcquireLock(dir string) (*Lock, error) {
	if dir == "" {
		return nil, errors.New("session: lock path is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create lock dir: %w", err)
	}
	path := filepath.Join(dir, lockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: open lock: %w", err)
	}
	if err := lockSessionFile(f); err != nil {
		pid := lockOwnerPID(f)
		_ = f.Close()
		if errors.Is(err, errLockHeld) {
			return nil, &LockedError{Dir: dir, PID: pid}
		}
		return nil, fmt.Errorf("session: lock %s: %w", dir, err)
	}
	locked := &Lock{file: f}
	if err := writeLockOwner(f); err != nil {
		_ = locked.Close()
		return nil, err
	}
	return locked, nil
}

func writeLockOwner(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("session: truncate lock metadata: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("session: seek lock metadata: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		return fmt.Errorf("session: write lock metadata: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("session: sync lock metadata: %w", err)
	}
	return nil
}

func lockOwnerPID(f *os.File) int {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// Close releases the lock. The lock file and last-owner PID remain for stable
// inode locking and diagnostics; they do not indicate that the session is active.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	unlockErr := unlockSessionFile(f)
	closeErr := f.Close()
	return errors.Join(unlockErr, closeErr)
}
