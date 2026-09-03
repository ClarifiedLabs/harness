package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"harness/internal/cli"
	"harness/internal/session"
	"harness/internal/ui"
)

// sessionDirTimestampLayout matches the directory names DefaultPath creates
// under the sessions root.
const sessionDirTimestampLayout = "20060102T150405Z"

// runSessionErrors implements `harness session errors`: classified tool and
// model failures for one session directory, or scanned across recent sessions
// under the default sessions root when no directory argument is given.
func runSessionErrors(env environment, invocation cli.Invocation) int {
	values := invocation.Flags
	tool := cliLast(values, "tool", "")
	kind := cliLast(values, "kind", "")
	model := cliLast(values, "model", "")
	agent := cliLast(values, "agent", "")
	since := cliLast(values, "since", "24h")
	all := cliBool(values, "all")
	format := cliLast(values, "format", "text")
	beforeValue := cliLast(values, "before", "")
	if format != "text" && format != "json" {
		fmt.Fprintf(env.stderr, "session errors: unsupported --format %q (want text or json)\n", format)
		return ui.ExitUsage
	}
	filter := session.ErrorFilter{Tool: tool, Kind: kind, Model: model, Agent: agent}
	var before time.Time
	if strings.TrimSpace(beforeValue) != "" {
		var err error
		before, err = time.Parse(time.RFC3339, beforeValue)
		if err != nil {
			fmt.Fprintf(env.stderr, "session errors: invalid --before %q: want RFC3339\n", beforeValue)
			return ui.ExitUsage
		}
	}
	analyzedAt := time.Now().UTC()
	if env.now != nil {
		analyzedAt = env.now().UTC()
	}

	if len(invocation.Args) == 1 {
		dir, err := session.ResolveSessionDir(stateDir(env.lookup), invocation.Args[0])
		if err != nil {
			fmt.Fprintf(env.stderr, "session errors: %v\n", err)
			return ui.ExitUsage
		}
		analysis, err := session.AnalyzeErrors(dir, filter, before)
		if err != nil {
			fmt.Fprintf(env.stderr, "session errors: %v\n", err)
			return ui.ExitRuntime
		}
		report := sessionErrorsReport{
			Scope:           map[string]any{"dir": dir, "before": beforeValue},
			SessionsScanned: 1,
			AnalyzedAt:      analyzedAt,
			Summary:         analysis.Summary,
			Rows:            analysis.Rows,
			Sources:         analysis.Sources,
		}
		return writeSessionErrors(env.stdout, format, report, []sessionErrorBlock{{dir: dir, rows: analysis.Rows, summary: analysis.Summary}}, 0)
	}

	window, err := time.ParseDuration(since)
	if err != nil || window < 0 {
		fmt.Fprintf(env.stderr, "session errors: invalid --since %q: must be a duration like 24h or 720h\n", since)
		return ui.ExitUsage
	}
	now := time.Now
	if env.now != nil {
		now = env.now
	}
	root := filepath.Join(stateDir(env.lookup), "harness", "sessions")
	dirs, err := sessionDirsSince(root, now().Add(-window), all)
	if err != nil {
		fmt.Fprintf(env.stderr, "session errors: %v\n", err)
		return ui.ExitRuntime
	}
	var blocks []sessionErrorBlock
	var analyses []session.ErrorAnalysis
	var skipped []sessionSkippedSession
	for _, dir := range dirs {
		analysis, err := session.AnalyzeErrors(dir, filter, before)
		if err != nil {
			skipped = append(skipped, sessionSkippedSession{Dir: dir, Reason: err.Error()})
			continue
		}
		analyses = append(analyses, analysis)
		if len(analysis.Rows) > 0 || analysis.Summary.FailedCommandResults > 0 {
			blocks = append(blocks, sessionErrorBlock{dir: dir, rows: analysis.Rows, summary: analysis.Summary})
		}
	}
	merged := session.MergeErrorAnalyses(analyses...)
	report := sessionErrorsReport{
		Scope:           map[string]any{"sessions_root": root, "since": since, "all": all, "before": beforeValue},
		SessionsScanned: len(analyses),
		AnalyzedAt:      analyzedAt,
		Summary:         merged.Summary,
		Rows:            merged.Rows,
		Sources:         merged.Sources,
		SkippedSessions: skipped,
	}
	return writeSessionErrors(env.stdout, format, report, blocks, len(analyses))
}

// sessionErrorBlock groups one session's rows for the text renderer.
type sessionErrorBlock struct {
	dir     string
	rows    []session.ErrorRow
	summary session.ErrorSummary
}

// sessionErrorsReport is the JSON output shape.
type sessionErrorsReport struct {
	Scope           map[string]any          `json:"scope"`
	SessionsScanned int                     `json:"sessions_scanned"`
	AnalyzedAt      time.Time               `json:"analyzed_at"`
	Summary         session.ErrorSummary    `json:"summary"`
	Rows            []session.ErrorRow      `json:"rows"`
	Sources         []session.ErrorSource   `json:"sources"`
	SkippedSessions []sessionSkippedSession `json:"skipped_sessions,omitempty"`
}

type sessionSkippedSession struct {
	Dir    string `json:"dir"`
	Reason string `json:"reason"`
}

// writeSessionErrors renders the report as JSON, or as per-session text
// blocks followed by a scan footer when scanning (scanned > 0).
func writeSessionErrors(w io.Writer, format string, report sessionErrorsReport, blocks []sessionErrorBlock, scanned int) int {
	rows := report.Rows
	if rows == nil {
		rows = []session.ErrorRow{}
	}
	report.Rows = rows
	if report.Sources == nil {
		report.Sources = []session.ErrorSource{}
	}
	if report.Summary.ByKind == nil {
		report.Summary = session.SummarizeErrors(rows)
	}
	if format == "json" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(w, "session errors: encode json: %v\n", err)
			return ui.ExitRuntime
		}
		if _, err := fmt.Fprintln(w, string(data)); err != nil {
			return ui.ExitRuntime
		}
		return ui.ExitOK
	}
	for _, block := range blocks {
		writeSessionErrorBlock(w, block.dir, block.rows, block.summary)
	}
	if scanned > 0 {
		summary := report.Summary
		fmt.Fprintf(w, "Scanned %d sessions: %d/%d failed tool results (%.1f%%), %d model request failures\n",
			scanned, summary.FailedToolResults, summary.ToolResults, summary.ToolErrorRate*100, summary.ModelRequestFailures)
		if summary.CommandResults > 0 {
			fmt.Fprintf(w, "Command execution failures: %d/%d (%.1f%%); effective failures: %d/%d (%.1f%%); cancelled: %d\n",
				summary.FailedCommandResults, summary.CommandResults, summary.CommandFailureRate*100,
				summary.EffectiveFailedResults, summary.ToolResults, summary.EffectiveFailureRate*100,
				summary.CancelledCommandResults)
		}
		if len(report.SkippedSessions) > 0 {
			fmt.Fprintf(w, "Skipped %d unreadable or unsupported sessions\n", len(report.SkippedSessions))
		}
	}
	return ui.ExitOK
}

func writeSessionErrorBlock(w io.Writer, dir string, rows []session.ErrorRow, summary session.ErrorSummary) {
	if summary.ByKind == nil {
		summary = session.SummarizeErrors(rows)
	}
	fmt.Fprintf(w, "Session errors: %s\n", dir)
	fmt.Fprintf(w, "  failed tool results: %d   model request failures: %d\n",
		summary.FailedToolResults, summary.ModelRequestFailures)
	if summary.CommandResults > 0 {
		fmt.Fprintf(w, "  command execution failures: %d/%d (%.1f%%)   effective failures: %d/%d (%.1f%%)   cancelled: %d\n",
			summary.FailedCommandResults, summary.CommandResults, summary.CommandFailureRate*100,
			summary.EffectiveFailedResults, summary.ToolResults, summary.EffectiveFailureRate*100,
			summary.CancelledCommandResults)
	}
	if len(summary.ByTool) > 0 {
		top, n := session.TopCount(summary.ByTool)
		fmt.Fprintf(w, "  top tool: %s (%d)\n", top, n)
	}
	if len(summary.ByKind) > 0 {
		top, n := session.TopCount(summary.ByKind)
		fmt.Fprintf(w, "  top kind: %s (%d)\n", top, n)
	}
	if len(summary.ByModel) > 0 {
		top, n := session.TopCount(summary.ByModel)
		fmt.Fprintf(w, "  top model: %s (%d)\n", top, n)
	}
	for _, row := range rows {
		fmt.Fprintf(w, "  [%s] [%s] [p%d t%d] [%d%%] %s: %s",
			orDash(row.Agent), orDash(row.Model), row.Prompt, row.Turn, row.ContextPct, orDash(row.Tool), row.Kind)
		if row.Excerpt != "" {
			fmt.Fprintf(w, " — %s", row.Excerpt)
		}
		fmt.Fprintln(w)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sessionDirsSince lists session directories under root, newest names first is
// unnecessary (blocks follow directory order); when all is false, only
// sessions created at or after cutoff are included.
func sessionDirsSince(root string, cutoff time.Time, all bool) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessions root %s: %w", root, err)
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !all {
			created, ok := sessionDirCreated(entry)
			if !ok || created.Before(cutoff) {
				continue
			}
		}
		dirs = append(dirs, filepath.Join(root, entry.Name()))
	}
	return dirs, nil
}

// sessionDirCreated derives a session directory's creation time from its
// timestamp name, falling back to the directory mtime for foreign names.
func sessionDirCreated(entry os.DirEntry) (time.Time, bool) {
	name := entry.Name()
	if len(name) >= len(sessionDirTimestampLayout) {
		if t, err := time.Parse(sessionDirTimestampLayout, name[:len(sessionDirTimestampLayout)]); err == nil {
			return t, true
		}
	}
	info, err := entry.Info()
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}
