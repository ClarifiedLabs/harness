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

var (
	// ErrLocked means another process owns the session's advisory lock.
	ErrLocked   = errors.New("session is active")
	errLockHeld = errors.New("session lock held")
)

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

// ProbeActivity reports whether dir's existing kernel lock is held without
// creating the lock file or changing its diagnostic metadata. The result is a
// point-in-time snapshot; callers that resume a session must still AcquireLock.
func ProbeActivity(dir string) (ActivityStatus, error) {
	path := filepath.Join(dir, lockFile)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ActivityInactive, nil
	}
	if err != nil {
		return ActivityUnknown, fmt.Errorf("session: inspect lock %s: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return ActivityUnknown, fmt.Errorf("session: lock path is not a regular file: %s", path)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return ActivityUnknown, fmt.Errorf("session: open lock probe %s: %w", path, err)
	}
	after, statErr := f.Stat()
	if statErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		closeErr := f.Close()
		if statErr == nil {
			statErr = errors.New("lock path changed while opening")
		}
		return ActivityUnknown, errors.Join(fmt.Errorf("session: inspect opened lock %s: %w", path, statErr), closeErr)
	}

	lockErr := lockSessionFile(f)
	if errors.Is(lockErr, errLockHeld) {
		if closeErr := f.Close(); closeErr != nil {
			return ActivityActive, fmt.Errorf("session: close active lock probe %s: %w", path, closeErr)
		}
		return ActivityActive, nil
	}
	if lockErr != nil {
		closeErr := f.Close()
		return ActivityUnknown, errors.Join(fmt.Errorf("session: probe lock %s: %w", path, lockErr), closeErr)
	}
	unlockErr := unlockSessionFile(f)
	closeErr := f.Close()
	if err := errors.Join(unlockErr, closeErr); err != nil {
		return ActivityUnknown, fmt.Errorf("session: release lock probe %s: %w", path, err)
	}
	return ActivityInactive, nil
}

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
