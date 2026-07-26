package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type groupKey struct {
	Stateful bool
	Policy   string
}

type groupSummary struct {
	Stateful                        bool    `json:"stateful"`
	Policy                          string  `json:"policy"`
	Runs                            int     `json:"runs"`
	CorrectRuns                     int     `json:"correct_runs"`
	ExercisedRuns                   int     `json:"exercised_runs"`
	MedianUncachedInput             float64 `json:"median_uncached_input"`
	MedianUncachedInputAfterTurn10  float64 `json:"median_uncached_input_after_turn_10"`
	MedianCacheReadRatioAfterTurn10 float64 `json:"median_cache_read_ratio_after_turn_10"`
	MedianMaxRequestInputTokens     float64 `json:"median_max_request_input_tokens"`
	MedianMaxContextRatio           float64 `json:"median_max_context_ratio"`
	MedianResponseStateResets       float64 `json:"median_response_state_resets"`
	MedianRetentionEpochs           float64 `json:"median_retention_epochs"`
	MedianCompactions               float64 `json:"median_compactions"`
	MedianWallSeconds               float64 `json:"median_wall_seconds"`
}

type benchmarkSummary struct {
	Model                   string            `json:"model"`
	Runs                    int               `json:"runs"`
	Groups                  []groupSummary    `json:"groups"`
	RecommendedByMode       map[string]string `json:"recommended_by_mode,omitempty"`
	RecommendationRationale map[string]string `json:"recommendation_rationale,omitempty"`
}

// summarize aggregates per-run records. The model comes from the run config
// (a single consistent source) rather than records[0], which would be wrong
// for a future multi-model matrix.
func summarize(model string, records []runRecord) benchmarkSummary {
	summary := benchmarkSummary{
		Model:                   model,
		Runs:                    len(records),
		RecommendedByMode:       map[string]string{},
		RecommendationRationale: map[string]string{},
	}
	grouped := map[groupKey][]runRecord{}
	for _, record := range records {
		key := groupKey{Stateful: record.Stateful, Policy: record.Policy}
		grouped[key] = append(grouped[key], record)
	}
	keys := make([]groupKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Stateful != keys[j].Stateful {
			return keys[i].Stateful
		}
		return keys[i].Policy < keys[j].Policy
	})
	for _, key := range keys {
		summary.Groups = append(summary.Groups, summarizeGroup(key, grouped[key]))
	}
	for _, stateful := range []bool{true, false} {
		mode := "stateless"
		if stateful {
			mode = "stateful"
		}
		policy, rationale := recommendPolicy(summary.Groups, stateful)
		if policy != "" {
			summary.RecommendedByMode[mode] = policy
			summary.RecommendationRationale[mode] = rationale
		}
	}
	return summary
}

func summarizeGroup(key groupKey, records []runRecord) groupSummary {
	group := groupSummary{
		Stateful: key.Stateful,
		Policy:   key.Policy,
		Runs:     len(records),
	}
	var uncached, post10, cacheRatios, maxInputs, maxRatios, resets, epochs, compactions, wall []float64
	for _, record := range records {
		if record.Correct {
			group.CorrectRuns++
		}
		if record.PolicyExercised {
			group.ExercisedRuns++
		}
		uncached = append(uncached, float64(record.UncachedInputTokens))
		post10 = append(post10, float64(record.UncachedInputAfterTurn10))
		cacheDenominator := record.UncachedInputAfterTurn10 + record.CacheReadAfterTurn10 + record.CacheWriteAfterTurn10
		ratio := 0.0
		if cacheDenominator > 0 {
			ratio = float64(record.CacheReadAfterTurn10) / float64(cacheDenominator)
		}
		cacheRatios = append(cacheRatios, ratio)
		maxInputs = append(maxInputs, float64(record.MaxRequestInputTokens))
		maxRatio := 0.0
		if record.ContextWindow > 0 {
			maxRatio = float64(record.MaxRequestInputTokens) / float64(record.ContextWindow)
		}
		maxRatios = append(maxRatios, maxRatio)
		resets = append(resets, float64(record.ResponseStateResets))
		epochs = append(epochs, float64(record.RetentionEpochs))
		compactions = append(compactions, float64(record.Compactions))
		wall = append(wall, record.WallSeconds)
	}
	group.MedianUncachedInput = median(uncached)
	group.MedianUncachedInputAfterTurn10 = median(post10)
	group.MedianCacheReadRatioAfterTurn10 = median(cacheRatios)
	group.MedianMaxRequestInputTokens = median(maxInputs)
	group.MedianMaxContextRatio = median(maxRatios)
	group.MedianResponseStateResets = median(resets)
	group.MedianRetentionEpochs = median(epochs)
	group.MedianCompactions = median(compactions)
	group.MedianWallSeconds = median(wall)
	return group
}

func recommendPolicy(groups []groupSummary, stateful bool) (string, string) {
	var eligible []groupSummary
	for _, group := range groups {
		if group.Stateful != stateful ||
			group.Runs == 0 ||
			group.CorrectRuns != group.Runs ||
			group.ExercisedRuns != group.Runs {
			continue
		}
		eligible = append(eligible, group)
	}
	if len(eligible) == 0 {
		return "", ""
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return eligible[i].MedianUncachedInputAfterTurn10 < eligible[j].MedianUncachedInputAfterTurn10
	})
	best := eligible[0]
	return best.Policy, fmt.Sprintf(
		"lowest median post-turn-10 uncached input (%.0f tokens) among policies with 100%% correctness and policy exercise",
		best.MedianUncachedInputAfterTurn10,
	)
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

func writeRecords(results string, records []runRecord) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(results, "runs.json"), append(data, '\n'))
}

func writeSummary(results string, summary benchmarkSummary) error {
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(results, "summary.json"), append(data, '\n')); err != nil {
		return err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# Retention benchmark: %s\n\n", summary.Model)
	fmt.Fprintf(&text, "Runs: %d\n\n", summary.Runs)
	text.WriteString("| Mode | Policy | Correct | Exercised | Median uncached input | Post-turn-10 uncached | Post-turn-10 cache read | Max request input | Window used | Resets | Epochs | Compactions | Wall |\n")
	text.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, group := range summary.Groups {
		mode := "stateless"
		if group.Stateful {
			mode = "stateful"
		}
		fmt.Fprintf(
			&text,
			"| %s | %s | %d/%d | %d/%d | %.0f | %.0f | %.1f%% | %.0f | %.1f%% | %.1f | %.1f | %.1f | %.1fs |\n",
			mode,
			group.Policy,
			group.CorrectRuns,
			group.Runs,
			group.ExercisedRuns,
			group.Runs,
			group.MedianUncachedInput,
			group.MedianUncachedInputAfterTurn10,
			group.MedianCacheReadRatioAfterTurn10*100,
			group.MedianMaxRequestInputTokens,
			group.MedianMaxContextRatio*100,
			group.MedianResponseStateResets,
			group.MedianRetentionEpochs,
			group.MedianCompactions,
			group.MedianWallSeconds,
		)
	}
	if len(summary.RecommendedByMode) > 0 {
		text.WriteString("\nRecommendations:\n\n")
		for _, mode := range []string{"stateful", "stateless"} {
			policy, ok := summary.RecommendedByMode[mode]
			if !ok {
				continue
			}
			fmt.Fprintf(&text, "- %s: `%s` — %s.\n", mode, policy, summary.RecommendationRationale[mode])
		}
	}
	return writeFileAtomic(filepath.Join(results, "summary.md"), []byte(text.String()))
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".retentionbench-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
