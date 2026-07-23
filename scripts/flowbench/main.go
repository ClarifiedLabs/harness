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
	var (
		caseName     = flag.String("case", "", "benchmark case name")
		baselineSHA  = flag.String("baseline", "", "baseline harness git revision")
		candidateSHA = flag.String("candidate", "", "candidate harness git revision")
		repo         = flag.String("repo", ".", "harness repository")
		results      = flag.String("results", "", "result directory outside the repository")
		models       = flag.String("models", strings.Join(defaultModels, ","), "comma-separated model target ids")
		repetitions  = flag.Int("repetitions", 3, "baseline/candidate pairs per model")
		dryRun       = flag.Bool("dry-run", false, "print the resolved run matrix without building or calling models")
		resume       = flag.Bool("resume", false, "reuse valid completed records and rerun interrupted unrecorded runs")
		importRuns   = flag.String("import-baseline-runs", "", "reuse validated baseline records from another case runs JSON file")
	)
	flag.Parse()
	cases := allCases()
	if *caseName == "" || *baselineSHA == "" || *candidateSHA == "" {
		flag.Usage()
		os.Exit(2)
	}
	c, ok := cases[*caseName]
	if !ok {
		fmt.Fprintf(os.Stderr, "flowbench: unknown case %q\n", *caseName)
		os.Exit(2)
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
	records, err := executeMatrix(context.Background(), runConfig{
		Repo:         absRepo,
		Results:      resultDir,
		Case:         c,
		BaselineSHA:  *baselineSHA,
		CandidateSHA: *candidateSHA,
		Models:       selectedModels,
		Repetitions:  *repetitions,
		DryRun:       *dryRun,
		Resume:       *resume,
		ImportRuns:   *importRuns,
	})
	if *dryRun {
		for _, record := range records {
			fmt.Printf("%02d\t%s\t%d\t%s\t%s\n", record.Order, record.Model, record.Repetition, record.Variant, record.HarnessSHA)
		}
	}
	fmt.Fprintf(os.Stderr, "flowbench: results %s\n", resultDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "flowbench: %v\n", err)
		os.Exit(1)
	}
}
