package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"harness/internal/llm"
)

// AnalysisVersion is the stable schema version emitted by WriteAnalysisJSON.
const AnalysisVersion = 13

const (
	maxAnalysisTextSessions     = 100
	maxAnalysisMetadataBytes    = 8 << 20
	maxAnalysisDiscoveryEntries = 100_000
	maxAnalysisDiscoveryDepth   = 64
	maxAnalysisRawBytes         = 256 << 20
	maxAnalysisRawLineBytes     = 16 << 20
	maxAnalysisEventRecords     = 500_000
)

// AnalyzeOptions limits corpus analysis to a reproducible event prefix.
type AnalyzeOptions struct {
	Before time.Time
}

// AnalysisValue distinguishes unavailable telemetry from an observed zero and
// distinguishes a measured stream in which the requested milestone never
// occurred.
type AnalysisValue struct {
	Available bool `json:"available"`
	Observed  bool `json:"observed"`
	Value     int  `json:"value"`
}

// ClosureAnalysis summarizes prompt closure diagnostics.
type ClosureAnalysis struct {
	Available           bool           `json:"available"`
	Prompts             int            `json:"prompts"`
	Triggers            map[string]int `json:"triggers"`
	TurnBudgetExhausted int            `json:"turn_budget_exhausted"`
}

// WorkflowAnalysis keeps an explicitly supplied "unknown" outcome distinct
// from prompts for which no workflow status provider was present.
type WorkflowAnalysis struct {
	Available                   bool            `json:"available"`
	Prompts                     int             `json:"prompts"`
	Supplied                    int             `json:"supplied"`
	Unsupplied                  int             `json:"unsupplied"`
	Outcomes                    map[string]int  `json:"outcomes"`
	RemainingRequirementsTotal  int             `json:"remaining_requirements_total"`
	RemainingRequirements       IntDistribution `json:"remaining_requirements"`
	CompletionSourceAvailable   bool            `json:"completion_source_available"`
	CompletionSourceReports     int             `json:"completion_source_reports"`
	CompletionSourceUnavailable int             `json:"completion_source_unavailable"`
	remainingRequirementValues  []int
}

// CompletionAnalysis aggregates host-validated child outcome footers without
// exposing child Markdown or blocker bodies.
type CompletionAnalysis struct {
	Applicable            bool           `json:"applicable"`
	Reports               int            `json:"reports"`
	Unavailable           int            `json:"unavailable"`
	Outcomes              map[string]int `json:"outcomes"`
	Validation            map[string]int `json:"validation"`
	Contracts             map[string]int `json:"contracts"`
	ParentReworkAvailable bool           `json:"parent_rework_available"`
	ParentReworkObserved  int            `json:"parent_rework_observed"`
}

// ProgressAnalysis summarizes turn_progress records. Pending batching steers
// have not yet had two subsequent tool turns or a completed prompt by the
// analyzed cutoff and therefore are not treated as failures.
type ProgressAnalysis struct {
	Available                     bool          `json:"available"`
	ToolTurns                     int           `json:"tool_turns"`
	MaxInspectionNoProgressStreak AnalysisValue `json:"max_inspection_no_progress_streak"`
	TurnsToFirstMutation          AnalysisValue `json:"turns_to_first_successful_mutation"`
	TurnsToFirstVerification      AnalysisValue `json:"turns_to_first_successful_verification"`
	BatchingSteers                int           `json:"batching_steers"`
	BatchingCompliant             int           `json:"batching_compliant_within_two_tool_turns"`
	BatchingNoncompliant          int           `json:"batching_noncompliant"`
	BatchingPending               int           `json:"batching_pending"`
}

// HookAnalysis summarizes bounded hook diagnostics.
type HookAnalysis struct {
	Available        bool `json:"available"`
	Diagnostics      int  `json:"diagnostics"`
	Timeouts         int  `json:"timeouts"`
	CircuitOpened    int  `json:"circuit_opened"`
	CircuitOpenSkips int  `json:"circuit_open_skips"`
}

// ContextAnalysis summarizes bounded context-accounting telemetry. Provider
// count scopes remain separate because payload-only counts are not interchangeable
// with effective logical-context counts.
type ContextAnalysis struct {
	Available              bool           `json:"available"`
	Samples                int            `json:"samples"`
	MaxTotalTokens         int            `json:"max_total_tokens"`
	MaxPayloadTokens       int            `json:"max_payload_tokens"`
	MaxProviderInputTokens int            `json:"max_provider_input_tokens"`
	ProviderCountScopes    map[string]int `json:"provider_count_scopes"`
	ProviderMaxByScope     map[string]int `json:"provider_max_by_scope"`
}

// RetentionAnalysis summarizes non-content retention decisions and continuation
// resets from replay logs.
type RetentionAnalysis struct {
	Available               bool `json:"available"`
	Epochs                  int  `json:"epochs"`
	BlocksTrimmed           int  `json:"blocks_trimmed"`
	BytesRemoved            int  `json:"bytes_removed"`
	EstimatedTokensRemoved  int  `json:"estimated_tokens_removed"`
	ResponseStateResets     int  `json:"response_state_resets"`
	ContinuationStateResets int  `json:"continuation_state_resets"`
	MeasurementAnchorResets int  `json:"measurement_anchor_resets"`
	NextRequestStateful     int  `json:"next_request_stateful"`
	NextRequestFull         int  `json:"next_request_full"`
}

// TrajectoryAnalysis measures current evaluator, stagnation, and mutation
// telemetry without exposing candidate IDs, scores, evidence references, or
// modified paths. Mutation attribution is derived passively from raw events and
// never participates in model-visible policy.
type TrajectoryAnalysis struct {
	Available                 bool `json:"available"`
	Streams                   int  `json:"streams"`
	Schema                    int  `json:"schema"`
	Transitions               int  `json:"transitions"`
	BranchResets              int  `json:"branch_resets"`
	Evaluations               int  `json:"evaluations"`
	AcceptedEvaluations       int  `json:"accepted_evaluations"`
	RejectedEvaluations       int  `json:"rejected_evaluations"`
	ActiveNoImprovementStreak int  `json:"active_no_improvement_streak"`
	MaxNoImprovementStreak    int  `json:"max_no_improvement_streak"`
	StagnationBaselines       int  `json:"stagnation_baselines"`
	StagnationImprovements    int  `json:"stagnation_improvements"`
	StagnationPlateaus        int  `json:"stagnation_plateaus"`
	StagnationRegressions     int  `json:"stagnation_regressions"`
	StagnationIndeterminate   int  `json:"stagnation_indeterminate"`
	UnorderedScoreEvaluations int  `json:"unordered_score_evaluations"`
	StagnationLaneResets      int  `json:"stagnation_lane_resets"`
	StagnationNudges          int  `json:"stagnation_nudges"`
	MutationPathObservations  int  `json:"mutation_path_observations"`
	DiffPathConfirmations     int  `json:"diff_path_confirmations"`
	ActiveModifiedPaths       int  `json:"active_modified_paths"`
	ActiveConfirmedPaths      int  `json:"active_confirmed_paths"`
	UnconfirmedMutationPaths  int  `json:"unconfirmed_mutation_paths"`
	InvalidMutationPaths      int  `json:"invalid_mutation_paths"`
}

// InvariantAnalysis records impossible values without exposing event payloads.
type InvariantAnalysis struct {
	ContextAvailable                bool `json:"context_available"`
	RetentionAvailable              bool `json:"retention_available"`
	UsageReconciliationAvailable    bool `json:"usage_reconciliation_available"`
	NegativeContextViolations       int  `json:"negative_context_violations"`
	InconsistentRetentionViolations int  `json:"inconsistent_retention_violations"`
	UsageReconciliationViolations   int  `json:"usage_reconciliation_violations"`
}

// TelemetryAnalysis is reusable by single-session stats and corpus reports.
type TelemetryAnalysis struct {
	Closure    ClosureAnalysis    `json:"closure"`
	Workflow   WorkflowAnalysis   `json:"workflow"`
	Completion CompletionAnalysis `json:"completion"`
	Progress   ProgressAnalysis   `json:"progress"`
	Hooks      HookAnalysis       `json:"hooks"`
	Context    ContextAnalysis    `json:"context"`
	Retention  RetentionAnalysis  `json:"retention"`
	Trajectory TrajectoryAnalysis `json:"trajectory"`
	Invariants InvariantAnalysis  `json:"invariants"`
}

// AnalysisSource describes the immutable raw.ndjson prefix considered for a
// physical session stream. It deliberately contains no transcript fields.
type AnalysisSource struct {
	Path          string `json:"path"`
	Status        string `json:"status"`
	SnapshotBytes int64  `json:"snapshot_bytes"`
	IncludedBytes int    `json:"included_bytes"`
	Events        int    `json:"events"`
	SHA256        string `json:"sha256,omitempty"`
}

// ExecutionAnalysis summarizes transcript-free loop activity and completion.
type ExecutionAnalysis struct {
	Completeness         string         `json:"completeness"`
	Prompts              int            `json:"prompts"`
	CompletedPrompts     int            `json:"completed_prompts"`
	Turns                int            `json:"turns"`
	ToolCalls            int            `json:"tool_calls"`
	ToolResults          int            `json:"tool_results"`
	ToolErrors           int            `json:"tool_errors"`
	CommandResults       int            `json:"command_results"`
	CommandFailures      int            `json:"command_failures"`
	CommandCancellations int            `json:"command_cancellations"`
	EffectiveFailures    int            `json:"effective_failures"`
	ModelErrors          int            `json:"model_errors"`
	TerminationAvailable bool           `json:"termination_available"`
	Terminations         map[string]int `json:"terminations"`
}

// ExecutionIdentityAnalysis summarizes immutable per-attempt identity evidence.
// Available requires complete identity fields on every observed attempt; Stable
// additionally requires one agent/provider/model tuple matching final metadata.
type ExecutionIdentityAnalysis struct {
	Available bool   `json:"available"`
	Stable    bool   `json:"stable"`
	Attempts  int    `json:"attempts"`
	Agent     string `json:"agent,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
}

// SessionAnalysis is one physical root or delegate stream.
type SessionAnalysis struct {
	Path              string                    `json:"path"`
	RootPath          string                    `json:"root_path"`
	RootID            string                    `json:"root_id,omitempty"`
	CohortKey         string                    `json:"cohort_key"`
	ID                string                    `json:"id,omitempty"`
	ParentID          string                    `json:"parent_id,omitempty"`
	Agent             string                    `json:"agent,omitempty"`
	Provider          string                    `json:"provider,omitempty"`
	Model             string                    `json:"model,omitempty"`
	Delegate          bool                      `json:"delegate"`
	MetadataStatus    string                    `json:"metadata_status"`
	BuildAvailable    bool                      `json:"build_available"`
	RuntimeAvailable  bool                      `json:"runtime_available"`
	Build             BuildMetadata             `json:"build"`
	Runtime           RuntimeProfile            `json:"runtime"`
	Source            AnalysisSource            `json:"source"`
	Execution         ExecutionAnalysis         `json:"execution"`
	ExecutionIdentity ExecutionIdentityAnalysis `json:"execution_identity"`
	Telemetry         TelemetryAnalysis         `json:"telemetry"`
	Usage             UsageAnalysis             `json:"usage"`
	Storage           StorageAnalysis           `json:"storage"`
}

// TelemetryCoverage counts physical streams carrying each structured signal.
type TelemetryCoverage struct {
	Sessions                   int `json:"sessions"`
	Closure                    int `json:"closure"`
	Workflow                   int `json:"workflow"`
	CompletionApplicable       int `json:"completion_applicable"`
	CompletionValid            int `json:"completion_valid"`
	CompletionCoverageFailures int `json:"completion_coverage_failures"`
	Progress                   int `json:"progress"`
	Hooks                      int `json:"hooks"`
	Context                    int `json:"context"`
	ProviderCountScope         int `json:"provider_count_scope"`
	Retention                  int `json:"retention"`
	Trajectory                 int `json:"trajectory"`
}

// AnalysisReport is a deterministic, transcript-free corpus report. Path may
// name one session root or a directory containing session roots.
type AnalysisReport struct {
	Version                int                 `json:"version"`
	Path                   string              `json:"path"`
	Before                 *time.Time          `json:"before"`
	Roots                  int                 `json:"roots"`
	Sessions               int                 `json:"sessions"`
	MissingStreams         int                 `json:"missing_streams"`
	IncompleteStreams      int                 `json:"incomplete_streams"`
	MalformedStreams       int                 `json:"malformed_streams"`
	SymlinkStreams         int                 `json:"symlink_streams"`
	LimitExceededStreams   int                 `json:"limit_exceeded_streams"`
	MalformedChildMetadata int                 `json:"malformed_child_metadata"`
	Completeness           map[string]int      `json:"completeness"`
	Execution              ExecutionAnalysis   `json:"execution"`
	Telemetry              TelemetryAnalysis   `json:"telemetry"`
	Coverage               TelemetryCoverage   `json:"telemetry_coverage"`
	Usage                  UsageAnalysis       `json:"usage"`
	Storage                StorageAnalysis     `json:"storage"`
	Distributions          UsageDistributions  `json:"distributions"`
	Hierarchies            []HierarchyAnalysis `json:"hierarchies"`
	Cohorts                []CohortAnalysis    `json:"cohorts"`
	Items                  []SessionAnalysis   `json:"items"`
}

type analysisStream struct {
	dir      string
	delegate bool
	meta     ChildMeta
	metaOK   bool
	metaBad  bool
}

type analysisMetadata struct {
	id, parent, agent, provider, model string
	build                              BuildMetadata
	runtime                            RuntimeProfile
	buildAvailable                     bool
	runtimeAvailable                   bool
	persistedUsage                     *UsageTotals
}

// AnalyzeCorpus recursively analyzes one session root or a directory of roots.
// Delegate children are owned by their nearest discovered root, symlinks are
// never followed, and every physical child directory is counted at most once.
func AnalyzeCorpus(path string, opts AnalyzeOptions) (AnalysisReport, error) {
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return AnalysisReport{}, fmt.Errorf("session: analyze corpus: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return AnalysisReport{}, fmt.Errorf("session: analyze corpus: refusing symlink %s", clean)
	}
	if !info.IsDir() {
		return AnalysisReport{}, fmt.Errorf("session: analyze corpus: %s is not a directory", clean)
	}

	roots, err := discoverAnalysisRoots(clean)
	if err != nil {
		return AnalysisReport{}, err
	}
	return analyzeSessionRoots(clean, roots, opts)
}

// AnalyzeSessionDirs analyzes a preselected set of root session directories.
// It is intended for callers that apply an outer history cutoff before walking
// each root's delegate hierarchy. Directory order does not affect the report.
func AnalyzeSessionDirs(path string, dirs []string, opts AnalyzeOptions) (AnalysisReport, error) {
	roots := make([]string, 0, len(dirs))
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		clean := filepath.Clean(dir)
		if _, ok := seen[clean]; ok {
			continue
		}
		info, err := os.Lstat(clean)
		if err != nil {
			return AnalysisReport{}, fmt.Errorf("session: analyze corpus: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return AnalysisReport{}, fmt.Errorf("session: analyze corpus: invalid session directory %s", clean)
		}
		seen[clean] = struct{}{}
		roots = append(roots, clean)
	}
	sort.Strings(roots)
	return analyzeSessionRoots(filepath.Clean(path), roots, opts)
}

type cohortAccumulator struct {
	analysis        CohortAnalysis
	inclusiveTokens []int
	rootTokens      []int
	childTokens     []int
	inclusiveCosts  []float64
	rootCosts       []float64
	childCosts      []float64
}

func analyzeSessionRoots(path string, roots []string, opts AnalyzeOptions) (AnalysisReport, error) {
	report := AnalysisReport{
		Version: AnalysisVersion, Path: path, Roots: len(roots),
		Completeness: make(map[string]int),
		Execution:    ExecutionAnalysis{Terminations: make(map[string]int)},
	}
	if !opts.Before.IsZero() {
		before := opts.Before.UTC()
		report.Before = &before
	}
	cohorts := make(map[string]*cohortAccumulator)
	var inclusiveTokens, rootTokens, childTokens []int
	var inclusiveCosts, rootCosts, childCosts []float64
	for _, root := range roots {
		streams, err := discoverAnalysisTree(root)
		if err != nil {
			return AnalysisReport{}, err
		}
		rootMeta := readAnalysisMetadata(root, streams[0])
		cohort := cohortIdentity(rootMeta.build, rootMeta.runtime, rootMeta.buildAvailable && rootMeta.runtimeAvailable)
		acc := cohorts[cohort.Key]
		if acc == nil {
			acc = &cohortAccumulator{analysis: CohortAnalysis{
				Cohort:     cohort,
				Execution:  ExecutionAnalysis{Terminations: make(map[string]int)},
				Workflow:   WorkflowAnalysis{Outcomes: make(map[string]int)},
				Completion: CompletionAnalysis{Outcomes: make(map[string]int), Validation: make(map[string]int), Contracts: make(map[string]int)},
			}}
			cohorts[cohort.Key] = acc
		}
		acc.analysis.Roots++
		hierarchy := UsageAnalysis{}
		hierarchyExecution := ExecutionAnalysis{Terminations: make(map[string]int)}
		hierarchyWorkflow := WorkflowAnalysis{Outcomes: make(map[string]int)}
		hierarchyCompletion := CompletionAnalysis{Outcomes: make(map[string]int), Validation: make(map[string]int), Contracts: make(map[string]int)}
		var hierarchyStorage StorageAnalysis
		allRawComplete := opts.Before.IsZero()
		rootItem := -1
		for _, stream := range streams {
			events, source, err := readAnalysisEvents(stream.dir, opts.Before)
			if err != nil {
				return AnalysisReport{}, fmt.Errorf("session: analyze %s: %w", stream.dir, err)
			}
			metadata := readAnalysisMetadata(stream.dir, stream)
			item := SessionAnalysis{
				Path: stream.dir, RootPath: root, RootID: rootMeta.id, CohortKey: cohort.Key,
				Delegate: stream.delegate, Source: source,
				ID: metadata.id, ParentID: metadata.parent, Agent: metadata.agent,
				Provider: metadata.provider, Model: metadata.model,
				BuildAvailable: metadata.buildAvailable, RuntimeAvailable: metadata.runtimeAvailable,
				Build: metadata.build, Runtime: metadata.runtime,
			}
			switch {
			case stream.delegate && stream.metaBad:
				item.MetadataStatus = "malformed"
				report.MalformedChildMetadata++
			case stream.delegate && stream.metaOK:
				item.MetadataStatus = "available"
			case stream.delegate:
				item.MetadataStatus = "missing"
			case metadata.buildAvailable || metadata.runtimeAvailable || metadata.persistedUsage != nil:
				item.MetadataStatus = "available"
			default:
				item.MetadataStatus = "missing"
			}
			var fallback *ChildMeta
			if stream.metaOK && opts.Before.IsZero() {
				fallback = &stream.meta
			}
			item.Execution = deriveExecution(events, source.Status)
			item.ExecutionIdentity = deriveExecutionIdentity(events, source.Status, metadata)
			item.Telemetry = deriveTelemetry(events, fallback)
			if stream.delegate {
				item.Telemetry.Completion = deriveCompletion(fallback, true)
			}
			conversation, maintenance := usageFromEvents(events, source.Status)
			if stream.delegate {
				item.Usage.DescendantConversational = conversation
				item.Usage.DescendantMaintenance = maintenance
			} else {
				item.Usage.RootConversational = conversation
				item.Usage.RootMaintenance = maintenance
				rootItem = len(report.Items)
			}
			item.Usage.finish()
			item.Storage, err = analyzeStorage(stream.dir, source, opts.Before)
			if err != nil {
				return AnalysisReport{}, err
			}
			hierarchy.add(item.Usage)
			hierarchyExecution.add(item.Execution)
			hierarchyWorkflow.add(item.Telemetry.Workflow)
			hierarchyCompletion.add(item.Telemetry.Completion)
			hierarchyStorage.add(item.Storage)
			allRawComplete = allRawComplete && source.Status == "complete"
			report.Completeness[item.Execution.Completeness]++
			report.Execution.add(item.Execution)
			report.Telemetry.add(item.Telemetry)
			report.Coverage.add(item.Telemetry, events)
			report.Storage.add(item.Storage)
			report.Items = append(report.Items, item)
			switch source.Status {
			case "missing":
				report.MissingStreams++
			case "incomplete":
				report.IncompleteStreams++
			case "malformed":
				report.MalformedStreams++
			case "symlink":
				report.SymlinkStreams++
			case "limit_exceeded":
				report.LimitExceededStreams++
			}
		}
		if allRawComplete && hierarchy.Inclusive.Complete && rootMeta.persistedUsage != nil {
			hierarchy.Reconciliation = reconcileUsage(hierarchy.Inclusive, *rootMeta.persistedUsage)
			report.Telemetry.Invariants.UsageReconciliationAvailable = true
			report.Telemetry.Invariants.UsageReconciliationViolations += hierarchy.Reconciliation.Discrepancies
			if rootItem >= 0 {
				report.Items[rootItem].Usage.Reconciliation = hierarchy.Reconciliation
			}
		}
		report.Usage.add(hierarchy)
		report.Hierarchies = append(report.Hierarchies, HierarchyAnalysis{
			RootPath: root, RootID: rootMeta.id, Cohort: cohort, Sessions: len(streams),
			Execution: hierarchyExecution, Workflow: hierarchyWorkflow, Completion: hierarchyCompletion,
			Usage: hierarchy, Storage: hierarchyStorage,
		})
		acc.analysis.Sessions += len(streams)
		acc.analysis.Execution.add(hierarchyExecution)
		acc.analysis.Workflow.add(hierarchyWorkflow)
		acc.analysis.Completion.add(hierarchyCompletion)
		acc.analysis.Usage.add(hierarchy)
		acc.analysis.Storage.add(hierarchyStorage)
		if allRawComplete && hierarchy.Inclusive.Complete {
			rootTotal := addUsageSlice(hierarchy.RootConversational, hierarchy.RootMaintenance)
			childTotal := addUsageSlice(hierarchy.DescendantConversational, hierarchy.DescendantMaintenance)
			inclusiveTokens = append(inclusiveTokens, hierarchy.Inclusive.TotalTokens)
			rootTokens = append(rootTokens, rootTotal.TotalTokens)
			childTokens = append(childTokens, childTotal.TotalTokens)
			acc.inclusiveTokens = append(acc.inclusiveTokens, hierarchy.Inclusive.TotalTokens)
			acc.rootTokens = append(acc.rootTokens, rootTotal.TotalTokens)
			acc.childTokens = append(acc.childTokens, childTotal.TotalTokens)
			if hierarchy.Inclusive.CostComplete {
				inclusiveCosts = append(inclusiveCosts, hierarchy.Inclusive.KnownCostUSD)
				rootCosts = append(rootCosts, rootTotal.KnownCostUSD)
				childCosts = append(childCosts, childTotal.KnownCostUSD)
				acc.inclusiveCosts = append(acc.inclusiveCosts, hierarchy.Inclusive.KnownCostUSD)
				acc.rootCosts = append(acc.rootCosts, rootTotal.KnownCostUSD)
				acc.childCosts = append(acc.childCosts, childTotal.KnownCostUSD)
			}
		}
	}
	report.Sessions = len(report.Items)
	report.Distributions = buildUsageDistributions(inclusiveTokens, rootTokens, childTokens, inclusiveCosts, rootCosts, childCosts)
	keys := make([]string, 0, len(cohorts))
	for key := range cohorts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		acc := cohorts[key]
		acc.analysis.Distributions = buildUsageDistributions(acc.inclusiveTokens, acc.rootTokens, acc.childTokens, acc.inclusiveCosts, acc.rootCosts, acc.childCosts)
		report.Cohorts = append(report.Cohorts, acc.analysis)
	}
	sort.Slice(report.Hierarchies, func(i, j int) bool { return report.Hierarchies[i].RootPath < report.Hierarchies[j].RootPath })
	sort.Slice(report.Items, func(i, j int) bool { return report.Items[i].Path < report.Items[j].Path })
	return report, nil
}

func discoverAnalysisRoots(path string) ([]string, error) {
	if analysisSessionDir(path) {
		return []string{path}, nil
	}
	var roots []string
	visited := 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > maxAnalysisDiscoveryDepth {
			return fmt.Errorf("session: discovery depth exceeds %d at %s", maxAnalysisDiscoveryDepth, dir)
		}
		err := forEachAnalysisDirEntry(dir, func(entry os.DirEntry) error {
			visited++
			if visited > maxAnalysisDiscoveryEntries {
				return fmt.Errorf("session: discovery entries exceed %d", maxAnalysisDiscoveryEntries)
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return nil
			}
			child := filepath.Join(dir, entry.Name())
			if analysisSessionDir(child) {
				roots = append(roots, child)
				return nil
			}
			return walk(child, depth+1)
		})
		if err != nil {
			return fmt.Errorf("session: discover roots in %s: %w", dir, err)
		}
		return nil
	}
	if err := walk(path, 0); err != nil {
		return nil, err
	}
	sort.Strings(roots)
	return roots, nil
}

func analysisSessionDir(dir string) bool {
	for _, name := range []string{stateFile, eventLog, treeFile} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func discoverAnalysisTree(root string) ([]analysisStream, error) {
	streams := []analysisStream{{dir: root}}
	visited := 1
	var children func(string, int) error
	children = func(parent string, depth int) error {
		if depth > maxAnalysisDiscoveryDepth {
			return fmt.Errorf("session: delegate discovery depth exceeds %d at %s", maxAnalysisDiscoveryDepth, parent)
		}
		childrenDir := filepath.Join(parent, "children")
		info, err := os.Lstat(childrenDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("session: discover delegates in %s: %w", parent, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.IsDir() {
			return fmt.Errorf("session: discover delegates in %s: children is not a directory", parent)
		}
		err = forEachAnalysisDirEntry(childrenDir, func(entry os.DirEntry) error {
			visited++
			if visited > maxAnalysisDiscoveryEntries {
				return fmt.Errorf("session: delegate discovery entries exceed %d", maxAnalysisDiscoveryEntries)
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return nil
			}
			dir := filepath.Join(childrenDir, entry.Name())
			stream := analysisStream{dir: dir, delegate: true}
			metaPath := filepath.Join(dir, "meta.json")
			metaInfo, statErr := os.Lstat(metaPath)
			switch {
			case statErr == nil && metaInfo.Mode().IsRegular():
				data, readErr := readAnalysisMetadataFile(metaPath)
				if readErr == nil && json.Unmarshal(data, &stream.meta) == nil {
					stream.meta.TaskPreview = ""
					stream.meta.Transcript = ""
					stream.meta.Replay = ""
					stream.meta.Error = ""
					stream.metaOK = true
				} else {
					stream.metaBad = true
				}
			case statErr == nil:
				stream.metaBad = true
			case !errors.Is(statErr, os.ErrNotExist):
				stream.metaBad = true
			}
			streams = append(streams, stream)
			return children(dir, depth+1)
		})
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("session: discover delegates in %s: %w", parent, err)
		}
		return nil
	}
	return streams, children(root, 0)
}

func readAnalysisMetadata(dir string, stream analysisStream) analysisMetadata {
	var out analysisMetadata
	if stream.metaOK {
		out.id, out.parent, out.agent = stream.meta.ID, stream.meta.ParentID, stream.meta.Agent
		out.provider, out.model = stream.meta.Provider, stream.meta.Model
		out.build, out.runtime = stream.meta.Build, stream.meta.Runtime
		out.buildAvailable = stream.meta.Build.Version != "" || stream.meta.Build.Commit != "" || stream.meta.Build.Date != "" || stream.meta.Build.Modified
		out.runtimeAvailable = runtimeProfilePresent(stream.meta.Runtime)
	}
	statePath := filepath.Join(dir, stateFile)
	info, err := os.Lstat(statePath)
	if err != nil || !info.Mode().IsRegular() {
		return out
	}
	data, err := readAnalysisMetadataFile(statePath)
	if err != nil {
		return out
	}
	var state struct {
		ID            string         `json:"id"`
		ParentSession string         `json:"parent_session"`
		Agent         string         `json:"agent"`
		Provider      string         `json:"provider"`
		Model         string         `json:"model"`
		Build         BuildMetadata  `json:"build"`
		Runtime       RuntimeProfile `json:"runtime"`
		Usage         UsageTotals    `json:"usage"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &state) != nil || json.Unmarshal(data, &fields) != nil {
		return out
	}
	if state.ID != "" {
		out.id = state.ID
	}
	if state.ParentSession != "" {
		out.parent = state.ParentSession
	}
	if state.Agent != "" {
		out.agent = state.Agent
	}
	if state.Provider != "" {
		out.provider = state.Provider
	}
	if state.Model != "" {
		out.model = state.Model
	}
	if _, ok := fields["build"]; ok {
		out.build, out.buildAvailable = state.Build, true
	}
	if _, ok := fields["runtime"]; ok {
		out.runtime, out.runtimeAvailable = state.Runtime, true
	}
	if _, ok := fields["usage"]; ok {
		usage := state.Usage
		out.persistedUsage = &usage
	}
	return out
}

func forEachAnalysisDirEntry(dir string, visit func(os.DirEntry) error) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		entries, readErr := f.ReadDir(256)
		for _, entry := range entries {
			if err := visit(entry); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func readAnalysisMetadataFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxAnalysisMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAnalysisMetadataBytes {
		return nil, fmt.Errorf("session: analysis metadata %s exceeds %d bytes", path, maxAnalysisMetadataBytes)
	}
	return data, nil
}

func runtimeProfilePresent(runtime RuntimeProfile) bool {
	return runtime.RetentionPolicy != "" || runtime.ContextWindow != 0 ||
		runtime.ToolResultMaxBytes != 0 || runtime.ToolResultMaxLines != 0 ||
		runtime.CompactToolResultMaxBytes != 0 || runtime.CompactTimeoutSeconds != 0 || runtime.ResponsesStateful || runtime.NativeCompaction ||
		runtime.DelegateMaxTurns != 0 || runtime.DelegateMaxActive != 0 || runtime.DelegateMaxDescendants != 0 ||
		runtime.Prewarm || runtime.SearchBackend != "" || runtime.StagnationNudge
}

type analysisEventLimits struct {
	maxBytes   int64
	maxLine    int
	maxRecords int
}

var errAnalysisLineTooLong = errors.New("analysis event line exceeds limit")

func readAnalysisEvents(dir string, before time.Time) ([]Event, AnalysisSource, error) {
	return readAnalysisEventsWithLimits(dir, before, analysisEventLimits{
		maxBytes: maxAnalysisRawBytes, maxLine: maxAnalysisRawLineBytes, maxRecords: maxAnalysisEventRecords,
	})
}

func readAnalysisEventsWithLimits(dir string, before time.Time, limits analysisEventLimits) ([]Event, AnalysisSource, error) {
	path := filepath.Join(dir, eventLog)
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, AnalysisSource{Path: dir, Status: "missing"}, nil
	}
	if err != nil {
		return nil, AnalysisSource{}, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, AnalysisSource{Path: dir, Status: "symlink"}, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, AnalysisSource{Path: dir, Status: "missing"}, nil
	}
	if err != nil {
		return nil, AnalysisSource{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, AnalysisSource{}, err
	}
	status := "complete"
	readBytes := info.Size()
	if readBytes > limits.maxBytes {
		readBytes = limits.maxBytes
		status = "limit_exceeded"
	}
	reader := bufio.NewReader(io.LimitReader(f, readBytes))
	hash := sha256.New()
	includedBytes := 0
	records := 0
	var events []Event
	for {
		line, complete, readErr := readBoundedAnalysisLine(reader, limits.maxLine)
		if errors.Is(readErr, errAnalysisLineTooLong) {
			status = "limit_exceeded"
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, AnalysisSource{}, readErr
		}
		if complete && len(bytes.TrimSpace(line)) > 0 {
			records++
			if records > limits.maxRecords {
				status = "limit_exceeded"
				break
			}
			var event Event
			if err := json.Unmarshal(line, &event); err != nil {
				if status != "limit_exceeded" {
					status = "malformed"
				}
			} else if before.IsZero() || !event.Time.After(before) {
				_, _ = hash.Write(line)
				_, _ = hash.Write([]byte{'\n'})
				includedBytes += len(line) + 1
				event.Text = ""
				event.Display = ""
				// Tool-diff paths are needed only to correlate the shadow
				// trajectory's host-observed mutation confirmations. No path or
				// trajectory identifier is emitted by either analysis renderer.
				if event.Type != EventToolDiff {
					event.Path = ""
				}
				event.Input = nil
				event.Images = nil
				event.Summary = ""
				event.ErrorExcerpt = ""
				if event.ModelRequest != nil {
					event.ModelRequest.Message = ""
					event.ModelRequest.ResponsePayload = ""
				}
				events = append(events, event)
			}
		}
		if errors.Is(readErr, io.EOF) {
			if len(line) > 0 && status == "complete" {
				status = "incomplete"
			}
			break
		}
	}
	return events, AnalysisSource{
		Path: dir, Status: status, SnapshotBytes: info.Size(), IncludedBytes: includedBytes,
		Events: len(events), SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func readBoundedAnalysisLine(reader *bufio.Reader, maxBytes int) ([]byte, bool, error) {
	line := make([]byte, 0, min(maxBytes, 64<<10))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxBytes {
			return nil, false, errAnalysisLineTooLong
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line[:len(line)-1], true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, false, io.EOF
		default:
			return nil, false, err
		}
	}
}

func (c *TelemetryCoverage) add(telemetry TelemetryAnalysis, events []Event) {
	c.Sessions++
	if telemetry.Closure.Available {
		c.Closure++
	}
	if telemetry.Workflow.Available {
		c.Workflow++
	}
	if telemetry.Completion.Applicable {
		c.CompletionApplicable++
		if telemetry.Completion.Reports > 0 {
			c.CompletionValid++
		} else {
			c.CompletionCoverageFailures++
		}
	}
	if telemetry.Progress.Available {
		c.Progress++
	}
	if telemetry.Hooks.Available {
		c.Hooks++
	}
	if telemetry.Invariants.ContextAvailable {
		c.Context++
	}
	if telemetry.Invariants.RetentionAvailable {
		c.Retention++
	}
	if telemetry.Trajectory.Available {
		c.Trajectory++
	}
	for _, event := range events {
		if event.Context != nil && event.Context.ProviderInputScope != "" {
			c.ProviderCountScope++
			break
		}
	}
}

func deriveExecutionIdentity(events []Event, sourceStatus string, metadata analysisMetadata) ExecutionIdentityAnalysis {
	if sourceStatus != "complete" {
		return ExecutionIdentityAnalysis{}
	}
	var out ExecutionIdentityAnalysis
	for _, event := range events {
		if event.Type != EventTurnAttemptStart {
			continue
		}
		out.Attempts++
		if event.Agent == "" || event.Provider == "" || event.Model == "" {
			return out
		}
		if out.Agent == "" {
			out.Agent, out.Provider, out.Model = event.Agent, event.Provider, event.Model
			continue
		}
		if event.Agent != out.Agent || event.Provider != out.Provider || event.Model != out.Model {
			out.Available = true
			return out
		}
	}
	if out.Attempts == 0 {
		return out
	}
	out.Available = true
	out.Stable = out.Agent == metadata.agent && out.Provider == metadata.provider && out.Model == metadata.model
	return out
}

func deriveExecution(events []Event, sourceStatus string) ExecutionAnalysis {
	out := ExecutionAnalysis{Terminations: make(map[string]int)}
	prompts := make(map[int]struct{})
	completed := make(map[int]struct{})
	turns := make(map[[2]int]struct{})
	for _, event := range events {
		if event.Prompt > 0 {
			prompts[event.Prompt] = struct{}{}
		}
		if event.Prompt > 0 && event.Turn > 0 {
			switch event.Type {
			case EventTurnComplete, EventTurnProgress, EventTurnAttemptStart:
				turns[[2]int{event.Prompt, event.Turn}] = struct{}{}
			}
		}
		switch event.Type {
		case EventPromptUsage:
			completed[event.Prompt] = struct{}{}
			if event.TerminationReason != "" {
				out.TerminationAvailable = true
				out.Terminations[normalizedTermination(event.TerminationReason)]++
			}
		case EventToolStart:
			out.ToolCalls++
		case EventToolResult:
			out.ToolResults++
			if event.ResultError {
				out.ToolErrors++
			}
			addExecutionCommandResult(&out, event.ResultMetrics)
		case EventBackgroundJobResult:
			addExecutionCommandResult(&out, event.ResultMetrics)
		case EventModelRequest:
			if event.ModelRequest != nil && event.ModelRequest.State == llm.ModelRequestFailed {
				out.ModelErrors++
			}
		}
	}
	out.Prompts = len(prompts)
	out.CompletedPrompts = len(completed)
	out.Turns = len(turns)
	out.EffectiveFailures = out.ToolErrors + out.CommandFailures
	switch {
	case sourceStatus == "missing" || sourceStatus == "symlink":
		out.Completeness = "unavailable"
	case sourceStatus == "incomplete" || sourceStatus == "malformed" || sourceStatus == "limit_exceeded":
		out.Completeness = "incomplete"
	case out.Prompts == 0:
		out.Completeness = "unknown"
	case out.CompletedPrompts == out.Prompts:
		out.Completeness = "complete"
	default:
		out.Completeness = "incomplete"
	}
	return out
}

func addExecutionCommandResult(out *ExecutionAnalysis, metrics map[string]int) {
	if metrics["command_outcome_available"] == 0 {
		return
	}
	out.CommandResults++
	if metrics["command_failed"] != 0 {
		out.CommandFailures++
	}
	if metrics["command_cancelled"] != 0 {
		out.CommandCancellations++
	}
}

func (e *ExecutionAnalysis) add(other ExecutionAnalysis) {
	if e.Terminations == nil {
		e.Terminations = make(map[string]int)
	}
	e.Prompts += other.Prompts
	e.CompletedPrompts += other.CompletedPrompts
	e.Turns += other.Turns
	e.ToolCalls += other.ToolCalls
	e.ToolResults += other.ToolResults
	e.ToolErrors += other.ToolErrors
	e.CommandResults += other.CommandResults
	e.CommandFailures += other.CommandFailures
	e.CommandCancellations += other.CommandCancellations
	e.EffectiveFailures += other.EffectiveFailures
	e.ModelErrors += other.ModelErrors
	e.TerminationAvailable = e.TerminationAvailable || other.TerminationAvailable
	for reason, count := range other.Terminations {
		e.Terminations[reason] += count
	}
	if completenessRank(other.Completeness) > completenessRank(e.Completeness) {
		e.Completeness = other.Completeness
	}
}

func completenessRank(value string) int {
	switch value {
	case "incomplete":
		return 4
	case "unavailable":
		return 3
	case "unknown":
		return 2
	case "complete":
		return 1
	default:
		return 0
	}
}

func normalizedTermination(value string) string {
	switch value = strings.TrimSpace(value); value {
	case "model_completed", "turn_limit", "cancelled", "repeat_guard", "error_guard", "error":
		return value
	default:
		return "unknown"
	}
}

func deriveTelemetry(events []Event, child *ChildMeta) TelemetryAnalysis {
	out := TelemetryAnalysis{
		Closure:  ClosureAnalysis{Triggers: make(map[string]int)},
		Workflow: WorkflowAnalysis{Outcomes: make(map[string]int)},
		Context:  ContextAnalysis{ProviderCountScopes: make(map[string]int), ProviderMaxByScope: make(map[string]int)},
	}
	closures := make(map[int]string)
	closurePrompts := make(map[int]struct{})
	completed := make(map[int]bool)
	workflow := make(map[int]*WorkflowStatusSnapshot)
	var progress []Event
	schemaAvailable := false
	for _, event := range events {
		if event.TelemetryVersion > 0 {
			schemaAvailable = true
		}
		if context := event.Context; context != nil {
			out.Context.Available = true
			out.Context.Samples++
			out.Context.MaxTotalTokens = max(out.Context.MaxTotalTokens, max(0, context.Total))
			out.Context.MaxPayloadTokens = max(out.Context.MaxPayloadTokens, max(0, context.PayloadTotal))
			out.Context.MaxProviderInputTokens = max(out.Context.MaxProviderInputTokens, max(0, context.ProviderInputTokens))
			if context.ProviderInputScope != "" {
				scope := normalizedProviderCountScope(context.ProviderInputScope)
				out.Context.ProviderCountScopes[scope]++
				out.Context.ProviderMaxByScope[scope] = max(out.Context.ProviderMaxByScope[scope], max(0, context.ProviderInputTokens))
			}
			out.Invariants.ContextAvailable = true
			if negativeContext(context) || negativeCompatibleContextArithmetic(context) {
				out.Invariants.NegativeContextViolations++
			}
		}
		switch event.Type {
		case EventClosure:
			out.Closure.Available = true
			closurePrompts[event.Prompt] = struct{}{}
			closures[event.Prompt] = normalizedClosureTrigger(event.ClosureTrigger)
			if event.TelemetryVersion > 0 || event.WorkflowStatus != nil {
				out.Workflow.Available = true
				workflow[event.Prompt] = event.WorkflowStatus
			}
		case EventPromptUsage:
			completed[event.Prompt] = true
			if event.TelemetryVersion > 0 || event.ClosureTrigger != "" || event.TurnBudgetExhausted {
				out.Closure.Available = true
				closurePrompts[event.Prompt] = struct{}{}
			}
			if _, ok := closures[event.Prompt]; !ok && event.ClosureTrigger != "" {
				closures[event.Prompt] = normalizedClosureTrigger(event.ClosureTrigger)
			}
			if event.TurnBudgetExhausted {
				out.Closure.TurnBudgetExhausted++
			}
			if event.TelemetryVersion > 0 || event.WorkflowStatus != nil {
				out.Workflow.Available = true
				workflow[event.Prompt] = event.WorkflowStatus
			}
		case EventTurnProgress:
			if event.TurnProgress != nil {
				progress = append(progress, event)
			}
		case EventHookDiagnostic:
			if hook := event.HookDiagnostic; hook != nil {
				out.Hooks.Available = true
				out.Hooks.Diagnostics++
				if hook.Outcome == "timeout" {
					out.Hooks.Timeouts++
				}
				if hook.CircuitOpen && hook.Outcome != "circuit_open" {
					out.Hooks.CircuitOpened++
				}
				if hook.Outcome == "circuit_open" {
					out.Hooks.CircuitOpenSkips++
				}
			}
		case EventRetention:
			if retention := event.Retention; retention != nil {
				out.Retention.Available = true
				out.Retention.Epochs++
				out.Retention.BlocksTrimmed += retention.BlocksTrimmed
				out.Retention.BytesRemoved += retention.BytesRemoved
				out.Retention.EstimatedTokensRemoved += retention.EstimatedTokensRemoved
				if retention.ResponseStateReset {
					out.Retention.ResponseStateResets++
				}
				if retention.ContinuationStateReset {
					out.Retention.ContinuationStateResets++
				}
				if retention.MeasurementAnchorReset {
					out.Retention.MeasurementAnchorResets++
				}
				if retention.NextRequestStateful {
					out.Retention.NextRequestStateful++
				} else {
					out.Retention.NextRequestFull++
				}
				out.Invariants.RetentionAvailable = true
				if negativeRetentionContext(retention) {
					out.Invariants.NegativeContextViolations++
				}
				if inconsistentRetention(retention) {
					out.Invariants.InconsistentRetentionViolations++
				}
			}
		}
	}
	if len(events) == 0 && child != nil && (child.TelemetryVersion > 0 || child.ClosureTrigger != "" || child.TurnBudgetExhausted || child.WorkflowStatus != nil) {
		out.Closure.Available = true
		closurePrompts[0] = struct{}{}
		if child.ClosureTrigger != "" {
			closures[0] = normalizedClosureTrigger(child.ClosureTrigger)
		}
		if child.TurnBudgetExhausted {
			out.Closure.TurnBudgetExhausted = 1
		}
		if child.TelemetryVersion > 0 || child.WorkflowStatus != nil {
			out.Workflow.Available = true
			workflow[0] = child.WorkflowStatus
		}
	}
	for _, status := range workflow {
		out.Workflow.Prompts++
		if status == nil {
			out.Workflow.Unsupplied++
			continue
		}
		out.Workflow.Supplied++
		out.Workflow.Outcomes[normalizedWorkflowOutcome(status.Outcome)]++
		if status.RemainingRequirements != nil && *status.RemainingRequirements >= 0 {
			out.Workflow.RemainingRequirementsTotal += *status.RemainingRequirements
			out.Workflow.remainingRequirementValues = append(out.Workflow.remainingRequirementValues, *status.RemainingRequirements)
		}
	}
	out.Workflow.RemainingRequirements = intDistribution(out.Workflow.remainingRequirementValues)
	out.Closure.Prompts = len(closurePrompts)
	for _, trigger := range closures {
		out.Closure.Triggers[trigger]++
	}
	if schemaAvailable {
		out.Hooks.Available = true
	}
	out.Progress = deriveProgress(progress, completed, schemaAvailable)
	out.Completion = deriveCompletion(child, child != nil)
	out.Trajectory = deriveTrajectory(events)
	return out
}

const (
	maxAnalysisMutationPaths     = 32
	maxAnalysisMutationPathBytes = 512
)

type mutationAnalysisAccumulator struct {
	active        map[string]struct{}
	confirmed     map[string]struct{}
	observations  int
	confirmations int
	invalid       int
}

func newMutationAnalysisAccumulator() mutationAnalysisAccumulator {
	return mutationAnalysisAccumulator{
		active:    make(map[string]struct{}, maxAnalysisMutationPaths),
		confirmed: make(map[string]struct{}, maxAnalysisMutationPaths),
	}
}

func (a *mutationAnalysisAccumulator) observe(paths []string, confirmation bool) {
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || len(path) > maxAnalysisMutationPathBytes || strings.IndexFunc(path, unicode.IsControl) >= 0 {
			a.invalid++
			continue
		}
		if confirmation {
			a.confirmations++
		} else {
			a.observations++
		}
		if _, ok := a.active[path]; !ok {
			if len(a.active) >= maxAnalysisMutationPaths {
				continue
			}
			a.active[path] = struct{}{}
		}
		if confirmation && len(a.confirmed) < maxAnalysisMutationPaths {
			a.confirmed[path] = struct{}{}
		}
	}
}

func (a *mutationAnalysisAccumulator) reset() {
	clear(a.active)
	clear(a.confirmed)
}

func deriveTrajectory(events []Event) TrajectoryAnalysis {
	var out TrajectoryAnalysis
	mutations := newMutationAnalysisAccumulator()
	for _, event := range events {
		switch event.Type {
		case EventEvaluatorResult, EventStagnationNudge:
			out.Available = true
			out.Transitions++
		case EventToolMutation:
			out.Available = true
			out.Transitions++
			if event.ToolMutation != nil {
				mutations.observe(event.ToolMutation.Paths, false)
			}
		case EventToolDiff:
			out.Available = true
			out.Transitions++
			mutations.observe([]string{event.Path}, true)
		case EventBranch:
			out.Available = true
			out.Transitions++
			mutations.reset()
		}
	}
	if !out.Available {
		return out
	}
	state := ReconstructTrajectory(events)
	out.Streams = 1
	out.Schema = state.Schema
	out.BranchResets = state.BranchResets
	out.Evaluations = state.TotalEvaluations
	out.AcceptedEvaluations = state.AcceptedEvaluations
	out.RejectedEvaluations = state.RejectedEvaluations
	out.ActiveNoImprovementStreak = state.NoImprovementStreak
	out.MaxNoImprovementStreak = state.MaxNoImprovementStreak
	out.StagnationBaselines = state.StagnationBaselines
	out.StagnationImprovements = state.StagnationImprovements
	out.StagnationPlateaus = state.StagnationPlateaus
	out.StagnationRegressions = state.StagnationRegressions
	out.StagnationIndeterminate = state.StagnationIndeterminate
	out.UnorderedScoreEvaluations = state.UnorderedScoreEvaluations
	out.StagnationLaneResets = state.StagnationLaneResets
	out.StagnationNudges = state.StagnationNudges
	out.MutationPathObservations = mutations.observations
	out.DiffPathConfirmations = mutations.confirmations
	out.ActiveModifiedPaths = len(mutations.active)
	out.ActiveConfirmedPaths = len(mutations.confirmed)
	out.UnconfirmedMutationPaths = len(mutations.active) - len(mutations.confirmed)
	out.InvalidMutationPaths = mutations.invalid
	return out
}

func deriveCompletion(child *ChildMeta, applicable bool) CompletionAnalysis {
	out := CompletionAnalysis{
		Applicable: applicable,
		Outcomes:   make(map[string]int),
		Validation: make(map[string]int),
		Contracts:  make(map[string]int),
	}
	if !applicable {
		return out
	}

	contract := ChildCompletionContractGeneral
	if child != nil {
		switch {
		case child.Mode == ChildCompletionContractImplementation:
			contract = ChildCompletionContractImplementation
		case strings.EqualFold(strings.TrimSpace(child.Agent), "review"):
			contract = ChildCompletionContractReview
		}
	}
	report := ChildCompletionReport{
		Outcome: ChildCompletionOutcomeUnknown, Contract: contract,
		Source: ChildCompletionSourceCompatibility, ValidationStatus: ChildCompletionValidationMissing,
	}
	persisted := child != nil && child.Completion != nil
	if persisted {
		report = *child.Completion
	}
	switch report.ValidationStatus {
	case ChildCompletionValidationValid,
		ChildCompletionValidationMissing,
		ChildCompletionValidationMalformed,
		ChildCompletionValidationInvalid,
		ChildCompletionValidationOversized,
		ChildCompletionValidationDuplicate,
		ChildCompletionValidationUnavailable:
	default:
		report.ValidationStatus = ChildCompletionValidationInvalid
	}
	if !validCompletionProvenance(report.Source, report.ValidationStatus) ||
		persisted && !validCompletionLifecycle(child, report.ValidationStatus) {
		report.ValidationStatus = ChildCompletionValidationInvalid
	}
	if report.ValidationStatus == ChildCompletionValidationValid {
		if status := validateChildCompletionReport(report, contract); status != ChildCompletionValidationValid {
			report.ValidationStatus = status
		}
	}
	if report.ValidationStatus != ChildCompletionValidationValid {
		report.Outcome = ChildCompletionOutcomeUnknown
	}
	out.Outcomes[report.Outcome]++
	out.Validation[report.ValidationStatus]++
	out.Contracts[contract]++
	if report.ValidationStatus != ChildCompletionValidationValid {
		out.Unavailable++
		return out
	}

	out.Reports++
	return out
}

func validCompletionLifecycle(child *ChildMeta, validation string) bool {
	if child == nil {
		return false
	}
	switch validation {
	case ChildCompletionValidationValid,
		ChildCompletionValidationMissing,
		ChildCompletionValidationMalformed,
		ChildCompletionValidationInvalid,
		ChildCompletionValidationOversized,
		ChildCompletionValidationDuplicate:
		return child.Status == ChildStatusCompleted && child.TerminationReason != "cancelled" && child.TerminationReason != "error"
	case ChildCompletionValidationUnavailable:
		switch child.Status {
		case ChildStatusFailed:
			return child.TerminationReason != "" && child.TerminationReason != "cancelled"
		case ChildStatusCanceled:
			return child.TerminationReason == "cancelled"
		case ChildStatusAbandoned:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func validCompletionProvenance(source, validation string) bool {
	switch validation {
	case ChildCompletionValidationValid:
		return source == ChildCompletionSourceDeclared
	case ChildCompletionValidationMissing,
		ChildCompletionValidationMalformed,
		ChildCompletionValidationInvalid,
		ChildCompletionValidationOversized,
		ChildCompletionValidationDuplicate:
		return source == ChildCompletionSourceCompatibility
	case ChildCompletionValidationUnavailable:
		return source == ChildCompletionSourceHost
	default:
		return false
	}
}

func deriveProgress(events []Event, completed map[int]bool, schemaAvailable bool) ProgressAnalysis {
	out := ProgressAnalysis{Available: schemaAvailable}
	if len(events) == 0 {
		if schemaAvailable {
			out.MaxInspectionNoProgressStreak.Available = true
			out.TurnsToFirstMutation.Available = true
			out.TurnsToFirstVerification.Available = true
		}
		return out
	}
	out.Available = true
	out.ToolTurns = len(events)
	out.MaxInspectionNoProgressStreak = AnalysisValue{Available: true, Observed: true}
	out.TurnsToFirstMutation.Available = true
	out.TurnsToFirstVerification.Available = true
	for i, event := range events {
		progress := event.TurnProgress
		out.MaxInspectionNoProgressStreak.Value = max(out.MaxInspectionNoProgressStreak.Value, progress.InspectionNoProgressRun)
		turn := event.Turn
		if turn <= 0 {
			turn = i + 1
		}
		if progress.SuccessfulMutation && !out.TurnsToFirstMutation.Observed {
			out.TurnsToFirstMutation.Observed, out.TurnsToFirstMutation.Value = true, turn
		}
		if progress.SuccessfulVerification && !out.TurnsToFirstVerification.Observed {
			out.TurnsToFirstVerification.Observed, out.TurnsToFirstVerification.Value = true, turn
		}
		if progress.SteerReason != "batching" {
			continue
		}
		out.BatchingSteers++
		following := 0
		compliant := false
		for j := i + 1; j < len(events) && following < 2; j++ {
			if events[j].Prompt != event.Prompt {
				continue
			}
			following++
			if events[j].TurnProgress.BatchedOperationCount > 0 {
				compliant = true
				break
			}
		}
		switch {
		case compliant:
			out.BatchingCompliant++
		case following == 2 || completed[event.Prompt]:
			out.BatchingNoncompliant++
		default:
			out.BatchingPending++
		}
	}
	return out
}

func normalizedLabel(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "unknown"
}

func negativeCompatibleContextArithmetic(context *ContextSnapshot) bool {
	switch normalizedProviderCountScope(context.ProviderInputScope) {
	case string(llm.InputTokenCountScopeRequestPayload):
		return context.ProviderInputTokens > context.PayloadTotal && context.PayloadTotal >= 0
	case string(llm.InputTokenCountScopeEffectiveContext):
		return context.ProviderInputTokens > context.Total && context.Total >= 0
	default:
		return false
	}
}

func normalizedProviderCountScope(value string) string {
	switch strings.TrimSpace(value) {
	case string(llm.InputTokenCountScopeEffectiveContext):
		return string(llm.InputTokenCountScopeEffectiveContext)
	case string(llm.InputTokenCountScopeRequestPayload):
		return string(llm.InputTokenCountScopeRequestPayload)
	default:
		return string(llm.InputTokenCountScopeUnknown)
	}
}

func normalizedClosureTrigger(value string) string {
	switch value = strings.TrimSpace(value); value {
	case "turn_budget", "stagnation", "repeat_guard", "error_guard":
		return value
	default:
		return "unknown"
	}
}

func normalizedWorkflowOutcome(value string) string {
	switch value = strings.TrimSpace(value); value {
	case "complete", "waiting", "blocked", "escalated", "in_progress", "unknown":
		return value
	default:
		return "unknown"
	}
}

func negativeContext(c *ContextSnapshot) bool {
	return c.Total < 0 || c.Window < 0 || c.System < 0 || c.Tools < 0 || c.Messages < 0 ||
		c.PayloadTotal < 0 || c.PayloadSystem < 0 || c.PayloadTools < 0 || c.PayloadMessages < 0 || c.ProviderInputTokens < 0
}

func negativeRetentionContext(r *RetentionSnapshot) bool {
	return r.ContextTokensBefore < 0 || r.ContextTokensAfter < 0 || r.DecisionContextTokens < 0 ||
		r.LocalEstimateTokensBefore < 0 || r.LocalEstimateTokensAfter < 0 || r.EstimatedTokensRemoved < 0
}

func inconsistentRetention(r *RetentionSnapshot) bool {
	if r.BlocksTrimmed < 0 || r.BytesBefore < 0 || r.BytesAfter < 0 || r.BytesRemoved < 0 {
		return true
	}
	if r.BytesAfter > r.BytesBefore || r.LocalEstimateTokensAfter > r.LocalEstimateTokensBefore {
		return true
	}
	if r.BytesRemoved != 0 && r.BytesRemoved != r.BytesBefore-r.BytesAfter {
		return true
	}
	return r.EstimatedTokensRemoved != 0 && r.EstimatedTokensRemoved != r.LocalEstimateTokensBefore-r.LocalEstimateTokensAfter
}

func (w *WorkflowAnalysis) add(other WorkflowAnalysis) {
	if w.Outcomes == nil {
		w.Outcomes = make(map[string]int)
	}
	for key, value := range other.Outcomes {
		w.Outcomes[key] += value
	}
	w.Available = w.Available || other.Available
	w.Prompts += other.Prompts
	w.Supplied += other.Supplied
	w.Unsupplied += other.Unsupplied
	w.RemainingRequirementsTotal += other.RemainingRequirementsTotal
	w.remainingRequirementValues = append(w.remainingRequirementValues, other.remainingRequirementValues...)
	w.RemainingRequirements = intDistribution(w.remainingRequirementValues)
	w.CompletionSourceAvailable = w.CompletionSourceAvailable || other.CompletionSourceAvailable
	w.CompletionSourceReports += other.CompletionSourceReports
	w.CompletionSourceUnavailable += other.CompletionSourceUnavailable
}

func (c *CompletionAnalysis) add(other CompletionAnalysis) {
	if c.Outcomes == nil {
		c.Outcomes = make(map[string]int)
	}
	if c.Validation == nil {
		c.Validation = make(map[string]int)
	}
	if c.Contracts == nil {
		c.Contracts = make(map[string]int)
	}
	c.Applicable = c.Applicable || other.Applicable
	c.Reports += other.Reports
	c.Unavailable += other.Unavailable
	for key, value := range other.Outcomes {
		c.Outcomes[key] += value
	}
	for key, value := range other.Validation {
		c.Validation[key] += value
	}
	for key, value := range other.Contracts {
		c.Contracts[key] += value
	}
	c.ParentReworkAvailable = c.ParentReworkAvailable || other.ParentReworkAvailable
	c.ParentReworkObserved += other.ParentReworkObserved
}

func (t *TelemetryAnalysis) add(other TelemetryAnalysis) {
	if t.Closure.Triggers == nil {
		t.Closure.Triggers = make(map[string]int)
	}
	for key, value := range other.Closure.Triggers {
		t.Closure.Triggers[key] += value
	}
	t.Closure.Available = t.Closure.Available || other.Closure.Available
	t.Closure.Prompts += other.Closure.Prompts
	t.Closure.TurnBudgetExhausted += other.Closure.TurnBudgetExhausted
	if t.Workflow.Outcomes == nil {
		t.Workflow.Outcomes = make(map[string]int)
	}
	for key, value := range other.Workflow.Outcomes {
		t.Workflow.Outcomes[key] += value
	}
	t.Workflow.Available = t.Workflow.Available || other.Workflow.Available
	t.Workflow.Prompts += other.Workflow.Prompts
	t.Workflow.Supplied += other.Workflow.Supplied
	t.Workflow.Unsupplied += other.Workflow.Unsupplied
	t.Workflow.RemainingRequirementsTotal += other.Workflow.RemainingRequirementsTotal
	t.Workflow.remainingRequirementValues = append(t.Workflow.remainingRequirementValues, other.Workflow.remainingRequirementValues...)
	t.Workflow.RemainingRequirements = intDistribution(t.Workflow.remainingRequirementValues)
	t.Workflow.CompletionSourceAvailable = t.Workflow.CompletionSourceAvailable || other.Workflow.CompletionSourceAvailable
	t.Workflow.CompletionSourceReports += other.Workflow.CompletionSourceReports
	t.Workflow.CompletionSourceUnavailable += other.Workflow.CompletionSourceUnavailable
	t.Completion.add(other.Completion)
	t.Progress.Available = t.Progress.Available || other.Progress.Available
	t.Progress.ToolTurns += other.Progress.ToolTurns
	mergeMaximum(&t.Progress.MaxInspectionNoProgressStreak, other.Progress.MaxInspectionNoProgressStreak)
	mergeMinimum(&t.Progress.TurnsToFirstMutation, other.Progress.TurnsToFirstMutation)
	mergeMinimum(&t.Progress.TurnsToFirstVerification, other.Progress.TurnsToFirstVerification)
	t.Progress.BatchingSteers += other.Progress.BatchingSteers
	t.Progress.BatchingCompliant += other.Progress.BatchingCompliant
	t.Progress.BatchingNoncompliant += other.Progress.BatchingNoncompliant
	t.Progress.BatchingPending += other.Progress.BatchingPending
	t.Hooks.Available = t.Hooks.Available || other.Hooks.Available
	t.Hooks.Diagnostics += other.Hooks.Diagnostics
	t.Hooks.Timeouts += other.Hooks.Timeouts
	t.Hooks.CircuitOpened += other.Hooks.CircuitOpened
	t.Hooks.CircuitOpenSkips += other.Hooks.CircuitOpenSkips
	t.Context.Available = t.Context.Available || other.Context.Available
	t.Context.Samples += other.Context.Samples
	t.Context.MaxTotalTokens = max(t.Context.MaxTotalTokens, other.Context.MaxTotalTokens)
	t.Context.MaxPayloadTokens = max(t.Context.MaxPayloadTokens, other.Context.MaxPayloadTokens)
	t.Context.MaxProviderInputTokens = max(t.Context.MaxProviderInputTokens, other.Context.MaxProviderInputTokens)
	if t.Context.ProviderCountScopes == nil {
		t.Context.ProviderCountScopes = make(map[string]int)
	}
	if t.Context.ProviderMaxByScope == nil {
		t.Context.ProviderMaxByScope = make(map[string]int)
	}
	for scope, count := range other.Context.ProviderCountScopes {
		t.Context.ProviderCountScopes[scope] += count
	}
	for scope, value := range other.Context.ProviderMaxByScope {
		t.Context.ProviderMaxByScope[scope] = max(t.Context.ProviderMaxByScope[scope], value)
	}
	t.Retention.Available = t.Retention.Available || other.Retention.Available
	t.Retention.Epochs += other.Retention.Epochs
	t.Retention.BlocksTrimmed += other.Retention.BlocksTrimmed
	t.Retention.BytesRemoved += other.Retention.BytesRemoved
	t.Retention.EstimatedTokensRemoved += other.Retention.EstimatedTokensRemoved
	t.Retention.ResponseStateResets += other.Retention.ResponseStateResets
	t.Retention.ContinuationStateResets += other.Retention.ContinuationStateResets
	t.Retention.MeasurementAnchorResets += other.Retention.MeasurementAnchorResets
	t.Retention.NextRequestStateful += other.Retention.NextRequestStateful
	t.Retention.NextRequestFull += other.Retention.NextRequestFull
	t.Trajectory.add(other.Trajectory)
	t.Invariants.ContextAvailable = t.Invariants.ContextAvailable || other.Invariants.ContextAvailable
	t.Invariants.RetentionAvailable = t.Invariants.RetentionAvailable || other.Invariants.RetentionAvailable
	t.Invariants.UsageReconciliationAvailable = t.Invariants.UsageReconciliationAvailable || other.Invariants.UsageReconciliationAvailable
	t.Invariants.NegativeContextViolations += other.Invariants.NegativeContextViolations
	t.Invariants.InconsistentRetentionViolations += other.Invariants.InconsistentRetentionViolations
	t.Invariants.UsageReconciliationViolations += other.Invariants.UsageReconciliationViolations
}

func (t *TrajectoryAnalysis) add(other TrajectoryAnalysis) {
	if !other.Available {
		return
	}
	t.Available = true
	t.Streams += other.Streams
	t.Schema = max(t.Schema, other.Schema)
	t.Transitions += other.Transitions
	t.BranchResets += other.BranchResets
	t.Evaluations += other.Evaluations
	t.AcceptedEvaluations += other.AcceptedEvaluations
	t.RejectedEvaluations += other.RejectedEvaluations
	t.ActiveNoImprovementStreak += other.ActiveNoImprovementStreak
	t.MaxNoImprovementStreak = max(t.MaxNoImprovementStreak, other.MaxNoImprovementStreak)
	t.StagnationBaselines += other.StagnationBaselines
	t.StagnationImprovements += other.StagnationImprovements
	t.StagnationPlateaus += other.StagnationPlateaus
	t.StagnationRegressions += other.StagnationRegressions
	t.StagnationIndeterminate += other.StagnationIndeterminate
	t.UnorderedScoreEvaluations += other.UnorderedScoreEvaluations
	t.StagnationLaneResets += other.StagnationLaneResets
	t.StagnationNudges += other.StagnationNudges
	t.MutationPathObservations += other.MutationPathObservations
	t.DiffPathConfirmations += other.DiffPathConfirmations
	t.ActiveModifiedPaths += other.ActiveModifiedPaths
	t.ActiveConfirmedPaths += other.ActiveConfirmedPaths
	t.UnconfirmedMutationPaths += other.UnconfirmedMutationPaths
	t.InvalidMutationPaths += other.InvalidMutationPaths
}

func mergeMaximum(dst *AnalysisValue, src AnalysisValue) {
	if !src.Available {
		return
	}
	if !dst.Available || src.Value > dst.Value {
		dst.Value = src.Value
	}
	dst.Available, dst.Observed = true, dst.Observed || src.Observed
}

func mergeMinimum(dst *AnalysisValue, src AnalysisValue) {
	if !src.Available {
		return
	}
	dst.Available = true
	if src.Observed && (!dst.Observed || src.Value < dst.Value) {
		dst.Observed, dst.Value = true, src.Value
	}
}

// WriteAnalysisJSON writes deterministic, versioned JSON with no transcript,
// tool input, result body, hook payload, or assistant text fields.
func WriteAnalysisJSON(report AnalysisReport, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("session: write analysis json: %w", err)
	}
	return nil
}

// WriteAnalysisText writes a bounded human-readable corpus summary.
func WriteAnalysisText(report AnalysisReport, w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Session analysis v%d\n", report.Version)
	fmt.Fprintf(&b, "  path: %s\n", report.Path)
	fmt.Fprintf(&b, "  roots/sessions: %d / %d\n", report.Roots, report.Sessions)
	fmt.Fprintf(&b, "  streams missing/incomplete/malformed/symlink/limit: %d / %d / %d / %d / %d\n", report.MissingStreams, report.IncompleteStreams, report.MalformedStreams, report.SymlinkStreams, report.LimitExceededStreams)
	fmt.Fprintf(&b, "  malformed child metadata: %d\n", report.MalformedChildMetadata)
	fmt.Fprintf(&b, "  completeness: %s\n", formatCountMap(true, report.Completeness))
	fmt.Fprintf(&b, "  prompts completed/observed: %d / %d\n", report.Execution.CompletedPrompts, report.Execution.Prompts)
	fmt.Fprintf(&b, "  turns/tools/results/errors: %d / %d / %d / %d tool, %d model\n", report.Execution.Turns, report.Execution.ToolCalls, report.Execution.ToolResults, report.Execution.ToolErrors, report.Execution.ModelErrors)
	fmt.Fprintf(&b, "  command results/failures/cancellations; effective failures: %d / %d / %d; %d\n", report.Execution.CommandResults, report.Execution.CommandFailures, report.Execution.CommandCancellations, report.Execution.EffectiveFailures)
	fmt.Fprintf(&b, "  terminations: %s\n", formatCountMap(report.Execution.TerminationAvailable, report.Execution.Terminations))
	fmt.Fprintf(&b, "  telemetry coverage closure/workflow/progress/hooks/context/count-scope/retention/trajectory: %d/%d/%d/%d/%d/%d/%d/%d of %d\n", report.Coverage.Closure, report.Coverage.Workflow, report.Coverage.Progress, report.Coverage.Hooks, report.Coverage.Context, report.Coverage.ProviderCountScope, report.Coverage.Retention, report.Coverage.Trajectory, report.Coverage.Sessions)
	fmt.Fprintf(&b, "  delegate completion valid/failures/applicable: %d / %d / %d\n", report.Coverage.CompletionValid, report.Coverage.CompletionCoverageFailures, report.Coverage.CompletionApplicable)
	writeTelemetryText(&b, "  ", report.Telemetry)
	writeAnalysisUsageText(&b, "  ", report.Usage, report.Distributions)
	fmt.Fprintf(&b, "  storage bytes total/state/tree/raw/compactions/tool-results: %d / %d / %d / %d / %d / %d\n", report.Storage.TotalBytes, report.Storage.State.Bytes, report.Storage.Tree.Bytes, report.Storage.Raw.Bytes, report.Storage.Compactions.Bytes, report.Storage.ToolResults.Bytes)
	fmt.Fprintf(&b, "  context resets snapshot/legacy-delta; payload bytes: %d / %d; %d / %d\n", report.Storage.SnapshotResetEntries, report.Storage.DeltaResetEntries, report.Storage.SnapshotPayloadBytes, report.Storage.DeltaPayloadBytes)
	fmt.Fprintf(&b, "  cohorts: %d\n", len(report.Cohorts))
	limit := min(len(report.Items), maxAnalysisTextSessions)
	fmt.Fprintf(&b, "Streams (showing %d of %d)\n", limit, len(report.Items))
	for _, item := range report.Items[:limit] {
		fmt.Fprintf(&b, "  %s: %s, %s, %d turns, %d tools, %d effective errors, %d events\n", item.Path, item.Source.Status, item.Execution.Completeness, item.Execution.Turns, item.Execution.ToolCalls, item.Execution.EffectiveFailures+item.Execution.ModelErrors, item.Source.Events)
	}
	if omitted := len(report.Items) - limit; omitted > 0 {
		fmt.Fprintf(&b, "  ... %d more streams omitted\n", omitted)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("session: write analysis text: %w", err)
	}
	return nil
}

func writeTelemetryText(w io.Writer, indent string, t TelemetryAnalysis) {
	fmt.Fprintf(w, "%sclosure triggers: %s\n", indent, formatCountMap(t.Closure.Available, t.Closure.Triggers))
	fmt.Fprintf(w, "%sturn budgets exhausted: %s of %s covered prompts\n", indent, availableCount(t.Closure.Available, t.Closure.TurnBudgetExhausted), availableCount(t.Closure.Available, t.Closure.Prompts))
	if t.Workflow.Available {
		fmt.Fprintf(w, "%sworkflow supplied/unsupplied: %d / %d; outcomes: %s; unresolved total/median/p90: %d / %d / %d (%d samples)\n", indent, t.Workflow.Supplied, t.Workflow.Unsupplied, formatCountMap(true, t.Workflow.Outcomes), t.Workflow.RemainingRequirementsTotal, t.Workflow.RemainingRequirements.Median, t.Workflow.RemainingRequirements.P90, t.Workflow.RemainingRequirements.Samples)
	} else {
		fmt.Fprintf(w, "%sworkflow outcomes: unavailable\n", indent)
	}
	if t.Completion.Applicable {
		fmt.Fprintf(w, "%sdelegate completion reports/unavailable: %d / %d; outcomes: %s; validation: %s; contracts: %s\n", indent, t.Completion.Reports, t.Completion.Unavailable, formatCountMap(true, t.Completion.Outcomes), formatCountMap(true, t.Completion.Validation), formatCountMap(true, t.Completion.Contracts))
		fmt.Fprintf(w, "%sparent rework observed: %s\n", indent, availableCount(t.Completion.ParentReworkAvailable, t.Completion.ParentReworkObserved))
	} else {
		fmt.Fprintf(w, "%sdelegate completion: not applicable\n", indent)
	}
	if t.Progress.Available {
		fmt.Fprintf(w, "%sprogress telemetry: available (%d tool turns)\n", indent, t.Progress.ToolTurns)
		fmt.Fprintf(w, "%smax inspection/no-progress streak: %d\n", indent, t.Progress.MaxInspectionNoProgressStreak.Value)
		fmt.Fprintf(w, "%sturns to first successful mutation/verification: %s / %s\n", indent, formatAnalysisValue(t.Progress.TurnsToFirstMutation), formatAnalysisValue(t.Progress.TurnsToFirstVerification))
		fmt.Fprintf(w, "%sbatching steers compliant/noncompliant/pending: %d / %d / %d / %d total\n", indent, t.Progress.BatchingCompliant, t.Progress.BatchingNoncompliant, t.Progress.BatchingPending, t.Progress.BatchingSteers)
	} else {
		fmt.Fprintf(w, "%sprogress telemetry: unavailable\n", indent)
	}
	if t.Hooks.Available {
		fmt.Fprintf(w, "%shook timeouts/circuit-openings/circuit-skips: %d / %d / %d\n", indent, t.Hooks.Timeouts, t.Hooks.CircuitOpened, t.Hooks.CircuitOpenSkips)
	} else {
		fmt.Fprintf(w, "%shook diagnostics: unavailable\n", indent)
	}
	if t.Context.Available {
		fmt.Fprintf(w, "%scontext samples/max total/max payload/max provider: %d / %d / %d / %d; provider scopes: %s\n", indent, t.Context.Samples, t.Context.MaxTotalTokens, t.Context.MaxPayloadTokens, t.Context.MaxProviderInputTokens, formatCountMap(true, t.Context.ProviderCountScopes))
	} else {
		fmt.Fprintf(w, "%scontext telemetry: unavailable\n", indent)
	}
	if t.Retention.Available {
		fmt.Fprintf(w, "%sretention epochs/blocks/bytes/tokens removed: %d / %d / %d / %d; continuation resets: %d\n", indent, t.Retention.Epochs, t.Retention.BlocksTrimmed, t.Retention.BytesRemoved, t.Retention.EstimatedTokensRemoved, t.Retention.ContinuationStateResets)
	} else {
		fmt.Fprintf(w, "%sretention telemetry: unavailable\n", indent)
	}
	if t.Trajectory.Available {
		fmt.Fprintf(w, "%strajectory streams/transitions/evaluations/branch resets: %d / %d / %d / %d\n", indent, t.Trajectory.Streams, t.Trajectory.Transitions, t.Trajectory.Evaluations, t.Trajectory.BranchResets)
		fmt.Fprintf(w, "%strajectory active mutation/confirmed paths: %d / %d\n", indent, t.Trajectory.ActiveModifiedPaths, t.Trajectory.ActiveConfirmedPaths)
		fmt.Fprintf(w, "%strajectory no-improvement active/max; baseline/improved/plateau/regressed/indeterminate: %d / %d; %d / %d / %d / %d / %d\n", indent, t.Trajectory.ActiveNoImprovementStreak, t.Trajectory.MaxNoImprovementStreak, t.Trajectory.StagnationBaselines, t.Trajectory.StagnationImprovements, t.Trajectory.StagnationPlateaus, t.Trajectory.StagnationRegressions, t.Trajectory.StagnationIndeterminate)
		fmt.Fprintf(w, "%strajectory unordered scores/lane resets/strategy resets: %d / %d / %d\n", indent, t.Trajectory.UnorderedScoreEvaluations, t.Trajectory.StagnationLaneResets, t.Trajectory.StagnationNudges)
		fmt.Fprintf(w, "%strajectory mutation observations/diff confirmations/unconfirmed/invalid paths: %d / %d / %d / %d\n", indent, t.Trajectory.MutationPathObservations, t.Trajectory.DiffPathConfirmations, t.Trajectory.UnconfirmedMutationPaths, t.Trajectory.InvalidMutationPaths)
	} else {
		fmt.Fprintf(w, "%strajectory telemetry: unavailable\n", indent)
	}
	fmt.Fprintf(w, "%snegative-context violations: %s\n", indent, availableCount(t.Invariants.ContextAvailable || t.Invariants.RetentionAvailable, t.Invariants.NegativeContextViolations))
	fmt.Fprintf(w, "%sinconsistent-retention violations: %s\n", indent, availableCount(t.Invariants.RetentionAvailable, t.Invariants.InconsistentRetentionViolations))
	fmt.Fprintf(w, "%susage-reconciliation violations: %s\n", indent, availableCount(t.Invariants.UsageReconciliationAvailable, t.Invariants.UsageReconciliationViolations))
}

func writeAnalysisUsageText(w io.Writer, indent string, usage UsageAnalysis, distributions UsageDistributions) {
	root := addUsageSlice(usage.RootConversational, usage.RootMaintenance)
	children := addUsageSlice(usage.DescendantConversational, usage.DescendantMaintenance)
	fmt.Fprintf(w, "%susage tokens root/descendant/inclusive: %s / %s / %s\n", indent, availableUsageTokens(root), availableUsageTokens(children), availableUsageTokens(usage.Inclusive))
	if usage.Inclusive.Available {
		fmt.Fprintf(w, "%susage calls priced/unpriced: %d / %d / %d; known cost: $%.6f (complete=%t)\n", indent, usage.Inclusive.ModelCalls, usage.Inclusive.PricedCalls, usage.Inclusive.UnpricedCalls, usage.Inclusive.KnownCostUSD, usage.Inclusive.CostComplete)
	} else {
		fmt.Fprintf(w, "%susage calls/cost: unavailable\n", indent)
	}
	fmt.Fprintf(w, "%shierarchy token median/p90 inclusive/root/descendant: %d/%d / %d/%d / %d/%d (%d samples)\n", indent, distributions.InclusiveTokens.Median, distributions.InclusiveTokens.P90, distributions.RootTokens.Median, distributions.RootTokens.P90, distributions.DescendantTokens.Median, distributions.DescendantTokens.P90, distributions.InclusiveTokens.Samples)
	fmt.Fprintf(w, "%shierarchy known-complete cost median/p90: $%.6f / $%.6f (%d samples)\n", indent, distributions.InclusiveKnownCostUSD.Median, distributions.InclusiveKnownCostUSD.P90, distributions.InclusiveKnownCostUSD.Samples)
}

func availableUsageTokens(usage UsageSlice) string {
	if !usage.Available {
		return "unavailable"
	}
	return fmt.Sprint(usage.TotalTokens)
}

func availableCount(available bool, value int) string {
	if !available {
		return "unavailable"
	}
	return fmt.Sprint(value)
}

func formatAnalysisValue(value AnalysisValue) string {
	if !value.Available {
		return "unavailable"
	}
	if !value.Observed {
		return "not observed"
	}
	return fmt.Sprint(value.Value)
}

func formatCountMap(available bool, values map[string]int) string {
	if !available {
		return "unavailable"
	}
	if len(values) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	return strings.Join(parts, ", ")
}
