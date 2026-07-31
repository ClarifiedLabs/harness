package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"harness/internal/llm"
)

// ErrorFilter narrows collected error rows; empty fields match everything.
type ErrorFilter struct {
	Tool  string
	Kind  string
	Model string
	Agent string
}

func (f ErrorFilter) matches(r ErrorRow) bool {
	if f.Tool != "" && r.Tool != f.Tool {
		return false
	}
	if f.Kind != "" && r.Kind != f.Kind {
		return false
	}
	if f.Model != "" && r.Model != f.Model {
		return false
	}
	if f.Agent != "" && r.Agent != f.Agent {
		return false
	}
	return true
}

// ErrorRow is one classified failure: a failed tool result or a failed model
// request, with the session context it happened in. Kind is the structured
// error_kind on new logs, or a text-classified value on legacy logs;
// Confidence says which. Tool is empty for model request failures.
type ErrorRow struct {
	Session     string    `json:"session"`
	Agent       string    `json:"agent,omitempty"`
	ModelTarget string    `json:"model_target,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	APIType     string    `json:"api_type,omitempty"`
	Model       string    `json:"model,omitempty"`
	Prompt      int       `json:"prompt,omitempty"`
	Turn        int       `json:"turn,omitempty"`
	ContextPct  int       `json:"context_pct,omitempty"`
	Tool        string    `json:"tool,omitempty"`
	Kind        string    `json:"kind"`
	Confidence  string    `json:"confidence"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	ResultIndex int       `json:"result_index,omitempty"`
	Consecutive int       `json:"consecutive,omitempty"`
	At          time.Time `json:"at"`
	Excerpt     string    `json:"excerpt,omitempty"`
}

// ErrorSource identifies the immutable event-log prefix used for one analysis
// stream. Appends after collection starts cannot change the reported rows.
type ErrorSource struct {
	Session       string `json:"session"`
	IncludedBytes int    `json:"included_bytes"`
	Events        int    `json:"events"`
	SHA256        string `json:"sha256"`
}

// ErrorAnalysis is the reproducible result of scanning one root session and
// its physical delegate streams.
type ErrorAnalysis struct {
	Rows    []ErrorRow    `json:"rows"`
	Summary ErrorSummary  `json:"summary"`
	Sources []ErrorSource `json:"sources"`
}

// MergeErrorAnalyses combines independently collected roots while preserving
// the physical-stream streaks already stored on each row.
func MergeErrorAnalyses(items ...ErrorAnalysis) ErrorAnalysis {
	merged := ErrorAnalysis{Summary: newErrorSummary()}
	for _, item := range items {
		merged.Rows = append(merged.Rows, item.Rows...)
		merged.Sources = append(merged.Sources, item.Sources...)
		merged.Summary.add(item.Summary)
	}
	sort.SliceStable(merged.Rows, func(i, j int) bool {
		if !merged.Rows[i].At.Equal(merged.Rows[j].At) {
			return merged.Rows[i].At.Before(merged.Rows[j].At)
		}
		return merged.Rows[i].Session < merged.Rows[j].Session
	})
	merged.Summary.finish(merged.Rows)
	return merged
}

// CollectErrors reads one session (root plus its delegate children) and
// returns the classified failure rows in chronological order, filtered by
// filter. Model request failures come from model_request events; tool
// failures from tool_result events with result_error.
func CollectErrors(dir string, filter ErrorFilter) ([]ErrorRow, error) {
	analysis, err := AnalyzeErrors(dir, filter, time.Time{})
	return analysis.Rows, err
}

// AnalyzeErrors scans one session tree. before, when non-zero, excludes
// events after the supplied timestamp while retaining a hash of exactly the
// complete NDJSON records that were considered.
func AnalyzeErrors(dir string, filter ErrorFilter, before time.Time) (ErrorAnalysis, error) {
	state, err := Load(dir)
	if err != nil {
		return ErrorAnalysis{}, fmt.Errorf("collect errors in %s: %w", dir, err)
	}
	analysis := ErrorAnalysis{Summary: newErrorSummary()}
	events, source, err := readErrorEvents(dir, before)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrorAnalysis{}, fmt.Errorf("read replay in %s: %w", dir, err)
	}
	stream := collectErrorStream(events, dir, state.Agent, state.Provider, state.Model, filter)
	analysis.Rows = append(analysis.Rows, stream.rows...)
	analysis.Summary.add(stream.summary)
	if source.Session != "" {
		analysis.Sources = append(analysis.Sources, source)
	}

	children, err := delegateChildren(dir)
	if err != nil {
		return ErrorAnalysis{}, err
	}
	for _, child := range children {
		events, source, err := readErrorEvents(child.dir, before)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return ErrorAnalysis{}, fmt.Errorf("read replay in %s: %w", child.dir, err)
		}
		stream := collectErrorStream(events, child.dir, child.meta.Agent, child.meta.Provider, child.meta.Model, filter)
		analysis.Rows = append(analysis.Rows, stream.rows...)
		analysis.Summary.add(stream.summary)
		analysis.Sources = append(analysis.Sources, source)
	}

	sort.SliceStable(analysis.Rows, func(i, j int) bool {
		if !analysis.Rows[i].At.Equal(analysis.Rows[j].At) {
			return analysis.Rows[i].At.Before(analysis.Rows[j].At)
		}
		return analysis.Rows[i].Session < analysis.Rows[j].Session
	})
	analysis.Summary.finish(analysis.Rows)
	return analysis, nil
}

func readErrorEvents(dir string, before time.Time) ([]Event, ErrorSource, error) {
	if err := validateReplaySchema(dir); err != nil {
		return nil, ErrorSource{}, err
	}
	path := filepath.Join(dir, eventLog)
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrorSource{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, ErrorSource{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, info.Size()))
	if err != nil {
		return nil, ErrorSource{}, err
	}
	// A writer may have been between Encode writes when Stat ran. Analyze only
	// complete newline-terminated records from the captured byte prefix.
	if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
		data = data[:end+1]
	} else {
		data = nil
	}
	var events []Event
	var included bytes.Buffer
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, ErrorSource{}, fmt.Errorf("session: replay decode: %w", err)
		}
		if !before.IsZero() && ev.Time.After(before) {
			continue
		}
		events = append(events, ev)
		included.Write(line)
		included.WriteByte('\n')
	}
	sum := sha256.Sum256(included.Bytes())
	return events, ErrorSource{
		Session: dir, IncludedBytes: included.Len(), Events: len(events),
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

type delegateChild struct {
	dir  string
	meta ChildMeta
}

// delegateChildren lists child session directories with their metadata, in
// directory order for deterministic output. Children without meta.json are
// skipped (a child may still be starting).
func delegateChildren(rootDir string) ([]delegateChild, error) {
	childrenDir := filepath.Join(rootDir, "children")
	entries, err := os.ReadDir(childrenDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read delegates in %s: %w", rootDir, err)
	}
	var children []delegateChild
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(childrenDir, entry.Name())
		metaPath := filepath.Join(dir, "meta.json")
		var meta ChildMeta
		if err := decodeJSONFile(metaPath, &meta, true); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("decode delegate metadata %s: %w", metaPath, err)
		}
		children = append(children, delegateChild{dir: dir, meta: meta})
	}
	return children, nil
}

type errorStream struct {
	rows    []ErrorRow
	summary ErrorSummary
}

type eventModelIdentity struct {
	targetID string
	provider string
	apiType  string
	model    string
}

func collectErrorStream(events []Event, sessionDir, agent, provider, model string, filter ErrorFilter) errorStream {
	stream := errorStream{summary: newErrorSummary()}
	var latestContext *ContextSnapshot
	identity := eventModelIdentity{provider: provider, model: model}
	resultIndex := 0
	type runKey struct{ tool, kind string }
	var previous runKey
	runLength := 0
	for _, ev := range events {
		if ev.Context != nil {
			snapshot := *ev.Context
			latestContext = &snapshot
		}
		if ev.Type == EventModelRequest && ev.ModelRequest != nil {
			if ev.ModelRequest.TargetID != "" {
				identity.targetID = ev.ModelRequest.TargetID
			}
			if ev.ModelRequest.Provider != "" {
				identity.provider = ev.ModelRequest.Provider
			}
			if ev.ModelRequest.APIType != "" {
				identity.apiType = ev.ModelRequest.APIType
			}
			if ev.ModelRequest.Model != "" {
				identity.model = ev.ModelRequest.Model
			}
		}
		switch {
		case ev.Type == EventToolResult:
			resultIndex++
			rowIdentity := identity
			if ev.ModelTarget != "" {
				rowIdentity.targetID = ev.ModelTarget
			}
			if ev.Provider != "" {
				rowIdentity.provider = ev.Provider
			}
			if ev.APIType != "" {
				rowIdentity.apiType = ev.APIType
			}
			if ev.Model != "" {
				rowIdentity.model = ev.Model
			}
			if filter.matchesResult(agent, ev.Tool, rowIdentity.model) {
				stream.summary.ToolResults++
				stream.summary.ResultsByTool[ev.Tool]++
				if rowIdentity.model != "" {
					stream.summary.ResultsByModel[rowIdentity.model]++
				}
				for metric, value := range ev.ResultMetrics {
					switch metric {
					case "operation_errors", "query_errors", "normalized_bounds":
						stream.summary.CompositeDiagnostics[metric] += value
					}
				}
			}
			if !ev.ResultError {
				previous, runLength = runKey{}, 0
				continue
			}
			kind := ev.ErrorKind
			confidence := "high"
			if kind == "" {
				classified := ClassifyToolError(ev.Display, ev.ErrorExcerpt)
				kind = string(classified.Kind)
				confidence = classified.Confidence
			}
			excerpt := ev.ErrorExcerpt
			if excerpt == "" {
				excerpt = displayErrorExcerpt(ev.Display)
			}
			key := runKey{tool: ev.Tool, kind: kind}
			if key == previous {
				runLength++
			} else {
				previous, runLength = key, 1
			}
			row := ErrorRow{
				Session:     sessionDir,
				Agent:       agent,
				ModelTarget: rowIdentity.targetID,
				Provider:    rowIdentity.provider,
				APIType:     rowIdentity.apiType,
				Model:       rowIdentity.model,
				Prompt:      ev.Prompt,
				Turn:        ev.Turn,
				ContextPct:  contextPct(latestContext),
				Tool:        ev.Tool,
				Kind:        kind,
				Confidence:  confidence,
				DurationMS:  ev.DurationMS,
				ResultIndex: resultIndex,
				Consecutive: runLength,
				At:          ev.Time,
				Excerpt:     excerpt,
			}
			if filter.matches(row) {
				stream.rows = append(stream.rows, row)
			}
		case ev.Type == EventModelRequest && ev.ModelRequest != nil && ev.ModelRequest.State == llm.ModelRequestFailed:
			kind := ClassifyModelRequestFailure(ev.ModelRequest)
			confidence := "high"
			if kind == llm.ToolErrorProviderError {
				confidence = "low"
			}
			rowProvider, rowModel := identity.provider, identity.model
			if ev.ModelRequest.Provider != "" {
				rowProvider = ev.ModelRequest.Provider
			}
			if ev.ModelRequest.Model != "" {
				rowModel = ev.ModelRequest.Model
			}
			row := ErrorRow{
				Session:     sessionDir,
				Agent:       agent,
				ModelTarget: identity.targetID,
				Provider:    rowProvider,
				APIType:     identity.apiType,
				Model:       rowModel,
				Prompt:      ev.Prompt,
				Turn:        ev.Turn,
				ContextPct:  contextPct(latestContext),
				Kind:        string(kind),
				Confidence:  confidence,
				At:          ev.Time,
				Excerpt:     ErrorExcerpt(ev.ModelRequest.Message),
			}
			if filter.matches(row) {
				stream.rows = append(stream.rows, row)
			}
		}
	}
	return stream
}

func (f ErrorFilter) matchesResult(agent, tool, model string) bool {
	return (f.Tool == "" || f.Tool == tool) &&
		(f.Model == "" || f.Model == model) &&
		(f.Agent == "" || f.Agent == agent)
}

func contextPct(snapshot *ContextSnapshot) int {
	if snapshot == nil || snapshot.Window <= 0 {
		return 0
	}
	return (snapshot.Total*100 + snapshot.Window/2) / snapshot.Window
}

// displayErrorExcerpt recovers the error portion of a recorded one-line
// Display ("[tool args] → error: <first line>") for legacy rows that predate
// stored excerpts.
func displayErrorExcerpt(display string) string {
	if i := strings.Index(display, "→ error: "); i >= 0 {
		return display[i+len("→ error: "):]
	}
	return display
}

// ErrorRepeat marks one (tool, kind) pair that failed at least three times
// consecutively — the repeat-failure loop signature.
type ErrorRepeat struct {
	Tool        string `json:"tool"`
	Kind        string `json:"kind"`
	Consecutive int    `json:"consecutive"`
}

// ErrorSummary aggregates classified rows for the stats section and the
// errors subcommand summary lines.
type ErrorSummary struct {
	ToolResults            int            `json:"tool_results"`
	FailedToolResults      int            `json:"failed_tool_results"`
	ToolErrorRate          float64        `json:"tool_error_rate"`
	ModelRequestFailures   int            `json:"model_request_failures"`
	ByKind                 map[string]int `json:"by_kind,omitempty"`
	ByTool                 map[string]int `json:"by_tool,omitempty"`
	ByModel                map[string]int `json:"by_model,omitempty"`
	RequestFailuresByModel map[string]int `json:"request_failures_by_model,omitempty"`
	ResultsByTool          map[string]int `json:"results_by_tool,omitempty"`
	ResultsByModel         map[string]int `json:"results_by_model,omitempty"`
	CompositeDiagnostics   map[string]int `json:"composite_diagnostics,omitempty"`
	Repeats                []ErrorRepeat  `json:"repeats,omitempty"`
}

// repeatFailureMinRun is the consecutive-run length that counts as a
// repeat-failure loop.
const repeatFailureMinRun = 3

// SummarizeErrors aggregates rows, which must be in chronological order (as
// CollectErrors returns them).
func SummarizeErrors(rows []ErrorRow) ErrorSummary {
	summary := newErrorSummary()
	summary.finish(rows)
	return summary
}

func newErrorSummary() ErrorSummary {
	return ErrorSummary{
		ByKind:                 make(map[string]int),
		ByTool:                 make(map[string]int),
		ByModel:                make(map[string]int),
		RequestFailuresByModel: make(map[string]int),
		ResultsByTool:          make(map[string]int),
		ResultsByModel:         make(map[string]int),
		CompositeDiagnostics:   make(map[string]int),
	}
}

func (s *ErrorSummary) add(other ErrorSummary) {
	s.ToolResults += other.ToolResults
	for k, n := range other.ResultsByTool {
		s.ResultsByTool[k] += n
	}
	for k, n := range other.ResultsByModel {
		s.ResultsByModel[k] += n
	}
	for k, n := range other.CompositeDiagnostics {
		s.CompositeDiagnostics[k] += n
	}
}

func (s *ErrorSummary) finish(rows []ErrorRow) {
	// Error counts come from the filtered rows. Denominators were collected
	// from complete physical streams before failures were selected.
	if s.ByKind == nil {
		*s = newErrorSummary()
	}
	for _, r := range rows {
		if r.Tool == "" {
			s.ModelRequestFailures++
			if r.Model != "" {
				s.RequestFailuresByModel[r.Model]++
			}
		} else {
			s.FailedToolResults++
			s.ByTool[r.Tool]++
			if r.Model != "" {
				s.ByModel[r.Model]++
			}
		}
		s.ByKind[r.Kind]++
	}
	if s.ToolResults > 0 {
		s.ToolErrorRate = float64(s.FailedToolResults) / float64(s.ToolResults)
	}
	type runKey struct{ tool, kind string }
	runs := make(map[runKey]int)
	for _, r := range rows {
		key := runKey{tool: r.Tool, kind: r.Kind}
		if r.Consecutive >= repeatFailureMinRun && r.Consecutive > runs[key] {
			runs[key] = r.Consecutive
		}
	}
	for key, n := range runs {
		s.Repeats = append(s.Repeats, ErrorRepeat{Tool: key.tool, Kind: key.kind, Consecutive: n})
	}
	sort.Slice(s.Repeats, func(i, j int) bool {
		if s.Repeats[i].Consecutive != s.Repeats[j].Consecutive {
			return s.Repeats[i].Consecutive > s.Repeats[j].Consecutive
		}
		if s.Repeats[i].Tool != s.Repeats[j].Tool {
			return s.Repeats[i].Tool < s.Repeats[j].Tool
		}
		return s.Repeats[i].Kind < s.Repeats[j].Kind
	})
}

// TopCount returns the key with the highest count, ties broken
// alphabetically, for the deterministic "top X" summary lines.
func TopCount(counts map[string]int) (string, int) {
	top, topN := "", 0
	for key, n := range counts {
		if n > topN || (n == topN && key < top) {
			top, topN = key, n
		}
	}
	return top, topN
}
