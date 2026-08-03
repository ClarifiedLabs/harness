package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"harness/internal/cli"
	"harness/internal/session"
	"harness/internal/ui"
)

// runSessionAnalyze implements recursive, transcript-free analysis for one
// session root, a directory containing roots, or the default session history.
func runSessionAnalyze(env environment, invocation cli.Invocation) int {
	values := invocation.Flags
	since := cliLast(values, "since", "24h")
	all := cliBool(values, "all")
	format := cliLast(values, "format", "text")
	beforeValue := cliLast(values, "before", "")
	if format != "text" && format != "json" {
		fmt.Fprintf(env.stderr, "session analyze: unsupported --format %q (want text or json)\n", format)
		return ui.ExitUsage
	}
	var before time.Time
	if strings.TrimSpace(beforeValue) != "" {
		var err error
		before, err = time.Parse(time.RFC3339, beforeValue)
		if err != nil {
			fmt.Fprintf(env.stderr, "session analyze: invalid --before %q: want RFC3339\n", beforeValue)
			return ui.ExitUsage
		}
	}
	opts := session.AnalyzeOptions{Before: before}

	var (
		report session.AnalysisReport
		err    error
	)
	if len(invocation.Args) == 1 {
		report, err = session.AnalyzeCorpus(invocation.Args[0], opts)
	} else {
		window, parseErr := time.ParseDuration(since)
		if parseErr != nil || window < 0 {
			fmt.Fprintf(env.stderr, "session analyze: invalid --since %q: must be a duration like 24h or 720h\n", since)
			return ui.ExitUsage
		}
		now := time.Now
		if env.now != nil {
			now = env.now
		}
		root := filepath.Join(stateDir(env.getenv), "harness", "sessions")
		dirs, discoverErr := sessionDirsSince(root, now().Add(-window), all)
		if discoverErr != nil {
			err = discoverErr
		} else {
			report, err = session.AnalyzeSessionDirs(root, dirs, opts)
		}
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "session analyze: %v\n", err)
		return ui.ExitRuntime
	}
	if format == "json" {
		err = session.WriteAnalysisJSON(report, env.stdout)
	} else {
		err = session.WriteAnalysisText(report, env.stdout)
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "session analyze: %v\n", err)
		return ui.ExitRuntime
	}
	return ui.ExitOK
}
