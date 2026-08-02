package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"harness/internal/session"
	"harness/internal/ui"
)

const sessionAnalyzeUsage = "usage: harness session analyze [--since D|--all] [--before RFC3339] [--format text|json] [dir]"

// runSessionAnalyze implements recursive, transcript-free analysis for one
// session root, a directory containing roots, or the default session history.
func runSessionAnalyze(env environment, args []string) int {
	flags := flag.NewFlagSet("session analyze", flag.ContinueOnError)
	flags.SetOutput(env.stderr)
	since := flags.String("since", "24h", "when scanning, include sessions created within this duration (e.g. 24h, 720h)")
	all := flags.Bool("all", false, "scan all sessions, ignoring --since")
	format := flags.String("format", "text", "output format: text or json")
	beforeValue := flags.String("before", "", "include only events at or before this RFC3339 timestamp")
	if err := flags.Parse(args); err != nil {
		return ui.ExitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(env.stderr, "session analyze: unsupported --format %q (want text or json)\n", *format)
		return ui.ExitUsage
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(env.stderr, sessionAnalyzeUsage)
		return ui.ExitUsage
	}
	var before time.Time
	if strings.TrimSpace(*beforeValue) != "" {
		var err error
		before, err = time.Parse(time.RFC3339, *beforeValue)
		if err != nil {
			fmt.Fprintf(env.stderr, "session analyze: invalid --before %q: want RFC3339\n", *beforeValue)
			return ui.ExitUsage
		}
	}
	opts := session.AnalyzeOptions{Before: before}

	var (
		report session.AnalysisReport
		err    error
	)
	if flags.NArg() == 1 {
		report, err = session.AnalyzeCorpus(flags.Arg(0), opts)
	} else {
		window, parseErr := time.ParseDuration(*since)
		if parseErr != nil || window < 0 {
			fmt.Fprintf(env.stderr, "session analyze: invalid --since %q: must be a duration like 24h or 720h\n", *since)
			return ui.ExitUsage
		}
		now := time.Now
		if env.now != nil {
			now = env.now
		}
		root := filepath.Join(stateDir(env.getenv), "harness", "sessions")
		dirs, discoverErr := sessionDirsSince(root, now().Add(-window), *all)
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
	if *format == "json" {
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
