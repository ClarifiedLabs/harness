package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		harnessPath   = flag.String("harness", "harness", "harness binary to benchmark")
		model         = flag.String("model", "", "model proxy target id")
		results       = flag.String("results", "", "result directory; defaults to a temporary directory")
		policiesFlag  = flag.String("policies", "age,disabled,pressure", "comma-separated retention policies")
		statefulFlag  = flag.String("stateful", "both", "Responses continuation fixture: true, false, or both")
		repetitions   = flag.Int("repetitions", 3, "runs per policy and stateful mode")
		contextWindow = flag.Int(
			"context-window",
			30_000,
			"context window override used to make pressure epochs observable",
		)
		probeCount = flag.Int("probe-count", 12, "ordered read calls required per run")
		probeBytes = flag.Int("probe-bytes", 6_000, "approximate bytes in each probe file")
		timeout    = flag.Duration("timeout", 15*time.Minute, "timeout per harness run")
		dryRun     = flag.Bool("dry-run", false, "print the run matrix without calling harness")
	)
	flag.Parse()

	if strings.TrimSpace(*model) == "" {
		fmt.Fprintln(os.Stderr, "retentionbench: -model is required")
		os.Exit(2)
	}
	policies, err := parsePolicies(*policiesFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench: %v\n", err)
		os.Exit(2)
	}
	statefulModes, err := parseStatefulModes(*statefulFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench: %v\n", err)
		os.Exit(2)
	}
	cfg := runConfig{
		Harness:       *harnessPath,
		Model:         *model,
		Results:       *results,
		Policies:      policies,
		StatefulModes: statefulModes,
		Repetitions:   *repetitions,
		ContextWindow: *contextWindow,
		ProbeCount:    *probeCount,
		ProbeBytes:    *probeBytes,
		Timeout:       *timeout,
		DryRun:        *dryRun,
	}
	records, resultDir, err := executeMatrix(context.Background(), cfg)
	if *dryRun {
		for _, record := range records {
			fmt.Printf(
				"%02d\t%s\tstateful=%t\t%s\trepetition=%d\n",
				record.Order,
				record.Model,
				record.Stateful,
				record.Policy,
				record.Repetition,
			)
		}
	}
	if resultDir != "" {
		fmt.Fprintf(os.Stderr, "retentionbench: results %s\n", resultDir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench: %v\n", err)
		os.Exit(1)
	}
}
