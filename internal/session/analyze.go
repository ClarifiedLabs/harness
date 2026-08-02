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

// AnalysisVersion is the stable schema version emitted by WriteAnalysisJSON.
const AnalysisVersion = 1

const maxAnalysisTextSessions = 100

// AnalyzeOptions limits corpus analysis to a reproducible event prefix.
type AnalyzeOptions struct {
	Before time.Time
}

// AnalysisValue distinguishes unavailable telemetry from an observed zero and
// distinguishes a measured stream in which the requested milestone never
// occurred.
type AnalysisValue struct {
	Available bool `json:"available"`
	Observed  bool `json:"observed"`
	Value     int  `json:"value"`
}

// ClosureAnalysis summarizes prompt closure diagnostics.
type ClosureAnalysis struct {
	Available           bool           `json:"available"`
	Prompts             int            `json:"prompts"`
	Triggers            map[string]int `json:"triggers"`
	TurnBudgetExhausted int            `json:"turn_budget_exhausted"`
}

// WorkflowAnalysis keeps an explicitly supplied "unknown" outcome distinct
// from prompts for which no workflow status provider was present.
type WorkflowAnalysis struct {
	Available  bool           `json:"available"`
	Prompts    int            `json:"prompts"`
	Supplied   int            `json:"supplied"`
	Unsupplied int            `json:"unsupplied"`
	Outcomes   map[string]int `json:"outcomes"`
}

// ProgressAnalysis summarizes turn_progress records. Pending batching steers
// have not yet had two subsequent tool turns or a completed prompt by the
// analyzed cutoff and therefore are not treated as failures.
type ProgressAnalysis struct {
	Available                     bool          `json:"available"`
	ToolTurns                     int           `json:"tool_turns"`
	MaxInspectionNoProgressStreak AnalysisValue `json:"max_inspection_no_progress_streak"`
	TurnsToFirstMutation          AnalysisValue `json:"turns_to_first_successful_mutation"`
	TurnsToFirstVerification      AnalysisValue `json:"turns_to_first_successful_verification"`
	BatchingSteers                int           `json:"batching_steers"`
	BatchingCompliant             int           `json:"batching_compliant_within_two_tool_turns"`
	BatchingNoncompliant          int           `json:"batching_noncompliant"`
	BatchingPending               int           `json:"batching_pending"`
}

// HookAnalysis summarizes bounded hook diagnostics.
type HookAnalysis struct {
	Available        bool `json:"available"`
	Diagnostics      int  `json:"diagnostics"`
	Timeouts         int  `json:"timeouts"`
	CircuitOpened    int  `json:"circuit_opened"`
	CircuitOpenSkips int  `json:"circuit_open_skips"`
}

// ContextAnalysis summarizes bounded context-accounting telemetry. Provider
// count scopes remain separate because payload-only counts are not interchangeable
// with effective logical-context counts.
type ContextAnalysis struct {
	Available              bool           `json:"available"`
	Samples                int            `json:"samples"`
	MaxTotalTokens         int            `json:"max_total_tokens"`
	MaxPayloadTokens       int            `json:"max_payload_tokens"`
	MaxProviderInputTokens int            `json:"max_provider_input_tokens"`
	ProviderCountScopes    map[string]int `json:"provider_count_scopes"`
}

// RetentionAnalysis summarizes non-content retention decisions and continuation
// resets from replay logs.
type RetentionAnalysis struct {
	Available               bool `json:"available"`
	Epochs                  int  `json:"epochs"`
	BlocksTrimmed           int  `json:"blocks_trimmed"`
	BytesRemoved            int  `json:"bytes_removed"`
	EstimatedTokensRemoved  int  `json:"estimated_tokens_removed"`
	ResponseStateResets     int  `json:"response_state_resets"`
	ContinuationStateResets int  `json:"continuation_state_resets"`
	MeasurementAnchorResets int  `json:"measurement_anchor_resets"`
	NextRequestStateful     int  `json:"next_request_stateful"`
	NextRequestFull         int  `json:"next_request_full"`
}

// InvariantAnalysis records impossible values without exposing event payloads.
type InvariantAnalysis struct {
	ContextAvailable                bool `json:"context_available"`
	RetentionAvailable              bool `json:"retention_available"`
	NegativeContextViolations       int  `json:"negative_context_violations"`
	InconsistentRetentionViolations int  `json:"inconsistent_retention_violations"`
}

// TelemetryAnalysis is reusable by single-session stats and corpus reports.
type TelemetryAnalysis struct {
	Closure    ClosureAnalysis   `json:"closure"`
	Workflow   WorkflowAnalysis  `json:"workflow"`
	Progress   ProgressAnalysis  `json:"progress"`
	Hooks      HookAnalysis      `json:"hooks"`
	Context    ContextAnalysis   `json:"context"`
	Retention  RetentionAnalysis `json:"retention"`
	Invariants InvariantAnalysis `json:"invariants"`
}

// AnalysisSource describes the immutable raw.ndjson prefix considered for a
// physical session stream. It deliberately contains no transcript fields.
type AnalysisSource struct {
	Path          string `json:"path"`
	Status        string `json:"status"`
	IncludedBytes int    `json:"included_bytes"`
	Events        int    `json:"events"`
	SHA256        string `json:"sha256,omitempty"`
}

// ExecutionAnalysis summarizes transcript-free loop activity and completion.
type ExecutionAnalysis struct {
	Completeness         string         `json:"completeness"`
	Prompts              int            `json:"prompts"`
	CompletedPrompts     int            `json:"completed_prompts"`
	Turns                int            `json:"turns"`
	ToolCalls            int            `json:"tool_calls"`
	ToolResults          int            `json:"tool_results"`
	ToolErrors           int            `json:"tool_errors"`
	ModelErrors          int            `json:"model_errors"`
	TerminationAvailable bool           `json:"termination_available"`
	Terminations         map[string]int `json:"terminations"`
}

// SessionAnalysis is one physical root or delegate stream.
type SessionAnalysis struct {
	Path           string            `json:"path"`
	ID             string            `json:"id,omitempty"`
	ParentID       string            `json:"parent_id,omitempty"`
	Agent          string            `json:"agent,omitempty"`
	Provider       string            `json:"provider,omitempty"`
	Model          string            `json:"model,omitempty"`
	Delegate       bool              `json:"delegate"`
	MetadataStatus string            `json:"metadata_status"`
	Source         AnalysisSource    `json:"source"`
	Execution      ExecutionAnalysis `json:"execution"`
	Telemetry      TelemetryAnalysis `json:"telemetry"`
}

// TelemetryCoverage counts physical streams carrying each structured signal.
type TelemetryCoverage struct {
	Sessions           int `json:"sessions"`
	Closure            int `json:"closure"`
	Workflow           int `json:"workflow"`
	Progress           int `json:"progress"`
	Hooks              int `json:"hooks"`
	Context            int `json:"context"`
	ProviderCountScope int `json:"provider_count_scope"`
	Retention          int `json:"retention"`
}

// AnalysisReport is a deterministic, transcript-free corpus report. Path may
// name one session root or a directory containing session roots.
type AnalysisReport struct {
	Version                int               `json:"version"`
	Path                   string            `json:"path"`
	Before                 *time.Time        `json:"before"`
	Roots                  int               `json:"roots"`
	Sessions               int               `json:"sessions"`
	MissingStreams         int               `json:"missing_streams"`
	IncompleteStreams      int               `json:"incomplete_streams"`
	MalformedStreams       int               `json:"malformed_streams"`
	SymlinkStreams         int               `json:"symlink_streams"`
	MalformedChildMetadata int               `json:"malformed_child_metadata"`
	Completeness           map[string]int    `json:"completeness"`
	Execution              ExecutionAnalysis `json:"execution"`
	Telemetry              TelemetryAnalysis `json:"telemetry"`
	Coverage               TelemetryCoverage `json:"telemetry_coverage"`
	Items                  []SessionAnalysis `json:"items"`
}

type analysisStream struct {
	dir      string
	delegate bool
	meta     ChildMeta
	metaOK   bool
	metaBad  bool
}

// AnalyzeCorpus recursively analyzes one session root or a directory of roots.
// Delegate children are owned by their nearest discovered root, symlinks are
// never followed, and every physical child directory is counted at most once.
func AnalyzeCorpus(path string, opts AnalyzeOptions) (AnalysisReport, error) {
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return AnalysisReport{}, fmt.Errorf("session: analyze corpus: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return AnalysisReport{}, fmt.Errorf("session: analyze corpus: refusing symlink %s", clean)
	}
	if !info.IsDir() {
		return AnalysisReport{}, fmt.Errorf("session: analyze corpus: %s is not a directory", clean)
	}

	roots, err := discoverAnalysisRoots(clean)
	if err != nil {
		return AnalysisReport{}, err
	}
	return analyzeSessionRoots(clean, roots, opts)
}

// AnalyzeSessionDirs analyzes a preselected set of root session directories.
// It is intended for callers that apply an outer history cutoff before walking
// each root's delegate hierarchy. Directory order does not affect the report.
func AnalyzeSessionDirs(path string, dirs []string, opts AnalyzeOptions) (AnalysisReport, error) {
	roots := make([]string, 0, len(dirs))
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		clean := filepath.Clean(dir)
		if _, ok := seen[clean]; ok {
			continue
		}
		info, err := os.Lstat(clean)
		if err != nil {
			return AnalysisReport{}, fmt.Errorf("session: analyze corpus: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return AnalysisReport{}, fmt.Errorf("session: analyze corpus: invalid session directory %s", clean)
		}
		seen[clean] = struct{}{}
		roots = append(roots, clean)
	}
	sort.Strings(roots)
	return analyzeSessionRoots(filepath.Clean(path), roots, opts)
}

func analyzeSessionRoots(path string, roots []string, opts AnalyzeOptions) (AnalysisReport, error) {
	report := AnalysisReport{
		Version: AnalysisVersion, Path: path, Roots: len(roots),
		Completeness: make(map[string]int),
		Execution:    ExecutionAnalysis{Terminations: make(map[string]int)},
	}
	if !opts.Before.IsZero() {
		before := opts.Before.UTC()
		report.Before = &before
	}
	for _, root := range roots {
		streams, err := discoverAnalysisTree(root)
		if err != nil {
			return AnalysisReport{}, err
		}
		for _, stream := range streams {
			events, source, err := readAnalysisEvents(stream.dir, opts.Before)
			if err != nil {
				return AnalysisReport{}, fmt.Errorf("session: analyze %s: %w", stream.dir, err)
			}
			item := SessionAnalysis{Path: stream.dir, Delegate: stream.delegate, Source: source}
			if stream.delegate {
				switch {
				case stream.metaBad:
					item.MetadataStatus = "malformed"
					report.MalformedChildMetadata++
				case stream.metaOK:
					item.MetadataStatus = "available"
				default:
					item.MetadataStatus = "missing"
				}
			}
			item.ID, item.ParentID, item.Agent, item.Provider, item.Model = analysisIdentity(stream.dir, stream)
			var fallback *ChildMeta
			if stream.metaOK && opts.Before.IsZero() {
				fallback = &stream.meta
			}
			item.Execution = deriveExecution(events, source.Status)
			item.Telemetry = deriveTelemetry(events, fallback)
			report.Completeness[item.Execution.Completeness]++
			report.Execution.add(item.Execution)
			report.Telemetry.add(item.Telemetry)
			report.Coverage.add(item.Telemetry, events)
			report.Items = append(report.Items, item)
			switch source.Status {
			case "missing":
				report.MissingStreams++
			case "incomplete":
				report.IncompleteStreams++
			case "malformed":
				report.MalformedStreams++
			case "symlink":
				report.SymlinkStreams++
			}
		}
	}
	report.Sessions = len(report.Items)
	sort.Slice(report.Items, func(i, j int) bool { return report.Items[i].Path < report.Items[j].Path })
	return report, nil
}

func discoverAnalysisRoots(path string) ([]string, error) {
	if analysisSessionDir(path) {
		return []string{path}, nil
	}
	var roots []string
	var walk func(string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("session: discover roots in %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				continue
			}
			child := filepath.Join(dir, entry.Name())
			if analysisSessionDir(child) {
				roots = append(roots, child)
				continue
			}
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(path); err != nil {
		return nil, err
	}
	sort.Strings(roots)
	return roots, nil
}

func analysisSessionDir(dir string) bool {
	for _, name := range []string{stateFile, eventLog, treeFile} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func discoverAnalysisTree(root string) ([]analysisStream, error) {
	streams := []analysisStream{{dir: root}}
	var children func(string) error
	children = func(parent string) error {
		childrenDir := filepath.Join(parent, "children")
		info, err := os.Lstat(childrenDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("session: discover delegates in %s: %w", parent, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.IsDir() {
			return fmt.Errorf("session: discover delegates in %s: children is not a directory", parent)
		}
		entries, err := os.ReadDir(childrenDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("session: discover delegates in %s: %w", parent, err)
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				continue
			}
			dir := filepath.Join(childrenDir, entry.Name())
			stream := analysisStream{dir: dir, delegate: true}
			metaPath := filepath.Join(dir, "meta.json")
			metaInfo, statErr := os.Lstat(metaPath)
			switch {
			case statErr == nil && metaInfo.Mode().IsRegular():
				data, readErr := os.ReadFile(metaPath)
				if readErr == nil && json.Unmarshal(data, &stream.meta) == nil {
					stream.metaOK = true
				} else {
					stream.metaBad = true
				}
			case statErr == nil:
				stream.metaBad = true
			case !errors.Is(statErr, os.ErrNotExist):
				stream.metaBad = true
			}
			streams = append(streams, stream)
			if err := children(dir); err != nil {
				return err
			}
		}
		return nil
	}
	return streams, children(root)
}

func analysisIdentity(dir string, stream analysisStream) (id, parent, agent, provider, model string) {
	if stream.metaOK {
		id, parent, agent, provider, model = stream.meta.ID, stream.meta.ParentID, stream.meta.Agent, stream.meta.Provider, stream.meta.Model
	}
	statePath := filepath.Join(dir, stateFile)
	info, err := os.Lstat(statePath)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return
	}
	var state struct {
		ID            string `json:"id"`
		ParentSession string `json:"parent_session"`
		Agent         string `json:"agent"`
		Provider      string `json:"provider"`
		Model         string `json:"model"`
	}
	if json.Unmarshal(data, &state) == nil {
		if state.ID != "" {
			id = state.ID
		}
		if state.ParentSession != "" {
			parent = state.ParentSession
		}
		if state.Agent != "" {
			agent = state.Agent
		}
		if state.Provider != "" {
			provider = state.Provider
		}
		if state.Model != "" {
			model = state.Model
		}
	}
	return
}

func readAnalysisEvents(dir string, before time.Time) ([]Event, AnalysisSource, error) {
	path := filepath.Join(dir, eventLog)
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, AnalysisSource{Path: dir, Status: "missing"}, nil
	}
	if err != nil {
		return nil, AnalysisSource{}, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, AnalysisSource{Path: dir, Status: "symlink"}, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, AnalysisSource{Path: dir, Status: "missing"}, nil
	}
	if err != nil {
		return nil, AnalysisSource{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, AnalysisSource{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, info.Size()))
	if err != nil {
		return nil, AnalysisSource{}, err
	}
	status := "complete"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		status = "incomplete"
		if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
			data = data[:end+1]
		} else {
			data = nil
		}
	}
	var events []Event
	var included bytes.Buffer
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			status = "malformed"
			continue
		}
		if !before.IsZero() && event.Time.After(before) {
			continue
		}
		events = append(events, event)
		included.Write(line)
		included.WriteByte('\n')
	}
	sum := sha256.Sum256(included.Bytes())
	return events, AnalysisSource{Path: dir, Status: status, IncludedBytes: included.Len(), Events: len(events), SHA256: hex.EncodeToString(sum[:])}, nil
}

func (c *TelemetryCoverage) add(telemetry TelemetryAnalysis, events []Event) {
	c.Sessions++
	if telemetry.Closure.Available {
		c.Closure++
	}
	if telemetry.Workflow.Available {
		c.Workflow++
	}
	if telemetry.Progress.Available {
		c.Progress++
	}
	if telemetry.Hooks.Available {
		c.Hooks++
	}
	if telemetry.Invariants.ContextAvailable {
		c.Context++
	}
	if telemetry.Invariants.RetentionAvailable {
		c.Retention++
	}
	for _, event := range events {
		if event.Context != nil && event.Context.ProviderInputScope != "" {
			c.ProviderCountScope++
			break
		}
	}
}

func deriveExecution(events []Event, sourceStatus string) ExecutionAnalysis {
	out := ExecutionAnalysis{Terminations: make(map[string]int)}
	prompts := make(map[int]struct{})
	completed := make(map[int]struct{})
	turns := make(map[[2]int]struct{})
	for _, event := range events {
		if event.Prompt > 0 {
			prompts[event.Prompt] = struct{}{}
		}
		if event.Prompt > 0 && event.Turn > 0 {
			switch event.Type {
			case EventTurnComplete, EventTurnProgress, EventTurnAttemptStart:
				turns[[2]int{event.Prompt, event.Turn}] = struct{}{}
			}
		}
		switch event.Type {
		case EventPromptUsage:
			completed[event.Prompt] = struct{}{}
			if event.TerminationReason != "" {
				out.TerminationAvailable = true
				out.Terminations[normalizedTermination(event.TerminationReason)]++
			}
		case EventToolStart:
			out.ToolCalls++
		case EventToolResult:
			out.ToolResults++
			if event.ResultError {
				out.ToolErrors++
			}
		case EventModelRequest:
			if event.ModelRequest != nil && event.ModelRequest.State == llm.ModelRequestFailed {
				out.ModelErrors++
			}
		}
	}
	out.Prompts = len(prompts)
	out.CompletedPrompts = len(completed)
	out.Turns = len(turns)
	switch {
	case sourceStatus == "missing" || sourceStatus == "symlink":
		out.Completeness = "unavailable"
	case sourceStatus == "incomplete" || sourceStatus == "malformed":
		out.Completeness = "incomplete"
	case out.Prompts == 0:
		out.Completeness = "unknown"
	case out.CompletedPrompts == out.Prompts:
		out.Completeness = "complete"
	default:
		out.Completeness = "incomplete"
	}
	return out
}

func (e *ExecutionAnalysis) add(other ExecutionAnalysis) {
	if e.Terminations == nil {
		e.Terminations = make(map[string]int)
	}
	e.Prompts += other.Prompts
	e.CompletedPrompts += other.CompletedPrompts
	e.Turns += other.Turns
	e.ToolCalls += other.ToolCalls
	e.ToolResults += other.ToolResults
	e.ToolErrors += other.ToolErrors
	e.ModelErrors += other.ModelErrors
	e.TerminationAvailable = e.TerminationAvailable || other.TerminationAvailable
	for reason, count := range other.Terminations {
		e.Terminations[reason] += count
	}
	if completenessRank(other.Completeness) > completenessRank(e.Completeness) {
		e.Completeness = other.Completeness
	}
}

func completenessRank(value string) int {
	switch value {
	case "incomplete":
		return 4
	case "unavailable":
		return 3
	case "unknown":
		return 2
	case "complete":
		return 1
	default:
		return 0
	}
}

func normalizedTermination(value string) string {
	switch value = strings.TrimSpace(value); value {
	case "model_completed", "turn_limit", "cancelled", "repeat_guard", "error_guard", "error":
		return value
	default:
		return "unknown"
	}
}

func deriveTelemetry(events []Event, child *ChildMeta) TelemetryAnalysis {
	out := TelemetryAnalysis{
		Closure:  ClosureAnalysis{Triggers: make(map[string]int)},
		Workflow: WorkflowAnalysis{Outcomes: make(map[string]int)},
		Context:  ContextAnalysis{ProviderCountScopes: make(map[string]int)},
	}
	closures := make(map[int]string)
	closurePrompts := make(map[int]struct{})
	completed := make(map[int]bool)
	workflow := make(map[int]*WorkflowStatusSnapshot)
	var progress []Event
	schemaAvailable := false
	for _, event := range events {
		if event.TelemetryVersion > 0 {
			schemaAvailable = true
		}
		if context := event.Context; context != nil {
			out.Context.Available = true
			out.Context.Samples++
			out.Context.MaxTotalTokens = max(out.Context.MaxTotalTokens, context.Total)
			out.Context.MaxPayloadTokens = max(out.Context.MaxPayloadTokens, context.PayloadTotal)
			out.Context.MaxProviderInputTokens = max(out.Context.MaxProviderInputTokens, context.ProviderInputTokens)
			if context.ProviderInputScope != "" {
				out.Context.ProviderCountScopes[normalizedProviderCountScope(context.ProviderInputScope)]++
			}
			out.Invariants.ContextAvailable = true
			if negativeContext(context) {
				out.Invariants.NegativeContextViolations++
			}
		}
		switch event.Type {
		case EventClosure:
			out.Closure.Available = true
			closurePrompts[event.Prompt] = struct{}{}
			closures[event.Prompt] = normalizedClosureTrigger(event.ClosureTrigger)
			if event.TelemetryVersion > 0 || event.WorkflowStatus != nil {
				out.Workflow.Available = true
				workflow[event.Prompt] = event.WorkflowStatus
			}
		case EventPromptUsage:
			completed[event.Prompt] = true
			if event.TelemetryVersion > 0 || event.ClosureTrigger != "" || event.TurnBudgetExhausted {
				out.Closure.Available = true
				closurePrompts[event.Prompt] = struct{}{}
			}
			if _, ok := closures[event.Prompt]; !ok && event.ClosureTrigger != "" {
				closures[event.Prompt] = normalizedClosureTrigger(event.ClosureTrigger)
			}
			if event.TurnBudgetExhausted {
				out.Closure.TurnBudgetExhausted++
			}
			if event.TelemetryVersion > 0 || event.WorkflowStatus != nil {
				out.Workflow.Available = true
				workflow[event.Prompt] = event.WorkflowStatus
			}
		case EventTurnProgress:
			if event.TurnProgress != nil {
				progress = append(progress, event)
			}
		case EventHookDiagnostic:
			if hook := event.HookDiagnostic; hook != nil {
				out.Hooks.Available = true
				out.Hooks.Diagnostics++
				if hook.Outcome == "timeout" {
					out.Hooks.Timeouts++
				}
				if hook.CircuitOpen && hook.Outcome != "circuit_open" {
					out.Hooks.CircuitOpened++
				}
				if hook.Outcome == "circuit_open" {
					out.Hooks.CircuitOpenSkips++
				}
			}
		case EventRetention:
			if retention := event.Retention; retention != nil {
				out.Retention.Available = true
				out.Retention.Epochs++
				out.Retention.BlocksTrimmed += retention.BlocksTrimmed
				out.Retention.BytesRemoved += retention.BytesRemoved
				out.Retention.EstimatedTokensRemoved += retention.EstimatedTokensRemoved
				if retention.ResponseStateReset {
					out.Retention.ResponseStateResets++
				}
				if retention.ContinuationStateReset {
					out.Retention.ContinuationStateResets++
				}
				if retention.MeasurementAnchorReset {
					out.Retention.MeasurementAnchorResets++
				}
				if retention.NextRequestStateful {
					out.Retention.NextRequestStateful++
				} else {
					out.Retention.NextRequestFull++
				}
				out.Invariants.RetentionAvailable = true
				if negativeRetentionContext(retention) {
					out.Invariants.NegativeContextViolations++
				}
				if inconsistentRetention(retention) {
					out.Invariants.InconsistentRetentionViolations++
				}
			}
		}
	}
	if len(events) == 0 && child != nil && (child.TelemetryVersion > 0 || child.ClosureTrigger != "" || child.TurnBudgetExhausted || child.WorkflowStatus != nil) {
		out.Closure.Available = true
		closurePrompts[0] = struct{}{}
		if child.ClosureTrigger != "" {
			closures[0] = normalizedClosureTrigger(child.ClosureTrigger)
		}
		if child.TurnBudgetExhausted {
			out.Closure.TurnBudgetExhausted = 1
		}
		if child.TelemetryVersion > 0 || child.WorkflowStatus != nil {
			out.Workflow.Available = true
			workflow[0] = child.WorkflowStatus
		}
	}
	for _, status := range workflow {
		out.Workflow.Prompts++
		if status == nil {
			out.Workflow.Unsupplied++
			continue
		}
		out.Workflow.Supplied++
		out.Workflow.Outcomes[normalizedWorkflowOutcome(status.Outcome)]++
	}
	out.Closure.Prompts = len(closurePrompts)
	for _, trigger := range closures {
		out.Closure.Triggers[trigger]++
	}
	if schemaAvailable {
		out.Hooks.Available = true
	}
	out.Progress = deriveProgress(progress, completed, schemaAvailable)
	return out
}

func deriveProgress(events []Event, completed map[int]bool, schemaAvailable bool) ProgressAnalysis {
	out := ProgressAnalysis{Available: schemaAvailable}
	if len(events) == 0 {
		if schemaAvailable {
			out.MaxInspectionNoProgressStreak.Available = true
			out.TurnsToFirstMutation.Available = true
			out.TurnsToFirstVerification.Available = true
		}
		return out
	}
	out.Available = true
	out.ToolTurns = len(events)
	out.MaxInspectionNoProgressStreak = AnalysisValue{Available: true, Observed: true}
	out.TurnsToFirstMutation.Available = true
	out.TurnsToFirstVerification.Available = true
	for i, event := range events {
		progress := event.TurnProgress
		out.MaxInspectionNoProgressStreak.Value = max(out.MaxInspectionNoProgressStreak.Value, progress.InspectionNoProgressRun)
		turn := event.Turn
		if turn <= 0 {
			turn = i + 1
		}
		if progress.SuccessfulMutation && !out.TurnsToFirstMutation.Observed {
			out.TurnsToFirstMutation.Observed, out.TurnsToFirstMutation.Value = true, turn
		}
		if progress.SuccessfulVerification && !out.TurnsToFirstVerification.Observed {
			out.TurnsToFirstVerification.Observed, out.TurnsToFirstVerification.Value = true, turn
		}
		if progress.SteerReason != "batching" {
			continue
		}
		out.BatchingSteers++
		following := 0
		compliant := false
		for j := i + 1; j < len(events) && following < 2; j++ {
			if events[j].Prompt != event.Prompt {
				continue
			}
			following++
			if events[j].TurnProgress.BatchedOperationCount > 0 {
				compliant = true
				break
			}
		}
		switch {
		case compliant:
			out.BatchingCompliant++
		case following == 2 || completed[event.Prompt]:
			out.BatchingNoncompliant++
		default:
			out.BatchingPending++
		}
	}
	return out
}

func normalizedLabel(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "unknown"
}

func normalizedProviderCountScope(value string) string {
	switch strings.TrimSpace(value) {
	case string(llm.InputTokenCountScopeEffectiveContext):
		return string(llm.InputTokenCountScopeEffectiveContext)
	case string(llm.InputTokenCountScopeRequestPayload):
		return string(llm.InputTokenCountScopeRequestPayload)
	default:
		return string(llm.InputTokenCountScopeUnknown)
	}
}

func normalizedClosureTrigger(value string) string {
	switch value = strings.TrimSpace(value); value {
	case "turn_budget", "stagnation", "repeat_guard", "error_guard":
		return value
	default:
		return "unknown"
	}
}

func normalizedWorkflowOutcome(value string) string {
	switch value = strings.TrimSpace(value); value {
	case "complete", "waiting", "blocked", "escalated", "in_progress", "unknown":
		return value
	default:
		return "unknown"
	}
}

func negativeContext(c *ContextSnapshot) bool {
	return c.Total < 0 || c.Window < 0 || c.System < 0 || c.Tools < 0 || c.Messages < 0 ||
		c.PayloadTotal < 0 || c.PayloadSystem < 0 || c.PayloadTools < 0 || c.PayloadMessages < 0 || c.ProviderInputTokens < 0
}

func negativeRetentionContext(r *RetentionSnapshot) bool {
	return r.ContextTokensBefore < 0 || r.ContextTokensAfter < 0 || r.DecisionContextTokens < 0 ||
		r.LocalEstimateTokensBefore < 0 || r.LocalEstimateTokensAfter < 0 || r.EstimatedTokensRemoved < 0
}

func inconsistentRetention(r *RetentionSnapshot) bool {
	if r.BlocksTrimmed < 0 || r.BytesBefore < 0 || r.BytesAfter < 0 || r.BytesRemoved < 0 {
		return true
	}
	if r.BytesAfter > r.BytesBefore || r.LocalEstimateTokensAfter > r.LocalEstimateTokensBefore {
		return true
	}
	if r.BytesRemoved != 0 && r.BytesRemoved != r.BytesBefore-r.BytesAfter {
		return true
	}
	return r.EstimatedTokensRemoved != 0 && r.EstimatedTokensRemoved != r.LocalEstimateTokensBefore-r.LocalEstimateTokensAfter
}

func (t *TelemetryAnalysis) add(other TelemetryAnalysis) {
	if t.Closure.Triggers == nil {
		t.Closure.Triggers = make(map[string]int)
	}
	for key, value := range other.Closure.Triggers {
		t.Closure.Triggers[key] += value
	}
	t.Closure.Available = t.Closure.Available || other.Closure.Available
	t.Closure.Prompts += other.Closure.Prompts
	t.Closure.TurnBudgetExhausted += other.Closure.TurnBudgetExhausted
	if t.Workflow.Outcomes == nil {
		t.Workflow.Outcomes = make(map[string]int)
	}
	for key, value := range other.Workflow.Outcomes {
		t.Workflow.Outcomes[key] += value
	}
	t.Workflow.Available = t.Workflow.Available || other.Workflow.Available
	t.Workflow.Prompts += other.Workflow.Prompts
	t.Workflow.Supplied += other.Workflow.Supplied
	t.Workflow.Unsupplied += other.Workflow.Unsupplied
	t.Progress.Available = t.Progress.Available || other.Progress.Available
	t.Progress.ToolTurns += other.Progress.ToolTurns
	mergeMaximum(&t.Progress.MaxInspectionNoProgressStreak, other.Progress.MaxInspectionNoProgressStreak)
	mergeMinimum(&t.Progress.TurnsToFirstMutation, other.Progress.TurnsToFirstMutation)
	mergeMinimum(&t.Progress.TurnsToFirstVerification, other.Progress.TurnsToFirstVerification)
	t.Progress.BatchingSteers += other.Progress.BatchingSteers
	t.Progress.BatchingCompliant += other.Progress.BatchingCompliant
	t.Progress.BatchingNoncompliant += other.Progress.BatchingNoncompliant
	t.Progress.BatchingPending += other.Progress.BatchingPending
	t.Hooks.Available = t.Hooks.Available || other.Hooks.Available
	t.Hooks.Diagnostics += other.Hooks.Diagnostics
	t.Hooks.Timeouts += other.Hooks.Timeouts
	t.Hooks.CircuitOpened += other.Hooks.CircuitOpened
	t.Hooks.CircuitOpenSkips += other.Hooks.CircuitOpenSkips
	t.Context.Available = t.Context.Available || other.Context.Available
	t.Context.Samples += other.Context.Samples
	t.Context.MaxTotalTokens = max(t.Context.MaxTotalTokens, other.Context.MaxTotalTokens)
	t.Context.MaxPayloadTokens = max(t.Context.MaxPayloadTokens, other.Context.MaxPayloadTokens)
	t.Context.MaxProviderInputTokens = max(t.Context.MaxProviderInputTokens, other.Context.MaxProviderInputTokens)
	if t.Context.ProviderCountScopes == nil {
		t.Context.ProviderCountScopes = make(map[string]int)
	}
	for scope, count := range other.Context.ProviderCountScopes {
		t.Context.ProviderCountScopes[scope] += count
	}
	t.Retention.Available = t.Retention.Available || other.Retention.Available
	t.Retention.Epochs += other.Retention.Epochs
	t.Retention.BlocksTrimmed += other.Retention.BlocksTrimmed
	t.Retention.BytesRemoved += other.Retention.BytesRemoved
	t.Retention.EstimatedTokensRemoved += other.Retention.EstimatedTokensRemoved
	t.Retention.ResponseStateResets += other.Retention.ResponseStateResets
	t.Retention.ContinuationStateResets += other.Retention.ContinuationStateResets
	t.Retention.MeasurementAnchorResets += other.Retention.MeasurementAnchorResets
	t.Retention.NextRequestStateful += other.Retention.NextRequestStateful
	t.Retention.NextRequestFull += other.Retention.NextRequestFull
	t.Invariants.ContextAvailable = t.Invariants.ContextAvailable || other.Invariants.ContextAvailable
	t.Invariants.RetentionAvailable = t.Invariants.RetentionAvailable || other.Invariants.RetentionAvailable
	t.Invariants.NegativeContextViolations += other.Invariants.NegativeContextViolations
	t.Invariants.InconsistentRetentionViolations += other.Invariants.InconsistentRetentionViolations
}

func mergeMaximum(dst *AnalysisValue, src AnalysisValue) {
	if !src.Available {
		return
	}
	if !dst.Available || src.Value > dst.Value {
		dst.Value = src.Value
	}
	dst.Available, dst.Observed = true, dst.Observed || src.Observed
}

func mergeMinimum(dst *AnalysisValue, src AnalysisValue) {
	if !src.Available {
		return
	}
	dst.Available = true
	if src.Observed && (!dst.Observed || src.Value < dst.Value) {
		dst.Observed, dst.Value = true, src.Value
	}
}

// WriteAnalysisJSON writes deterministic, versioned JSON with no transcript,
// tool input, result body, hook payload, or assistant text fields.
func WriteAnalysisJSON(report AnalysisReport, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("session: write analysis json: %w", err)
	}
	return nil
}

// WriteAnalysisText writes a bounded human-readable corpus summary.
func WriteAnalysisText(report AnalysisReport, w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Session analysis v%d\n", report.Version)
	fmt.Fprintf(&b, "  path: %s\n", report.Path)
	fmt.Fprintf(&b, "  roots/sessions: %d / %d\n", report.Roots, report.Sessions)
	fmt.Fprintf(&b, "  streams missing/incomplete/malformed/symlink: %d / %d / %d / %d\n", report.MissingStreams, report.IncompleteStreams, report.MalformedStreams, report.SymlinkStreams)
	fmt.Fprintf(&b, "  malformed child metadata: %d\n", report.MalformedChildMetadata)
	fmt.Fprintf(&b, "  completeness: %s\n", formatCountMap(true, report.Completeness))
	fmt.Fprintf(&b, "  prompts completed/observed: %d / %d\n", report.Execution.CompletedPrompts, report.Execution.Prompts)
	fmt.Fprintf(&b, "  turns/tools/results/errors: %d / %d / %d / %d tool, %d model\n", report.Execution.Turns, report.Execution.ToolCalls, report.Execution.ToolResults, report.Execution.ToolErrors, report.Execution.ModelErrors)
	fmt.Fprintf(&b, "  terminations: %s\n", formatCountMap(report.Execution.TerminationAvailable, report.Execution.Terminations))
	fmt.Fprintf(&b, "  telemetry coverage closure/workflow/progress/hooks/context/count-scope/retention: %d/%d/%d/%d/%d/%d/%d of %d\n", report.Coverage.Closure, report.Coverage.Workflow, report.Coverage.Progress, report.Coverage.Hooks, report.Coverage.Context, report.Coverage.ProviderCountScope, report.Coverage.Retention, report.Coverage.Sessions)
	writeTelemetryText(&b, "  ", report.Telemetry)
	limit := min(len(report.Items), maxAnalysisTextSessions)
	fmt.Fprintf(&b, "Streams (showing %d of %d)\n", limit, len(report.Items))
	for _, item := range report.Items[:limit] {
		fmt.Fprintf(&b, "  %s: %s, %s, %d turns, %d tools, %d errors, %d events\n", item.Path, item.Source.Status, item.Execution.Completeness, item.Execution.Turns, item.Execution.ToolCalls, item.Execution.ToolErrors+item.Execution.ModelErrors, item.Source.Events)
	}
	if omitted := len(report.Items) - limit; omitted > 0 {
		fmt.Fprintf(&b, "  ... %d more streams omitted\n", omitted)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("session: write analysis text: %w", err)
	}
	return nil
}

func writeTelemetryText(w io.Writer, indent string, t TelemetryAnalysis) {
	fmt.Fprintf(w, "%sclosure triggers: %s\n", indent, formatCountMap(t.Closure.Available, t.Closure.Triggers))
	fmt.Fprintf(w, "%sturn budgets exhausted: %s of %s covered prompts\n", indent, availableCount(t.Closure.Available, t.Closure.TurnBudgetExhausted), availableCount(t.Closure.Available, t.Closure.Prompts))
	if t.Workflow.Available {
		fmt.Fprintf(w, "%sworkflow supplied/unsupplied: %d / %d; outcomes: %s\n", indent, t.Workflow.Supplied, t.Workflow.Unsupplied, formatCountMap(true, t.Workflow.Outcomes))
	} else {
		fmt.Fprintf(w, "%sworkflow outcomes: unavailable\n", indent)
	}
	if t.Progress.Available {
		fmt.Fprintf(w, "%sprogress telemetry: available (%d tool turns)\n", indent, t.Progress.ToolTurns)
		fmt.Fprintf(w, "%smax inspection/no-progress streak: %d\n", indent, t.Progress.MaxInspectionNoProgressStreak.Value)
		fmt.Fprintf(w, "%sturns to first successful mutation/verification: %s / %s\n", indent, formatAnalysisValue(t.Progress.TurnsToFirstMutation), formatAnalysisValue(t.Progress.TurnsToFirstVerification))
		fmt.Fprintf(w, "%sbatching steers compliant/noncompliant/pending: %d / %d / %d / %d total\n", indent, t.Progress.BatchingCompliant, t.Progress.BatchingNoncompliant, t.Progress.BatchingPending, t.Progress.BatchingSteers)
	} else {
		fmt.Fprintf(w, "%sprogress telemetry: unavailable\n", indent)
	}
	if t.Hooks.Available {
		fmt.Fprintf(w, "%shook timeouts/circuit-openings/circuit-skips: %d / %d / %d\n", indent, t.Hooks.Timeouts, t.Hooks.CircuitOpened, t.Hooks.CircuitOpenSkips)
	} else {
		fmt.Fprintf(w, "%shook diagnostics: unavailable\n", indent)
	}
	if t.Context.Available {
		fmt.Fprintf(w, "%scontext samples/max total/max payload/max provider: %d / %d / %d / %d; provider scopes: %s\n", indent, t.Context.Samples, t.Context.MaxTotalTokens, t.Context.MaxPayloadTokens, t.Context.MaxProviderInputTokens, formatCountMap(true, t.Context.ProviderCountScopes))
	} else {
		fmt.Fprintf(w, "%scontext telemetry: unavailable\n", indent)
	}
	if t.Retention.Available {
		fmt.Fprintf(w, "%sretention epochs/blocks/bytes/tokens removed: %d / %d / %d / %d; continuation resets: %d\n", indent, t.Retention.Epochs, t.Retention.BlocksTrimmed, t.Retention.BytesRemoved, t.Retention.EstimatedTokensRemoved, t.Retention.ContinuationStateResets)
	} else {
		fmt.Fprintf(w, "%sretention telemetry: unavailable\n", indent)
	}
	fmt.Fprintf(w, "%snegative-context violations: %s\n", indent, availableCount(t.Invariants.ContextAvailable || t.Invariants.RetentionAvailable, t.Invariants.NegativeContextViolations))
	fmt.Fprintf(w, "%sinconsistent-retention violations: %s\n", indent, availableCount(t.Invariants.RetentionAvailable, t.Invariants.InconsistentRetentionViolations))
}

func availableCount(available bool, value int) string {
	if !available {
		return "unavailable"
	}
	return fmt.Sprint(value)
}

func formatAnalysisValue(value AnalysisValue) string {
	if !value.Available {
		return "unavailable"
	}
	if !value.Observed {
		return "not observed"
	}
	return fmt.Sprint(value.Value)
}

func formatCountMap(available bool, values map[string]int) string {
	if !available {
		return "unavailable"
	}
	if len(values) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	return strings.Join(parts, ", ")
}
