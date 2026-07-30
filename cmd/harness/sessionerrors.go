package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"harness/internal/session"
	"harness/internal/ui"
)

// sessionDirTimestampLayout matches the directory names DefaultPath creates
// under the sessions root.
const sessionDirTimestampLayout = "20060102T150405Z"

// runSessionErrors implements `harness session errors`: classified tool and
// model failures for one session directory, or scanned across recent sessions
// under the default sessions root when no directory argument is given.
func runSessionErrors(env environment, args []string) int {
	flags := flag.NewFlagSet("session errors", flag.ContinueOnError)
	flags.SetOutput(env.stderr)
	tool := flags.String("tool", "", "only failures from this tool")
	kind := flags.String("kind", "", "only failures of this error kind")
	model := flags.String("model", "", "only failures attributed to this model")
	agent := flags.String("agent", "", "only failures attributed to this agent")
	since := flags.String("since", "24h", "when scanning, include sessions created within this duration (e.g. 24h, 720h)")
	all := flags.Bool("all", false, "scan all sessions, ignoring --since")
	format := flags.String("format", "text", "output format: text or json")
	if err := flags.Parse(args); err != nil {
		return ui.ExitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(env.stderr, "session errors: unsupported --format %q (want text or json)\n", *format)
		return ui.ExitUsage
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(env.stderr, "usage: harness session errors [--tool T] [--kind K] [--model M] [--agent A] [--since D|--all] [--format text|json] [dir]")
		return ui.ExitUsage
	}
	filter := session.ErrorFilter{Tool: *tool, Kind: *kind, Model: *model, Agent: *agent}

	if flags.NArg() == 1 {
		dir := flags.Arg(0)
		rows, err := session.CollectErrors(dir, filter)
		if err != nil {
			fmt.Fprintf(env.stderr, "session errors: %v\n", err)
			return ui.ExitRuntime
		}
		report := sessionErrorsReport{
			Scope:           map[string]any{"dir": dir},
			SessionsScanned: 1,
			Rows:            rows,
		}
		return writeSessionErrors(env.stdout, *format, report, []sessionErrorBlock{{dir: dir, rows: rows}}, 0)
	}

	window, err := time.ParseDuration(*since)
	if err != nil || window < 0 {
		fmt.Fprintf(env.stderr, "session errors: invalid --since %q: must be a duration like 24h or 720h\n", *since)
		return ui.ExitUsage
	}
	now := time.Now
	if env.now != nil {
		now = env.now
	}
	root := filepath.Join(stateDir(env.getenv), "harness", "sessions")
	dirs, err := sessionDirsSince(root, now().Add(-window), *all)
	if err != nil {
		fmt.Fprintf(env.stderr, "session errors: %v\n", err)
		return ui.ExitRuntime
	}
	var blocks []sessionErrorBlock
	var rows []session.ErrorRow
	for _, dir := range dirs {
		sessionRows, err := session.CollectErrors(dir, filter)
		if err != nil {
			fmt.Fprintf(env.stderr, "session errors: %v\n", err)
			return ui.ExitRuntime
		}
		if len(sessionRows) == 0 {
			continue
		}
		blocks = append(blocks, sessionErrorBlock{dir: dir, rows: sessionRows})
		rows = append(rows, sessionRows...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].At.Equal(rows[j].At) {
			return rows[i].At.Before(rows[j].At)
		}
		return rows[i].Session < rows[j].Session
	})
	report := sessionErrorsReport{
		Scope:           map[string]any{"sessions_root": root, "since": *since, "all": *all},
		SessionsScanned: len(dirs),
		Rows:            rows,
	}
	return writeSessionErrors(env.stdout, *format, report, blocks, len(dirs))
}

// sessionErrorBlock groups one session's rows for the text renderer.
type sessionErrorBlock struct {
	dir  string
	rows []session.ErrorRow
}

// sessionErrorsReport is the JSON output shape.
type sessionErrorsReport struct {
	Scope           map[string]any       `json:"scope"`
	SessionsScanned int                  `json:"sessions_scanned"`
	Summary         session.ErrorSummary `json:"summary"`
	Rows            []session.ErrorRow   `json:"rows"`
}

// writeSessionErrors renders the report as JSON, or as per-session text
// blocks followed by a scan footer when scanning (scanned > 0).
func writeSessionErrors(w io.Writer, format string, report sessionErrorsReport, blocks []sessionErrorBlock, scanned int) int {
	rows := report.Rows
	if rows == nil {
		rows = []session.ErrorRow{}
	}
	report.Rows = rows
	report.Summary = session.SummarizeErrors(rows)
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
		writeSessionErrorBlock(w, block.dir, block.rows)
	}
	if scanned > 0 {
		summary := report.Summary
		fmt.Fprintf(w, "Scanned %d sessions: %d failed tool results, %d model request failures\n",
			scanned, summary.FailedToolResults, summary.ModelRequestFailures)
	}
	return ui.ExitOK
}

func writeSessionErrorBlock(w io.Writer, dir string, rows []session.ErrorRow) {
	summary := session.SummarizeErrors(rows)
	fmt.Fprintf(w, "Session errors: %s\n", dir)
	fmt.Fprintf(w, "  failed tool results: %d   model request failures: %d\n",
		summary.FailedToolResults, summary.ModelRequestFailures)
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
