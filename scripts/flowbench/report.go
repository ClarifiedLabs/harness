package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type aggregate struct {
	Case                   string              `json:"case"`
	Runs                   int                 `json:"runs"`
	BaselinePasses         int                 `json:"baseline_passes"`
	CandidatePasses        int                 `json:"candidate_passes"`
	Adoptions              int                 `json:"adoptions"`
	AdoptionsByModel       map[string]int      `json:"adoptions_by_model"`
	BaselineMedianTokens   float64             `json:"baseline_median_tokens"`
	CandidateMedianTokens  float64             `json:"candidate_median_tokens"`
	TokenSavingPct         float64             `json:"token_saving_pct"`
	PrimaryBaselineMedian  float64             `json:"primary_baseline_median"`
	PrimaryCandidateMedian float64             `json:"primary_candidate_median"`
	PrimaryReductionPct    float64             `json:"primary_reduction_pct"`
	DeepSeekCostUSD        float64             `json:"deepseek_cost_usd"`
	SubscriptionCosts      string              `json:"subscription_costs"`
	Accepted               bool                `json:"accepted"`
	Failures               []string            `json:"failures,omitempty"`
	Models                 map[string]modelAgg `json:"models"`
}

type modelAgg struct {
	BaselinePasses        int     `json:"baseline_passes"`
	CandidatePasses       int     `json:"candidate_passes"`
	Adoptions             int     `json:"adoptions"`
	BaselineMedianTokens  float64 `json:"baseline_median_tokens"`
	CandidateMedianTokens float64 `json:"candidate_median_tokens"`
	TokenSavingPct        float64 `json:"token_saving_pct"`
	CostUSD               float64 `json:"cost_usd"`
	CostBasis             string  `json:"cost_basis"`
}

func writeRecords(results string, records []runRecord) error {
	ordered := append([]runRecord(nil), records...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })
	data, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	caseName := "matrix"
	if len(records) > 0 && records[0].Case != "" {
		caseName = records[0].Case
	}
	return os.WriteFile(filepath.Join(results, caseName+"-runs.json"), data, 0o644)
}

func writeSummary(results string, c benchmarkCase, records []runRecord) error {
	agg := summarize(c, records)
	data, err := json.MarshalIndent(agg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(results, c.Name+"-summary.json"), data, 0o644); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Flow benchmark: %s\n\n", c.Name)
	fmt.Fprintf(&b, "- Runs: %d\n", agg.Runs)
	fmt.Fprintf(&b, "- Correctness: baseline %d, candidate %d\n", agg.BaselinePasses, agg.CandidatePasses)
	fmt.Fprintf(&b, "- Adoption: %d candidate runs\n", agg.Adoptions)
	fmt.Fprintf(&b, "- Unpaired median tokens: baseline %.0f, candidate %.0f\n", agg.BaselineMedianTokens, agg.CandidateMedianTokens)
	fmt.Fprintf(&b, "- Paired-median token saving: %.1f%%\n", agg.TokenSavingPct)
	fmt.Fprintf(&b, "- Median %s: baseline %.1f, candidate %.1f (%.1f%% reduction)\n", c.PrimaryMetric, agg.PrimaryBaselineMedian, agg.PrimaryCandidateMedian, agg.PrimaryReductionPct)
	fmt.Fprintf(&b, "- DeepSeek reported cost: $%.6f\n", agg.DeepSeekCostUSD)
	fmt.Fprintf(&b, "- Subscription provider cost: %s\n", agg.SubscriptionCosts)
	fmt.Fprintf(&b, "- Accepted: %t\n", agg.Accepted)
	if len(agg.Failures) > 0 {
		b.WriteString("\nFailures:\n\n")
		for _, failure := range agg.Failures {
			fmt.Fprintf(&b, "- %s\n", failure)
		}
	}
	return os.WriteFile(filepath.Join(results, c.Name+"-summary.md"), []byte(b.String()), 0o644)
}

func summarize(c benchmarkCase, records []runRecord) aggregate {
	agg := aggregate{
		Case:              c.Name,
		Runs:              len(records),
		AdoptionsByModel:  map[string]int{},
		Models:            map[string]modelAgg{},
		SubscriptionCosts: "N/A (alibaba-token-plan and openai-codex)",
	}
	var baselineTokens, candidateTokens, baselinePrimary, candidatePrimary []float64
	byModel := map[string][]runRecord{}
	for _, record := range records {
		byModel[record.Model] = append(byModel[record.Model], record)
		primary := float64(primaryValue(c, record.Metrics))
		if strings.HasPrefix(record.Model, "deepseek:") {
			agg.DeepSeekCostUSD += record.Metrics.CostUSD
		}
		if record.Variant == "baseline" {
			if record.Score.Pass {
				agg.BaselinePasses++
			}
			baselineTokens = append(baselineTokens, float64(record.Metrics.TotalTokens))
			baselinePrimary = append(baselinePrimary, primary)
		} else {
			if record.Score.Pass {
				agg.CandidatePasses++
			}
			candidateTokens = append(candidateTokens, float64(record.Metrics.TotalTokens))
			candidatePrimary = append(candidatePrimary, primary)
			if adopted(c.Name, record.Metrics) {
				agg.Adoptions++
				agg.AdoptionsByModel[record.Model]++
			}
		}
	}
	agg.BaselineMedianTokens = median(baselineTokens)
	agg.CandidateMedianTokens = median(candidateTokens)
	agg.TokenSavingPct = medianPairedReduction(records, func(record runRecord) float64 {
		return float64(record.Metrics.TotalTokens)
	})
	agg.PrimaryBaselineMedian = median(baselinePrimary)
	agg.PrimaryCandidateMedian = median(candidatePrimary)
	agg.PrimaryReductionPct = reductionPct(agg.PrimaryBaselineMedian, agg.PrimaryCandidateMedian)

	modelNames := make([]string, 0, len(byModel))
	for model := range byModel {
		modelNames = append(modelNames, model)
	}
	sort.Strings(modelNames)
	for _, model := range modelNames {
		var bt, ct []float64
		ma := modelAgg{CostBasis: "reported"}
		var modelRecords []runRecord
		for _, record := range byModel[model] {
			modelRecords = append(modelRecords, record)
			if record.Metrics.CostKnown || strings.HasPrefix(model, "deepseek:") {
				ma.CostUSD += record.Metrics.CostUSD
			}
			if record.Variant == "baseline" {
				bt = append(bt, float64(record.Metrics.TotalTokens))
				if record.Score.Pass {
					ma.BaselinePasses++
				}
			} else {
				ct = append(ct, float64(record.Metrics.TotalTokens))
				if record.Score.Pass {
					ma.CandidatePasses++
				}
				if adopted(c.Name, record.Metrics) {
					ma.Adoptions++
				}
			}
		}
		ma.BaselineMedianTokens = median(bt)
		ma.CandidateMedianTokens = median(ct)
		ma.TokenSavingPct = medianPairedReduction(modelRecords, func(record runRecord) float64 {
			return float64(record.Metrics.TotalTokens)
		})
		if strings.HasPrefix(model, "alibaba-token-plan:") || strings.HasPrefix(model, "openai-codex:") {
			ma.CostUSD = 0
			ma.CostBasis = "subscription (N/A)"
		}
		agg.Models[model] = ma
	}

	candidateRuns := len(candidateTokens)
	requiredPasses := minimumCorrectnessPasses(candidateRuns)
	if agg.CandidatePasses < requiredPasses {
		agg.Failures = append(agg.Failures, fmt.Sprintf(
			"candidate correctness %d/%d is below the 8/9 gate (%d required)",
			agg.CandidatePasses, candidateRuns, requiredPasses,
		))
	}
	if agg.CandidatePasses < agg.BaselinePasses {
		agg.Failures = append(agg.Failures, "candidate correctness is below baseline")
	}
	for _, model := range modelNames {
		ma := agg.Models[model]
		if ma.Adoptions < 2 {
			agg.Failures = append(agg.Failures, fmt.Sprintf("%s adoption %d/3 is below 2/3", model, ma.Adoptions))
		}
		if ma.TokenSavingPct < -10 {
			agg.Failures = append(agg.Failures, fmt.Sprintf("%s median tokens regressed %.1f%%", model, -ma.TokenSavingPct))
		}
	}
	if agg.PrimaryReductionPct < c.MinimumReductionPct {
		agg.Failures = append(agg.Failures, fmt.Sprintf("primary metric reduction %.1f%% is below %.1f%%", agg.PrimaryReductionPct, c.MinimumReductionPct))
	}
	if agg.TokenSavingPct <= 0 {
		agg.Failures = append(agg.Failures, "aggregate paired-median tokens did not decrease")
	}
	agg.Accepted = len(agg.Failures) == 0
	return agg
}

func minimumCorrectnessPasses(runs int) int {
	if runs <= 0 {
		return 0
	}
	return (8*runs + 8) / 9
}

func primaryValue(c benchmarkCase, m metrics) int {
	switch c.PrimaryMetric {
	case "rg_to_read_transitions":
		return m.RGToReadTransitions
	case "command_to_command_transitions":
		return m.CommandToCommandTransitions
	case "avoidable_todo_only_turns":
		return m.AvoidableTodoOnlyTurns
	case "git_calls":
		return m.GitCalls
	case "background_polls":
		return m.BackgroundPolls
	default:
		return 0
	}
}

func adopted(name string, m metrics) bool {
	switch name {
	case "search_context":
		return m.UsedSearchContext
	case "command_steps":
		return m.UsedCommandSteps
	case "todo_coissue":
		return m.ToolCalls["update_todos"] > 0 && m.AvoidableTodoOnlyTurns == 0
	case "git_workspace_summary":
		return m.UsedWorkspaceSummary
	case "background_wait":
		return m.BackgroundWaits > 0 && m.BackgroundPolls == 0
	default:
		return false
	}
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func reductionPct(before, after float64) float64 {
	if before == 0 {
		if after == 0 {
			return 0
		}
		return -100
	}
	return (before - after) * 100 / before
}

func medianPairedReduction(records []runRecord, value func(runRecord) float64) float64 {
	type pair struct {
		baseline  *runRecord
		candidate *runRecord
	}
	pairs := make(map[string]pair)
	for i := range records {
		record := &records[i]
		key := record.Model + "\x00" + fmt.Sprint(record.Repetition)
		p := pairs[key]
		switch record.Variant {
		case "baseline":
			p.baseline = record
		case "candidate":
			p.candidate = record
		}
		pairs[key] = p
	}
	var reductions []float64
	for _, p := range pairs {
		if p.baseline == nil || p.candidate == nil {
			continue
		}
		reductions = append(reductions, reductionPct(value(*p.baseline), value(*p.candidate)))
	}
	return median(reductions)
}
