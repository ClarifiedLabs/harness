package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case defaultStackVerifierCommand:
		os.Exit(runDefaultStackVerifier(".", os.Stdin, os.Stdout))
	case stagnationEvaluatorCommand:
		os.Exit(runStagnationEvaluator(".", os.Args[1:], os.Stdin, os.Stdout))
	case stagnationRecoveryCommand:
		os.Exit(runStagnationRecoveryEvaluator(".", os.Stdin, os.Stdout))
	case lineageEvaluatorCommand:
		os.Exit(runLineageEvaluator(".", os.Stdin, os.Stdout))
	}

	var (
		caseName     = flag.String("case", "", "benchmark case name")
		suiteName    = flag.String("suite", "", "benchmark suite name (tool_accuracy)")
		profile      = flag.String("profile", "promotion", "suite profile: smoke or promotion")
		baselineSHA  = flag.String("baseline", "", "baseline harness git revision")
		candidateSHA = flag.String("candidate", "", "candidate harness git revision")
		repo         = flag.String("repo", ".", "harness repository")
		results      = flag.String("results", "", "result directory outside the repository")
		models       = flag.String("models", strings.Join(defaultModels, ","), "comma-separated model target ids")
		repetitions  = flag.Int("repetitions", 5, "baseline/candidate pairs per model")
		reasoning    = flag.String("reasoning", "medium", "reasoning profile for every run")
		parallel     = flag.Bool("parallel-models", false, "run one instance of each model concurrently per AB/BA round")
		dryRun       = flag.Bool("dry-run", false, "print the resolved run matrix without building or calling models")
		resume       = flag.Bool("resume", false, "reuse valid completed records and rerun interrupted unrecorded runs")
		importRuns   = flag.String("import-baseline-runs", "", "reuse validated baseline records from another case runs JSON file")
	)
	flag.Parse()
	cases := allCases()
	if (*caseName == "") == (*suiteName == "") || *baselineSHA == "" || *candidateSHA == "" {
		flag.Usage()
		os.Exit(2)
	}
	selectedCases := []benchmarkCase{}
	if *caseName != "" {
		c, ok := cases[*caseName]
		if !ok {
			fmt.Fprintf(os.Stderr, "flowbench: unknown case %q\n", *caseName)
			os.Exit(2)
		}
		selectedCases = append(selectedCases, c)
	} else {
		if *suiteName != "tool_accuracy" || (*profile != "smoke" && *profile != "promotion") {
			fmt.Fprintf(os.Stderr, "flowbench: unsupported suite/profile %q/%q\n", *suiteName, *profile)
			os.Exit(2)
		}
		for _, name := range []string{"edit_precision", "edit_drift_recovery", "known_path_batching", "unknown_path_discovery"} {
			selectedCases = append(selectedCases, cases[name])
		}
	}
	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "flowbench: repo: %v\n", err)
		os.Exit(1)
	}
	resultDir := *results
	if resultDir == "" {
		resultDir, err = os.MkdirTemp("", "harness-flowbench-results-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "flowbench: results: %v\n", err)
			os.Exit(1)
		}
	} else if resultDir, err = filepath.Abs(resultDir); err != nil {
		fmt.Fprintf(os.Stderr, "flowbench: results: %v\n", err)
		os.Exit(1)
	}
	var selectedModels []string
	for _, model := range strings.Split(*models, ",") {
		if model = strings.TrimSpace(model); model != "" {
			selectedModels = append(selectedModels, model)
		}
	}
	explicitModels, explicitRepetitions := false, false
	flag.Visit(func(f *flag.Flag) {
		explicitModels = explicitModels || f.Name == "models"
		explicitRepetitions = explicitRepetitions || f.Name == "repetitions"
	})
	if *suiteName != "" && *profile == "smoke" {
		if !explicitModels {
			selectedModels = []string{"alibaba-token-plan:qwen3.8-max"}
		}
		if !explicitRepetitions {
			*repetitions = 1
		}
	}
	for _, c := range selectedCases {
		records, runErr := executeMatrix(context.Background(), runConfig{
			Repo: absRepo, Results: resultDir, Case: c,
			BaselineSHA: *baselineSHA, CandidateSHA: *candidateSHA,
			Models: selectedModels, Repetitions: *repetitions,
			Reasoning:      *reasoning,
			ParallelModels: *parallel,
			DryRun:         *dryRun, Resume: *resume, ImportRuns: *importRuns, Profile: *profile,
		})
		if *dryRun {
			for _, record := range records {
				fmt.Printf("%s\t%02d\t%s\t%d\t%s\t%s\t%s\n", c.Name, record.Order, record.Model, record.Repetition, record.Variant, record.HarnessSHA, record.Agent)
			}
		}
		if runErr != nil {
			fmt.Fprintf(os.Stderr, "flowbench: results %s\n", resultDir)
			fmt.Fprintf(os.Stderr, "flowbench: %v\n", runErr)
			os.Exit(1)
		}
	}
	fmt.Fprintf(os.Stderr, "flowbench: results %s\n", resultDir)
}
