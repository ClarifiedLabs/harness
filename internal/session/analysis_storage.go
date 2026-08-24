package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	maxAnalysisStorageEntries = 10_000
	maxAnalysisStorageDepth   = 64
	maxAnalysisTreeBytes      = 64 << 20
)

// StorageComponent is bounded filesystem metadata for one session data class.
// Status is complete, missing, incomplete, malformed, symlink,
// cutoff_incomplete, or limit_exceeded. Symlinks are counted but never followed.
var errStopAnalysisStorageWalk = errors.New("stop bounded storage walk")

type StorageComponent struct {
	Status      string `json:"status"`
	Files       int    `json:"files"`
	Directories int    `json:"directories"`
	Bytes       int64  `json:"bytes"`
	Symlinks    int    `json:"symlinks"`
}

// StorageAnalysis contains sizes and reset encoding counters only. It never
// exposes transcript bodies, tool inputs, or artifact contents.
type StorageAnalysis struct {
	Available            bool             `json:"available"`
	State                StorageComponent `json:"state"`
	Tree                 StorageComponent `json:"tree"`
	Raw                  StorageComponent `json:"raw"`
	Compactions          StorageComponent `json:"compactions"`
	ToolResults          StorageComponent `json:"tool_results"`
	Lineage              StorageComponent `json:"lineage"`
	TotalBytes           int64            `json:"total_bytes"`
	ContextResetEntries  int              `json:"context_reset_entries"`
	SnapshotResetEntries int              `json:"snapshot_reset_entries"`
	DeltaResetEntries    int              `json:"delta_reset_entries"`
	SnapshotPayloadBytes int64            `json:"snapshot_payload_bytes"`
	DeltaPayloadBytes    int64            `json:"delta_payload_bytes"`
}

func analyzeStorage(dir string, source AnalysisSource, before time.Time) (StorageAnalysis, error) {
	state, err := analyzeJSONFile(filepath.Join(dir, stateFile), !before.IsZero())
	if err != nil {
		return StorageAnalysis{}, err
	}
	tree, resets, err := analyzeTreeStorage(filepath.Join(dir, treeFile), !before.IsZero())
	if err != nil {
		return StorageAnalysis{}, err
	}
	raw := StorageComponent{Status: source.Status, Files: 1, Bytes: source.SnapshotBytes}
	switch source.Status {
	case "missing":
		raw.Files = 0
	case "symlink":
		raw.Files = 0
		raw.Symlinks = 1
	}
	if !before.IsZero() && raw.Status == "complete" {
		raw.Status = "cutoff_incomplete"
	}
	compactions, err := analyzeStorageDir(filepath.Join(dir, "compactions"), !before.IsZero())
	if err != nil {
		return StorageAnalysis{}, err
	}
	toolResults, err := analyzeStorageDir(filepath.Join(dir, "artifacts", "tool-results"), !before.IsZero())
	if err != nil {
		return StorageAnalysis{}, err
	}
	lineage, err := analyzeStorageDir(filepath.Join(dir, "lineage"), !before.IsZero())
	if err != nil {
		return StorageAnalysis{}, err
	}
	out := StorageAnalysis{
		Available: true, State: state, Tree: tree, Raw: raw,
		Compactions: compactions, ToolResults: toolResults, Lineage: lineage,
		ContextResetEntries:  resets.contextResetEntries,
		SnapshotResetEntries: resets.snapshotResetEntries,
		DeltaResetEntries:    resets.deltaResetEntries,
		SnapshotPayloadBytes: resets.snapshotPayloadBytes,
		DeltaPayloadBytes:    resets.deltaPayloadBytes,
	}
	out.TotalBytes = state.Bytes + tree.Bytes + raw.Bytes + compactions.Bytes + toolResults.Bytes + lineage.Bytes
	return out, nil
}

func analyzeJSONFile(path string, cutoff bool) (StorageComponent, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return StorageComponent{Status: "missing"}, nil
	}
	if err != nil {
		return StorageComponent{}, fmt.Errorf("session: analyze storage %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return StorageComponent{Status: "symlink", Symlinks: 1}, nil
	}
	if !info.Mode().IsRegular() {
		return StorageComponent{Status: "malformed"}, nil
	}
	out := StorageComponent{Status: "complete", Files: 1, Bytes: info.Size()}
	if info.Size() > maxAnalysisMetadataBytes {
		out.Status = "limit_exceeded"
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return StorageComponent{}, fmt.Errorf("session: analyze storage %s: %w", path, err)
	}
	dec := json.NewDecoder(io.LimitReader(f, maxAnalysisMetadataBytes+1))
	var value any
	decodeErr := dec.Decode(&value)
	if decodeErr == nil {
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			decodeErr = err
			if decodeErr == nil {
				decodeErr = errors.New("multiple JSON values")
			}
		}
	}
	closeErr := f.Close()
	if decodeErr != nil {
		out.Status = "malformed"
	} else if closeErr != nil {
		return StorageComponent{}, closeErr
	} else if cutoff {
		out.Status = "cutoff_incomplete"
	}
	return out, nil
}

type resetStorage struct {
	contextResetEntries  int
	snapshotResetEntries int
	deltaResetEntries    int
	snapshotPayloadBytes int64
	deltaPayloadBytes    int64
}

func analyzeTreeStorage(path string, cutoff bool) (StorageComponent, resetStorage, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return StorageComponent{Status: "missing"}, resetStorage{}, nil
	}
	if err != nil {
		return StorageComponent{}, resetStorage{}, fmt.Errorf("session: analyze storage %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return StorageComponent{Status: "symlink", Symlinks: 1}, resetStorage{}, nil
	}
	if !info.Mode().IsRegular() {
		return StorageComponent{Status: "malformed"}, resetStorage{}, nil
	}
	out := StorageComponent{Status: "complete", Files: 1, Bytes: info.Size()}
	if info.Size() > maxAnalysisTreeBytes {
		out.Status = "limit_exceeded"
		return out, resetStorage{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return StorageComponent{}, resetStorage{}, fmt.Errorf("session: analyze storage %s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, maxAnalysisTreeBytes+1))
	scanner.Buffer(make([]byte, 64*1024), maxAnalysisTreeBytes)
	lineNumber := 0
	var resets resetStorage
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		lineNumber++
		var record struct {
			Type         string          `json:"type"`
			Messages     json.RawMessage `json:"messages"`
			ContextDelta json.RawMessage `json:"context_delta"`
			Delta        json.RawMessage `json:"delta"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			out.Status = "malformed"
			continue
		}
		if lineNumber == 1 || record.Type != string(EntryContextReset) {
			continue
		}
		resets.contextResetEntries++
		if len(record.ContextDelta) > 0 && string(record.ContextDelta) != "null" {
			resets.deltaResetEntries++
			resets.deltaPayloadBytes += int64(len(record.ContextDelta))
		} else if len(record.Delta) > 0 && string(record.Delta) != "null" {
			resets.deltaResetEntries++
			resets.deltaPayloadBytes += int64(len(record.Delta))
		} else {
			resets.snapshotResetEntries++
			resets.snapshotPayloadBytes += int64(len(record.Messages))
		}
	}
	if err := scanner.Err(); err != nil {
		out.Status = "malformed"
	}
	if cutoff && out.Status == "complete" {
		out.Status = "cutoff_incomplete"
	}
	return out, resets, nil
}

func analyzeStorageDir(path string, cutoff bool) (StorageComponent, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return StorageComponent{Status: "missing"}, nil
	}
	if err != nil {
		return StorageComponent{}, fmt.Errorf("session: analyze storage %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return StorageComponent{Status: "symlink", Symlinks: 1}, nil
	}
	if !info.IsDir() {
		return StorageComponent{Status: "malformed"}, nil
	}
	out := StorageComponent{Status: "complete", Directories: 1}
	visited := 1
	limitReached := false
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > maxAnalysisStorageDepth {
			out.Status = "limit_exceeded"
			limitReached = true
			return nil
		}
		err := forEachAnalysisDirEntry(dir, func(entry os.DirEntry) error {
			if limitReached {
				return errStopAnalysisStorageWalk
			}
			if visited >= maxAnalysisStorageEntries {
				out.Status = "limit_exceeded"
				limitReached = true
				return errStopAnalysisStorageWalk
			}
			visited++
			child := filepath.Join(dir, entry.Name())
			childInfo, err := os.Lstat(child)
			if err != nil {
				return err
			}
			if childInfo.Mode()&os.ModeSymlink != 0 {
				out.Symlinks++
				if out.Status == "complete" {
					out.Status = "symlink"
				}
				return nil
			}
			if childInfo.IsDir() {
				out.Directories++
				if err := walk(child, depth+1); err != nil {
					return err
				}
				return nil
			}
			if childInfo.Mode().IsRegular() {
				out.Files++
				out.Bytes += childInfo.Size()
			}
			return nil
		})
		if errors.Is(err, errStopAnalysisStorageWalk) {
			return nil
		}
		return err
	}
	if err := walk(path, 0); err != nil {
		return StorageComponent{}, fmt.Errorf("session: analyze storage %s: %w", path, err)
	}
	if cutoff && out.Status == "complete" {
		out.Status = "cutoff_incomplete"
	}
	return out, nil
}

func (s *StorageAnalysis) add(other StorageAnalysis) {
	s.Available = s.Available || other.Available
	addStorageComponent(&s.State, other.State)
	addStorageComponent(&s.Tree, other.Tree)
	addStorageComponent(&s.Raw, other.Raw)
	addStorageComponent(&s.Compactions, other.Compactions)
	addStorageComponent(&s.ToolResults, other.ToolResults)
	addStorageComponent(&s.Lineage, other.Lineage)
	s.TotalBytes += other.TotalBytes
	s.ContextResetEntries += other.ContextResetEntries
	s.SnapshotResetEntries += other.SnapshotResetEntries
	s.DeltaResetEntries += other.DeltaResetEntries
	s.SnapshotPayloadBytes += other.SnapshotPayloadBytes
	s.DeltaPayloadBytes += other.DeltaPayloadBytes
}

func addStorageComponent(dst *StorageComponent, src StorageComponent) {
	dst.Files += src.Files
	dst.Directories += src.Directories
	dst.Bytes += src.Bytes
	dst.Symlinks += src.Symlinks
	if storageStatusRank(src.Status) > storageStatusRank(dst.Status) {
		dst.Status = src.Status
	}
}

func storageStatusRank(status string) int {
	switch status {
	case "limit_exceeded":
		return 7
	case "malformed":
		return 6
	case "incomplete":
		return 5
	case "symlink":
		return 4
	case "cutoff_incomplete":
		return 3
	case "missing":
		return 2
	case "complete":
		return 1
	default:
		return 0
	}
}
