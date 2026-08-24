package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// EvidenceCatalogVersion versions the deterministic, derived catalog shape.
	// The catalog is never persisted separately from canonical session events.
	EvidenceCatalogVersion = 1
	DefaultEvidenceLimit   = 20
	MaxEvidenceLimit       = 100

	EvidenceKindEvaluator = "evaluator"
	EvidenceKindTool      = "tool"

	EvidenceStatusAvailable    = "available"
	EvidenceStatusStale        = "stale"
	EvidenceStatusMissing      = "missing"
	EvidenceStatusExternal     = "external"
	EvidenceStatusUnsafe       = "unsafe"
	EvidenceStatusUnreadable   = "unreadable"
	EvidenceStatusUnreferenced = "unreferenced"
	EvidenceStatusRecorded     = "recorded"
)

// EvidenceQuery selects a bounded newest-first page from a session's derived
// evidence catalog. Empty Kind and Status match every supported value. Prompt
// zero matches every prompt. Limit zero selects DefaultEvidenceLimit.
type EvidenceQuery struct {
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Status string `json:"status,omitempty"`
	Prompt int    `json:"prompt,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// EvidenceRecord is bounded metadata derived from one canonical session event
// and, when present, its referenced local artifact. Artifact contents are never
// read into the catalog.
type EvidenceRecord struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	Outcome        string     `json:"outcome"`
	Prompt         int        `json:"prompt,omitempty"`
	Turn           int        `json:"turn,omitempty"`
	Time           *time.Time `json:"time,omitempty"`
	Source         string     `json:"source,omitempty"`
	Reference      string     `json:"reference,omitempty"`
	Path           string     `json:"path,omitempty"`
	Bytes          int64      `json:"bytes,omitempty"`
	Modified       *time.Time `json:"modified,omitempty"`
	Summary        string     `json:"summary,omitempty"`
	ErrorKind      string     `json:"error_kind,omitempty"`
	ErrorExcerpt   string     `json:"error_excerpt,omitempty"`
	Score          *float64   `json:"score,omitempty"`
	ScoreDirection string     `json:"score_direction,omitempty"`
	Candidate      string     `json:"candidate,omitempty"`
}

// EvidencePage is the stable response envelope for REPL and CLI consumers.
// Records are newest first. Omitted counts matching records hidden by Limit.
type EvidencePage struct {
	Version  int              `json:"version"`
	Session  string           `json:"session"`
	Total    int              `json:"total"`
	Matched  int              `json:"matched"`
	Returned int              `json:"returned"`
	Omitted  int              `json:"omitted"`
	Records  []EvidenceRecord `json:"records"`
}

// QueryEvidence derives a bounded catalog directly from raw.ndjson. It
// catalogs every evaluator result plus tool errors and tool results that
// declare a truncated-output artifact. Successful inline-only tool results are
// intentionally excluded to keep the surface focused on durable evidence.
func QueryEvidence(dir string, query EvidenceQuery) (EvidencePage, error) {
	query.ID = strings.TrimSpace(query.ID)
	query.Kind = strings.ToLower(strings.TrimSpace(query.Kind))
	query.Status = strings.ToLower(strings.TrimSpace(query.Status))
	if err := ValidateEvidenceQuery(query); err != nil {
		return EvidencePage{}, err
	}
	if query.Limit == 0 {
		query.Limit = DefaultEvidenceLimit
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return EvidencePage{}, fmt.Errorf("session: evidence catalog path: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return EvidencePage{}, fmt.Errorf("session: evidence catalog directory: %w", err)
	}
	if !info.IsDir() {
		return EvidencePage{}, fmt.Errorf("session: evidence catalog path is not a directory: %s", dir)
	}
	page := EvidencePage{Version: EvidenceCatalogVersion, Session: absDir, Records: []EvidenceRecord{}}
	if err := validateReplaySchema(dir); err != nil {
		return EvidencePage{}, err
	}
	cwd, err := evidenceSessionCWD(dir)
	if err != nil {
		return EvidencePage{}, err
	}
	eventPath := filepath.Join(dir, eventLog)
	eventInfo, err := os.Lstat(eventPath)
	if errors.Is(err, os.ErrNotExist) {
		return page, nil
	}
	if err != nil {
		return EvidencePage{}, fmt.Errorf("session: inspect evidence event log: %w", err)
	}
	if !eventInfo.Mode().IsRegular() {
		return EvidencePage{}, fmt.Errorf("session: evidence event log is not a regular file: %s", eventPath)
	}
	f, err := os.Open(eventPath)
	if err != nil {
		return EvidencePage{}, fmt.Errorf("session: open evidence event log: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, followReadBufferSize), maxReplayRecordSize)
	evaluatorSequence, toolSequence := 0, 0
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return EvidencePage{}, fmt.Errorf("session: evidence replay decode: %w", err)
		}
		var record EvidenceRecord
		include := false
		switch event.Type {
		case EventEvaluatorResult:
			evaluatorSequence++
			record = evaluatorEvidenceRecord(dir, cwd, evaluatorSequence, event)
			include = true
		case EventToolResult:
			toolSequence++
			if event.ResultError || event.ResultTruncated {
				record = toolEvidenceRecord(dir, toolSequence, event)
				include = true
			}
		}
		if !include {
			continue
		}
		page.Total++
		if !evidenceMatches(record, query) {
			continue
		}
		page.Matched++
		if len(page.Records) < query.Limit {
			page.Records = append(page.Records, record)
		} else {
			copy(page.Records, page.Records[1:])
			page.Records[len(page.Records)-1] = record
		}
	}
	if err := scanner.Err(); err != nil {
		return EvidencePage{}, fmt.Errorf("session: scan evidence event log: %w", err)
	}
	for left, right := 0, len(page.Records)-1; left < right; left, right = left+1, right-1 {
		page.Records[left], page.Records[right] = page.Records[right], page.Records[left]
	}
	page.Returned = len(page.Records)
	page.Omitted = page.Matched - page.Returned
	return page, nil
}

// ValidateEvidenceQuery checks public query bounds without opening a session.
func ValidateEvidenceQuery(query EvidenceQuery) error {
	kind := strings.ToLower(strings.TrimSpace(query.Kind))
	status := strings.ToLower(strings.TrimSpace(query.Status))
	if query.Prompt < 0 {
		return errors.New("session: evidence prompt must be non-negative")
	}
	if query.Limit < 0 || query.Limit > MaxEvidenceLimit {
		return fmt.Errorf("session: evidence limit must be between 1 and %d", MaxEvidenceLimit)
	}
	if kind != "" && kind != EvidenceKindEvaluator && kind != EvidenceKindTool {
		return fmt.Errorf("session: unsupported evidence kind %q (want evaluator or tool)", query.Kind)
	}
	if status != "" && !validEvidenceStatus(status) {
		return fmt.Errorf("session: unsupported evidence status %q", query.Status)
	}
	return nil
}

func validEvidenceStatus(status string) bool {
	switch status {
	case EvidenceStatusAvailable, EvidenceStatusStale, EvidenceStatusMissing,
		EvidenceStatusExternal, EvidenceStatusUnsafe, EvidenceStatusUnreadable,
		EvidenceStatusUnreferenced, EvidenceStatusRecorded:
		return true
	default:
		return false
	}
}

func evidenceMatches(record EvidenceRecord, query EvidenceQuery) bool {
	return (query.ID == "" || record.ID == query.ID) &&
		(query.Kind == "" || record.Kind == query.Kind) &&
		(query.Status == "" || record.Status == query.Status) &&
		(query.Prompt == 0 || record.Prompt == query.Prompt)
}

func evaluatorEvidenceRecord(sessionDir, cwd string, sequence int, event Event) EvidenceRecord {
	record := EvidenceRecord{
		ID:      "eval-" + zeroPadEvidenceSequence(sequence),
		Kind:    EvidenceKindEvaluator,
		Outcome: "unknown",
		Prompt:  event.Prompt,
		Turn:    event.Turn,
		Time:    evidenceTimePointer(event.Time),
	}
	if result := event.EvaluatorResult; result != nil {
		record.Source = result.Handler
		record.Reference = result.EvidenceRef
		record.Score = result.Score
		record.ScoreDirection = result.ScoreDirection
		record.Candidate = result.Candidate
		if result.Accepted {
			record.Outcome = "accepted"
		} else {
			record.Outcome = "rejected"
		}
	}
	if record.Reference == "" {
		record.Status = EvidenceStatusUnreferenced
		return record
	}
	var modified time.Time
	record.Status, record.Path, record.Bytes, modified = inspectEvaluatorReference(sessionDir, cwd, record.Reference, event.Time)
	record.Modified = evidenceTimePointer(modified)
	return record
}

func toolEvidenceRecord(sessionDir string, sequence int, event Event) EvidenceRecord {
	record := EvidenceRecord{
		ID:           "tool-" + zeroPadEvidenceSequence(sequence),
		Kind:         EvidenceKindTool,
		Prompt:       event.Prompt,
		Turn:         event.Turn,
		Time:         evidenceTimePointer(event.Time),
		Source:       event.Tool,
		Summary:      event.Display,
		ErrorKind:    event.ErrorKind,
		ErrorExcerpt: event.ErrorExcerpt,
	}
	if event.ResultError {
		record.Outcome = "error"
	} else {
		record.Outcome = "success"
	}
	if !event.ResultTruncated {
		record.Status = EvidenceStatusRecorded
		return record
	}
	record.Reference = event.ArtifactRef
	if record.Reference == "" {
		record.Reference = ToolResultArtifactReference(event.Prompt, event.Turn, event.ToolID)
	}
	var modified time.Time
	record.Status, record.Path, record.Bytes, modified = inspectSessionReference(sessionDir, record.Reference, event.Time)
	record.Modified = evidenceTimePointer(modified)
	return record
}

func zeroPadEvidenceSequence(sequence int) string {
	return fmt.Sprintf("%06d", sequence)
}

func evidenceTimePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}

func evidenceSessionCWD(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("session: read evidence state: %w", err)
	}
	var state struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return "", fmt.Errorf("session: decode evidence state: %w", err)
	}
	return state.CWD, nil
}

func inspectEvaluatorReference(sessionDir, cwd, reference string, eventTime time.Time) (string, string, int64, time.Time) {
	if strings.ContainsRune(reference, 0) {
		return EvidenceStatusUnsafe, "", 0, time.Time{}
	}
	if looksLikeExternalEvidenceReference(reference) {
		return EvidenceStatusExternal, "", 0, time.Time{}
	}
	if filepath.IsAbs(reference) {
		for _, root := range []string{cwd, sessionDir} {
			if relative, ok := evidencePathWithin(root, reference); ok {
				return inspectEvidencePath(root, relative, eventTime)
			}
		}
		return EvidenceStatusExternal, "", 0, time.Time{}
	}
	if cwd == "" {
		return EvidenceStatusExternal, "", 0, time.Time{}
	}
	reference = filepath.FromSlash(reference)
	if filepath.IsAbs(reference) {
		return EvidenceStatusExternal, "", 0, time.Time{}
	}
	clean := filepath.Clean(reference)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return EvidenceStatusExternal, "", 0, time.Time{}
	}
	return inspectEvidencePath(cwd, clean, eventTime)
}

func inspectSessionReference(sessionDir, reference string, eventTime time.Time) (string, string, int64, time.Time) {
	if reference == "" || strings.ContainsRune(reference, 0) || filepath.IsAbs(reference) {
		return EvidenceStatusUnsafe, "", 0, time.Time{}
	}
	clean := filepath.Clean(filepath.FromSlash(reference))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return EvidenceStatusUnsafe, "", 0, time.Time{}
	}
	return inspectEvidencePath(sessionDir, clean, eventTime)
}

func evidencePathWithin(root, path string) (string, bool) {
	if root == "" {
		return "", false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return relative, true
}

func inspectEvidencePath(root, relative string, eventTime time.Time) (string, string, int64, time.Time) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return EvidenceStatusUnreadable, "", 0, time.Time{}
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if errors.Is(err, os.ErrNotExist) {
		return EvidenceStatusMissing, filepath.Join(rootAbs, relative), 0, time.Time{}
	}
	if err != nil {
		return EvidenceStatusUnreadable, rootAbs, 0, time.Time{}
	}
	path := filepath.Join(rootResolved, relative)
	current := rootResolved
	components := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return EvidenceStatusMissing, path, 0, time.Time{}
		}
		if err != nil {
			return EvidenceStatusUnreadable, path, 0, time.Time{}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return EvidenceStatusUnsafe, path, 0, time.Time{}
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return EvidenceStatusMissing, path, 0, time.Time{}
	}
	if err != nil {
		return EvidenceStatusUnreadable, path, 0, time.Time{}
	}
	if !info.Mode().IsRegular() {
		return EvidenceStatusUnsafe, path, 0, info.ModTime()
	}
	status := EvidenceStatusAvailable
	if !eventTime.IsZero() && info.ModTime().After(eventTime) {
		status = EvidenceStatusStale
	}
	return status, path, info.Size(), info.ModTime()
}

func looksLikeExternalEvidenceReference(reference string) bool {
	if strings.Contains(reference, "://") {
		return true
	}
	prefix, _, found := strings.Cut(reference, ":")
	if !found || prefix == "" {
		return false
	}
	for _, r := range prefix {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.') {
			return false
		}
	}
	// Preserve Windows drive-letter paths when running on Windows.
	return !(len(prefix) == 1 && len(reference) > 2 && (reference[2] == '\\' || reference[2] == '/'))
}
