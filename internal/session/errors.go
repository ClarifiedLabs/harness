package session

import (
	"errors"
	"fmt"
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
	Session    string    `json:"session"`
	Agent      string    `json:"agent,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
	Prompt     int       `json:"prompt,omitempty"`
	Turn       int       `json:"turn,omitempty"`
	ContextPct int       `json:"context_pct,omitempty"`
	Tool       string    `json:"tool,omitempty"`
	Kind       string    `json:"kind"`
	Confidence string    `json:"confidence"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	At         time.Time `json:"at"`
	Excerpt    string    `json:"excerpt,omitempty"`
}

// CollectErrors reads one session (root plus its delegate children) and
// returns the classified failure rows in chronological order, filtered by
// filter. Model request failures come from model_request events; tool
// failures from tool_result events with result_error.
func CollectErrors(dir string, filter ErrorFilter) ([]ErrorRow, error) {
	state, err := Load(dir)
	if err != nil {
		return nil, fmt.Errorf("collect errors in %s: %w", dir, err)
	}
	var rows []ErrorRow
	events, err := readEvents(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read replay in %s: %w", dir, err)
	}
	rows = append(rows, collectErrorRows(events, dir, state.Agent, state.Provider, state.Model)...)

	children, err := delegateChildren(dir)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		events, err := readEvents(child.dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read replay in %s: %w", child.dir, err)
		}
		rows = append(rows, collectErrorRows(events, child.dir, child.meta.Agent, child.meta.Provider, child.meta.Model)...)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].At.Equal(rows[j].At) {
			return rows[i].At.Before(rows[j].At)
		}
		return rows[i].Session < rows[j].Session
	})

	filtered := rows[:0]
	for _, r := range rows {
		if filter.matches(r) {
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
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

func collectErrorRows(events []Event, sessionDir, agent, provider, model string) []ErrorRow {
	var rows []ErrorRow
	var latestContext *ContextSnapshot
	for _, ev := range events {
		if ev.Context != nil {
			snapshot := *ev.Context
			latestContext = &snapshot
		}
		switch {
		case ev.Type == EventToolResult && ev.ResultError:
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
			rows = append(rows, ErrorRow{
				Session:    sessionDir,
				Agent:      agent,
				Provider:   provider,
				Model:      model,
				Prompt:     ev.Prompt,
				Turn:       ev.Turn,
				ContextPct: contextPct(latestContext),
				Tool:       ev.Tool,
				Kind:       kind,
				Confidence: confidence,
				DurationMS: ev.DurationMS,
				At:         ev.Time,
				Excerpt:    excerpt,
			})
		case ev.Type == EventModelRequest && ev.ModelRequest != nil && ev.ModelRequest.State == llm.ModelRequestFailed:
			kind := ClassifyModelRequestFailure(ev.ModelRequest)
			confidence := "high"
			if kind == llm.ToolErrorProviderError {
				confidence = "low"
			}
			rowProvider, rowModel := provider, model
			if ev.ModelRequest.Provider != "" {
				rowProvider = ev.ModelRequest.Provider
			}
			if ev.ModelRequest.Model != "" {
				rowModel = ev.ModelRequest.Model
			}
			rows = append(rows, ErrorRow{
				Session:    sessionDir,
				Agent:      agent,
				Provider:   rowProvider,
				Model:      rowModel,
				Prompt:     ev.Prompt,
				Turn:       ev.Turn,
				ContextPct: contextPct(latestContext),
				Kind:       string(kind),
				Confidence: confidence,
				At:         ev.Time,
				Excerpt:    ErrorExcerpt(ev.ModelRequest.Message),
			})
		}
	}
	return rows
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
	FailedToolResults    int            `json:"failed_tool_results"`
	ModelRequestFailures int            `json:"model_request_failures"`
	ByKind               map[string]int `json:"by_kind,omitempty"`
	ByTool               map[string]int `json:"by_tool,omitempty"`
	ByModel              map[string]int `json:"by_model,omitempty"`
	Repeats              []ErrorRepeat  `json:"repeats,omitempty"`
}

// repeatFailureMinRun is the consecutive-run length that counts as a
// repeat-failure loop.
const repeatFailureMinRun = 3

// SummarizeErrors aggregates rows, which must be in chronological order (as
// CollectErrors returns them).
func SummarizeErrors(rows []ErrorRow) ErrorSummary {
	summary := ErrorSummary{
		ByKind:  make(map[string]int),
		ByTool:  make(map[string]int),
		ByModel: make(map[string]int),
	}
	type runKey struct{ tool, kind string }
	runs := make(map[runKey]int)
	var prev runKey
	prevLen := 0
	finishRun := func() {
		if prevLen >= repeatFailureMinRun && prevLen > runs[prev] {
			runs[prev] = prevLen
		}
	}
	for _, r := range rows {
		if r.Tool == "" {
			summary.ModelRequestFailures++
		} else {
			summary.FailedToolResults++
			summary.ByTool[r.Tool]++
		}
		summary.ByKind[r.Kind]++
		if r.Model != "" {
			summary.ByModel[r.Model]++
		}
		key := runKey{tool: r.Tool, kind: r.Kind}
		if key == prev {
			prevLen++
		} else {
			finishRun()
			prev, prevLen = key, 1
		}
	}
	finishRun()
	for key, n := range runs {
		summary.Repeats = append(summary.Repeats, ErrorRepeat{Tool: key.tool, Kind: key.kind, Consecutive: n})
	}
	sort.Slice(summary.Repeats, func(i, j int) bool {
		if summary.Repeats[i].Consecutive != summary.Repeats[j].Consecutive {
			return summary.Repeats[i].Consecutive > summary.Repeats[j].Consecutive
		}
		if summary.Repeats[i].Tool != summary.Repeats[j].Tool {
			return summary.Repeats[i].Tool < summary.Repeats[j].Tool
		}
		return summary.Repeats[i].Kind < summary.Repeats[j].Kind
	})
	return summary
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
