package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	maxListStateBytes     = 8 << 20
	maxSummaryPromptRunes = 4096
)

// ActivityStatus is a point-in-time session lock status.
type ActivityStatus string

const (
	ActivityActive   ActivityStatus = "active"
	ActivityInactive ActivityStatus = "inactive"
	ActivityUnknown  ActivityStatus = "unknown"
)

// UnsupportedSchemaVersionError reports a recorded session that cannot be
// listed by this Harness version.
type UnsupportedSchemaVersionError struct {
	Path string
	Got  int
	Want int
}

func (e *UnsupportedSchemaVersionError) Error() string {
	return fmt.Sprintf("%s: unsupported schema version %d (want %d)", e.Path, e.Got, e.Want)
}

// Summary is terminal-neutral metadata for one recorded root session.
type Summary struct {
	Path          string
	ID            string
	CWD           string
	Created       time.Time
	Updated       time.Time
	InitialPrompt string
	Activity      ActivityStatus
}

// ListOptions controls default-root session discovery.
type ListOptions struct {
	CWD           string
	All           bool
	IncludePrompt bool
	ProbeActivity bool
}

// DefaultRoot returns the directory containing Harness's automatically saved
// root sessions for stateDir.
func DefaultRoot(stateDir string) string {
	return filepath.Join(stateDir, "harness", "sessions")
}

// List discovers recorded root sessions immediately below root. Individual
// unreadable sessions are returned in skipped; an unreadable root is fatal.
func List(root string, opts ListOptions) (summaries []Summary, skipped []error, err error) {
	if root == "" {
		return nil, nil, errors.New("session: list root is empty")
	}
	root, err = filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, nil, fmt.Errorf("session: resolve list root: %w", err)
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("session: read sessions root %s: %w", root, err)
	}

	var wantedCWD string
	if !opts.All {
		if opts.CWD == "" {
			return nil, nil, nil
		}
		wantedCWD, err = cleanAbsoluteSessionPath(opts.CWD)
		if err != nil {
			return nil, nil, fmt.Errorf("session: resolve working directory %s: %w", opts.CWD, err)
		}
	}

	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		info, infoErr := os.Lstat(dir)
		if infoErr != nil {
			skipped = append(skipped, fmt.Errorf("%s: inspect session directory: %w", dir, infoErr))
			continue
		}
		if !info.IsDir() {
			continue
		}

		statePath := filepath.Join(dir, stateFile)
		stateInfo, stateErr := os.Lstat(statePath)
		if errors.Is(stateErr, os.ErrNotExist) {
			continue
		}
		if stateErr != nil {
			skipped = append(skipped, fmt.Errorf("%s: inspect state: %w", statePath, stateErr))
			continue
		}
		// A symlink or non-regular state path is not a recorded-session marker.
		if stateInfo.Mode()&os.ModeSymlink != 0 || !stateInfo.Mode().IsRegular() {
			continue
		}

		summary, stateErr := readSessionSummaryState(dir, statePath, stateInfo)
		if stateErr != nil {
			skipped = append(skipped, stateErr)
			continue
		}
		if !opts.All {
			persistedCWD, cwdErr := cleanAbsoluteSessionPath(summary.CWD)
			if cwdErr != nil {
				skipped = append(skipped, fmt.Errorf("%s: resolve persisted working directory %q: %w", statePath, summary.CWD, cwdErr))
				continue
			}
			if persistedCWD == "" || persistedCWD != wantedCWD {
				continue
			}
		}

		if opts.IncludePrompt {
			summary.InitialPrompt, stateErr = readInitialPrompt(dir)
			if stateErr != nil {
				skipped = append(skipped, stateErr)
				summary.InitialPrompt = ""
			}
		}
		if opts.ProbeActivity {
			summary.Activity, stateErr = ProbeActivity(dir)
			if stateErr != nil {
				skipped = append(skipped, fmt.Errorf("%s: probe activity: %w", dir, stateErr))
			}
		} else {
			summary.Activity = ActivityUnknown
		}
		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Created.Equal(summaries[j].Created) {
			return summaries[i].Path < summaries[j].Path
		}
		return summaries[i].Created.After(summaries[j].Created)
	})
	return summaries, skipped, nil
}

func readSessionSummaryState(dir, path string, before os.FileInfo) (Summary, error) {
	data, err := readBoundedListedFile(path, before, maxListStateBytes)
	if err != nil {
		return Summary{}, err
	}
	var state struct {
		Version int       `json:"version"`
		ID      string    `json:"id"`
		CWD     string    `json:"cwd"`
		Created time.Time `json:"created"`
		Updated time.Time `json:"updated"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return Summary{}, fmt.Errorf("%s: decode state: %w", path, err)
	}
	if state.Version != Version {
		return Summary{}, &UnsupportedSchemaVersionError{Path: path, Got: state.Version, Want: Version}
	}
	if state.ID == "" {
		return Summary{}, fmt.Errorf("%s: state is missing session id", path)
	}
	return Summary{
		Path:     dir,
		ID:       state.ID,
		CWD:      state.CWD,
		Created:  state.Created,
		Updated:  state.Updated,
		Activity: ActivityUnknown,
	}, nil
}

func readBoundedListedFile(path string, before os.FileInfo, limit int64) ([]byte, error) {
	if before.Size() > limit {
		return nil, fmt.Errorf("%s: file exceeds %d bytes", path, limit)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: open: %w", path, err)
	}
	after, statErr := f.Stat()
	if statErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		closeErr := f.Close()
		if statErr == nil {
			statErr = errors.New("file changed while opening")
		}
		return nil, errors.Join(fmt.Errorf("%s: inspect opened file: %w", path, statErr), closeErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("%s: read: %w", path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: file exceeds %d bytes", path, limit)
	}
	return data, nil
}

func readInitialPrompt(dir string) (string, error) {
	path := filepath.Join(dir, eventLog)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%s: inspect replay: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", fmt.Errorf("%s: replay path is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: open replay: %w", path, err)
	}
	after, statErr := f.Stat()
	if statErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		closeErr := f.Close()
		if statErr == nil {
			statErr = errors.New("replay changed while opening")
		}
		return "", errors.Join(fmt.Errorf("%s: inspect opened replay: %w", path, statErr), closeErr)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, followReadBufferSize), maxReplayRecordSize)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("%s: decode replay before initial prompt: %w", path, err)
		}
		if event.Type == EventUser {
			closeErr := f.Close()
			if closeErr != nil {
				return "", fmt.Errorf("%s: close replay: %w", path, closeErr)
			}
			return boundSummaryPrompt(event.Text), nil
		}
	}
	scanErr := scanner.Err()
	closeErr := f.Close()
	if err := errors.Join(scanErr, closeErr); err != nil {
		return "", fmt.Errorf("%s: scan replay before initial prompt: %w", path, err)
	}
	return "", nil
}

func boundSummaryPrompt(prompt string) string {
	runes := 0
	for index := range prompt {
		if runes == maxSummaryPromptRunes {
			return prompt[:index]
		}
		runes++
	}
	return prompt
}

func cleanAbsoluteSessionPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}
