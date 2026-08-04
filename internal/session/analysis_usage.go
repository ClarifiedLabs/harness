package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"

	"harness/internal/llm"
)

// UsageSlice is one non-overlapping usage bucket. Available distinguishes a
// measured zero from a stream that did not carry usable raw telemetry.
// Complete describes source-stream completeness; CostComplete describes price
// coverage and may be false while KnownCostUSD still reports a useful partial
// sum.
type UsageSlice struct {
	Available          bool    `json:"available"`
	Complete           bool    `json:"complete"`
	InputTokens        int     `json:"input_tokens"`
	CacheReadTokens    int     `json:"cache_read_tokens"`
	CacheWriteTokens   int     `json:"cache_write_tokens"`
	CacheWrite1hTokens int     `json:"cache_write_1h_tokens"`
	OutputTokens       int     `json:"output_tokens"`
	ReasoningTokens    int     `json:"reasoning_tokens"`
	TotalTokens        int     `json:"total_tokens"`
	KnownCostUSD       float64 `json:"known_cost_usd"`
	ModelCalls         int     `json:"model_calls"`
	PricedCalls        int     `json:"priced_calls"`
	UnpricedCalls      int     `json:"unpriced_calls"`
	CostComplete       bool    `json:"cost_complete"`
	Sources            int     `json:"sources"`
	CompleteSources    int     `json:"complete_sources"`
}

// UsageReconciliation compares complete raw hierarchy usage with the root's
// authoritative persisted aggregate. Prefix analyses deliberately skip it.
type UsageReconciliation struct {
	Available       bool       `json:"available"`
	Matches         bool       `json:"matches"`
	ComparedRoots   int        `json:"compared_roots"`
	Discrepancies   int        `json:"discrepancies"`
	DifferingFields []string   `json:"differing_fields,omitempty"`
	Persisted       UsageSlice `json:"persisted"`
}

// UsageAnalysis splits direct physical-stream usage without subtracting
// already-folded parent aggregates. Inclusive is the exact sum of the four
// disjoint buckets.
type UsageAnalysis struct {
	RootConversational       UsageSlice          `json:"root_conversational"`
	RootMaintenance          UsageSlice          `json:"root_maintenance"`
	DescendantConversational UsageSlice          `json:"descendant_conversational"`
	DescendantMaintenance    UsageSlice          `json:"descendant_maintenance"`
	Inclusive                UsageSlice          `json:"inclusive"`
	Reconciliation           UsageReconciliation `json:"reconciliation"`
}

// IntDistribution and FloatDistribution use nearest-rank percentiles and keep
// sample counts beside every result. A zero sample count means unavailable,
// not an observed zero.
type IntDistribution struct {
	Samples int `json:"samples"`
	Median  int `json:"median"`
	P90     int `json:"p90"`
}

type FloatDistribution struct {
	Samples int     `json:"samples"`
	Median  float64 `json:"median"`
	P90     float64 `json:"p90"`
}

// UsageDistributions treats each root hierarchy as one sample.
type UsageDistributions struct {
	InclusiveTokens        IntDistribution   `json:"inclusive_tokens"`
	RootTokens             IntDistribution   `json:"root_tokens"`
	DescendantTokens       IntDistribution   `json:"descendant_tokens"`
	InclusiveKnownCostUSD  FloatDistribution `json:"inclusive_known_complete_cost_usd"`
	RootKnownCostUSD       FloatDistribution `json:"root_known_complete_cost_usd"`
	DescendantKnownCostUSD FloatDistribution `json:"descendant_known_complete_cost_usd"`
}

// HierarchyAnalysis is one root-owned experimental sample. It contains only
// transcript-free aggregate telemetry.
type HierarchyAnalysis struct {
	RootPath  string            `json:"root_path"`
	RootID    string            `json:"root_id,omitempty"`
	Cohort    CohortIdentity    `json:"cohort"`
	Sessions  int               `json:"sessions"`
	Execution ExecutionAnalysis `json:"execution"`
	Workflow  WorkflowAnalysis  `json:"workflow"`
	Usage     UsageAnalysis     `json:"usage"`
	Storage   StorageAnalysis   `json:"storage"`
}

// CohortIdentity is copied into every cohort so opaque keys remain auditable.
type CohortIdentity struct {
	Available bool           `json:"available"`
	Key       string         `json:"key"`
	Build     BuildMetadata  `json:"build"`
	Runtime   RuntimeProfile `json:"runtime"`
}

// CohortAnalysis aggregates root-hierarchy samples sharing a build/runtime
// identity. Child streams inherit this cohort while retaining their own item
// metadata.
type CohortAnalysis struct {
	Cohort        CohortIdentity     `json:"cohort"`
	Roots         int                `json:"roots"`
	Sessions      int                `json:"sessions"`
	Execution     ExecutionAnalysis  `json:"execution"`
	Workflow      WorkflowAnalysis   `json:"workflow"`
	Usage         UsageAnalysis      `json:"usage"`
	Distributions UsageDistributions `json:"distributions"`
	Storage       StorageAnalysis    `json:"storage"`
}

func usageFromEvents(events []Event, sourceStatus string) (UsageSlice, UsageSlice) {
	available := sourceStatus != "missing" && sourceStatus != "symlink"
	complete := sourceStatus == "complete"
	conversation := newUsageSlice(available, complete)
	maintenance := newUsageSlice(available, complete)
	if !available {
		return conversation, maintenance
	}
	started := make(map[[3]int]struct{})
	accounted := make(map[[3]int]struct{})
	accountedTurns := make(map[[2]int]struct{})
	startedPrompts := make(map[int]struct{})
	accountedPrompts := make(map[int]struct{})
	duplicateAttemptUsage := false
	for _, event := range events {
		key := [3]int{event.Prompt, event.Turn, event.Attempt}
		switch event.Type {
		case EventTurnAttemptStart:
			started[key] = struct{}{}
			startedPrompts[event.Prompt] = struct{}{}
		case EventTurnAttemptUsage:
			if _, duplicate := accounted[key]; duplicate {
				duplicateAttemptUsage = true
				continue
			}
			conversation.addCall(event.Usage)
			accounted[key] = struct{}{}
			accountedTurns[[2]int{event.Prompt, event.Turn}] = struct{}{}
			accountedPrompts[event.Prompt] = struct{}{}
		case EventMaintenanceUsage:
			maintenance.addCall(event.Usage)
		}
	}
	for key := range started {
		if _, ok := accounted[key]; !ok {
			conversation.addCall(nil)
		}
	}
	// Legacy streams carried only folded turn/prompt totals. Those totals cannot
	// be split safely across retries and maintenance, so keep the physical-event
	// accounting boundary explicit rather than reporting an observed zero.
	legacyFoldedUsage := false
	for _, event := range events {
		if event.Usage == nil {
			continue
		}
		if event.Type == EventTurnComplete {
			if _, ok := accountedTurns[[2]int{event.Prompt, event.Turn}]; !ok {
				legacyFoldedUsage = true
			}
		}
		if event.Type == EventPromptUsage {
			_, promptAccounted := accountedPrompts[event.Prompt]
			_, promptStarted := startedPrompts[event.Prompt]
			if !promptAccounted && !promptStarted {
				legacyFoldedUsage = true
			}
		}
	}
	if legacyFoldedUsage {
		conversation.Available = false
		conversation.CompleteSources = 0
		maintenance.Available = false
		maintenance.CompleteSources = 0
	}
	if duplicateAttemptUsage {
		conversation.CompleteSources = 0
	}
	conversation.finish()
	maintenance.finish()
	return conversation, maintenance
}

func newUsageSlice(available, complete bool) UsageSlice {
	u := UsageSlice{Available: available, Complete: available && complete, Sources: 1}
	if available && complete {
		u.CompleteSources = 1
	}
	return u
}

func (u *UsageSlice) addCall(usage *llm.Usage) {
	u.ModelCalls++
	if usage == nil {
		u.UnpricedCalls++
		return
	}
	u.addTokens(*usage)
	if usage.CostKnown {
		u.PricedCalls++
		u.KnownCostUSD += usage.CostUSD
	} else {
		u.UnpricedCalls++
	}
}

func (u *UsageSlice) addTokens(usage llm.Usage) {
	u.InputTokens += max(0, usage.InputTokens)
	u.CacheReadTokens += max(0, usage.CacheReadTokens)
	u.CacheWriteTokens += max(0, usage.CacheWriteTokens)
	u.CacheWrite1hTokens += max(0, usage.CacheWrite1hTokens)
	u.OutputTokens += max(0, usage.OutputTokens)
	u.ReasoningTokens += max(0, usage.ReasoningTokens)
}

func (u *UsageSlice) finish() {
	u.TotalTokens = u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.CacheWrite1hTokens + u.OutputTokens + u.ReasoningTokens
	u.Complete = u.Available && u.Sources == u.CompleteSources
	u.CostComplete = u.Available && u.UnpricedCalls == 0
}

func addUsageSlice(a, b UsageSlice) UsageSlice {
	out := UsageSlice{
		Available:          a.Available || b.Available,
		InputTokens:        a.InputTokens + b.InputTokens,
		CacheReadTokens:    a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:   a.CacheWriteTokens + b.CacheWriteTokens,
		CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens,
		OutputTokens:       a.OutputTokens + b.OutputTokens,
		ReasoningTokens:    a.ReasoningTokens + b.ReasoningTokens,
		KnownCostUSD:       a.KnownCostUSD + b.KnownCostUSD,
		ModelCalls:         a.ModelCalls + b.ModelCalls,
		PricedCalls:        a.PricedCalls + b.PricedCalls,
		UnpricedCalls:      a.UnpricedCalls + b.UnpricedCalls,
		Sources:            a.Sources + b.Sources,
		CompleteSources:    a.CompleteSources + b.CompleteSources,
	}
	out.finish()
	return out
}

func (u *UsageAnalysis) finish() {
	u.Inclusive = addUsageSlice(addUsageSlice(u.RootConversational, u.RootMaintenance), addUsageSlice(u.DescendantConversational, u.DescendantMaintenance))
}

func (u *UsageAnalysis) add(other UsageAnalysis) {
	u.RootConversational = addUsageSlice(u.RootConversational, other.RootConversational)
	u.RootMaintenance = addUsageSlice(u.RootMaintenance, other.RootMaintenance)
	u.DescendantConversational = addUsageSlice(u.DescendantConversational, other.DescendantConversational)
	u.DescendantMaintenance = addUsageSlice(u.DescendantMaintenance, other.DescendantMaintenance)
	u.Reconciliation.add(other.Reconciliation)
	u.finish()
}

func (r *UsageReconciliation) add(other UsageReconciliation) {
	if !other.Available {
		return
	}
	r.Available = true
	r.ComparedRoots += other.ComparedRoots
	r.Discrepancies += other.Discrepancies
	r.Persisted = addUsageSlice(r.Persisted, other.Persisted)
	r.DifferingFields = append(r.DifferingFields, other.DifferingFields...)
	r.Matches = r.Discrepancies == 0
}

func persistedUsageSlice(usage UsageTotals) UsageSlice {
	u := UsageSlice{
		Available:          true,
		Complete:           true,
		InputTokens:        max(0, usage.InputTokens),
		CacheReadTokens:    max(0, usage.CacheReadTokens),
		CacheWriteTokens:   max(0, usage.CacheWriteTokens),
		CacheWrite1hTokens: max(0, usage.CacheWrite1hTokens),
		OutputTokens:       max(0, usage.OutputTokens),
		ReasoningTokens:    max(0, usage.ReasoningTokens),
		KnownCostUSD:       usage.CostUSD,
		Sources:            1,
		CompleteSources:    1,
	}
	// UsageTotals predates explicit persisted pricing coverage. CostKnown on the
	// embedded usage is authoritative when present; otherwise a non-zero stored
	// aggregate cost is still known.
	u.finish()
	u.CostComplete = usage.Usage.CostKnown || usage.CostUSD != 0 || u.TotalTokens == 0
	return u
}

func reconcileUsage(raw UsageSlice, persisted UsageTotals) UsageReconciliation {
	stored := persistedUsageSlice(persisted)
	fields := make([]string, 0, 7)
	if raw.InputTokens != stored.InputTokens {
		fields = append(fields, "input_tokens")
	}
	if raw.CacheReadTokens != stored.CacheReadTokens {
		fields = append(fields, "cache_read_tokens")
	}
	if raw.CacheWriteTokens != stored.CacheWriteTokens {
		fields = append(fields, "cache_write_tokens")
	}
	if raw.CacheWrite1hTokens != stored.CacheWrite1hTokens {
		fields = append(fields, "cache_write_1h_tokens")
	}
	if raw.OutputTokens != stored.OutputTokens {
		fields = append(fields, "output_tokens")
	}
	if raw.ReasoningTokens != stored.ReasoningTokens {
		fields = append(fields, "reasoning_tokens")
	}
	if math.Abs(raw.KnownCostUSD-stored.KnownCostUSD) > 1e-9 && raw.CostComplete && stored.CostComplete {
		fields = append(fields, "cost_usd")
	}
	return UsageReconciliation{
		Available: true, Matches: len(fields) == 0, ComparedRoots: 1,
		Discrepancies: len(fields), DifferingFields: fields, Persisted: stored,
	}
}

func cohortIdentity(build BuildMetadata, runtime RuntimeProfile, available bool) CohortIdentity {
	identity := CohortIdentity{Available: available, Build: build, Runtime: runtime}
	if !available {
		identity.Key = "unavailable"
		return identity
	}
	canonical := struct {
		Build   BuildMetadata  `json:"build"`
		Runtime RuntimeProfile `json:"runtime"`
	}{Build: build, Runtime: runtime}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	identity.Key = "cohort-v1-" + hex.EncodeToString(sum[:8])
	return identity
}

func buildUsageDistributions(inclusiveTokens, rootTokens, childTokens []int, inclusiveCosts, rootCosts, childCosts []float64) UsageDistributions {
	return UsageDistributions{
		InclusiveTokens:        intDistribution(inclusiveTokens),
		RootTokens:             intDistribution(rootTokens),
		DescendantTokens:       intDistribution(childTokens),
		InclusiveKnownCostUSD:  floatDistribution(inclusiveCosts),
		RootKnownCostUSD:       floatDistribution(rootCosts),
		DescendantKnownCostUSD: floatDistribution(childCosts),
	}
}

func intDistribution(values []int) IntDistribution {
	if len(values) == 0 {
		return IntDistribution{}
	}
	values = append([]int(nil), values...)
	sort.Ints(values)
	return IntDistribution{Samples: len(values), Median: nearestRankInt(values, 0.5), P90: nearestRankInt(values, 0.9)}
}

func floatDistribution(values []float64) FloatDistribution {
	if len(values) == 0 {
		return FloatDistribution{}
	}
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	return FloatDistribution{Samples: len(values), Median: nearestRankFloat(values, 0.5), P90: nearestRankFloat(values, 0.9)}
}

func nearestRankInt(values []int, percentile float64) int {
	index := int(math.Ceil(percentile*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	return values[index]
}

func nearestRankFloat(values []float64, percentile float64) float64 {
	index := int(math.Ceil(percentile*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	return values[index]
}
