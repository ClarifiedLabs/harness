package session

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"harness/internal/llm"
)

// Stats prints a deterministic, human-readable statistics report for one saved
// session. The session's persisted usage is authoritative and already includes
// delegate and compaction usage.
func Stats(dir string, w io.Writer) error {
	report, err := collectStats(dir)
	if err != nil {
		return err
	}
	if err := writeStats(report, w); err != nil {
		return fmt.Errorf("session: write stats: %w", err)
	}
	return nil
}

// StatsJSON writes the machine-readable session statistics needed for tool
// accuracy comparisons without exposing transcript bodies or tool inputs.
func StatsJSON(dir string, w io.Writer) error {
	report, err := collectStats(dir)
	if err != nil {
		return err
	}
	analysis, err := AnalyzeCorpus(dir, AnalyzeOptions{})
	if err != nil {
		return fmt.Errorf("session: analyze stats json: %w", err)
	}
	type toolJSON struct {
		Calls       int     `json:"calls"`
		Results     int     `json:"results"`
		Errors      int     `json:"errors"`
		ErrorRate   float64 `json:"error_rate"`
		Truncations int     `json:"truncations"`
	}
	tools := make(map[string]toolJSON)
	names := make(map[string]bool)
	for name := range report.tools.byName {
		names[name] = true
	}
	for name := range report.tools.resultsByName {
		names[name] = true
	}
	for name := range names {
		result := report.tools.resultsByName[name]
		rate := 0.0
		if result.results > 0 {
			rate = float64(result.errors) / float64(result.results)
		}
		tools[name] = toolJSON{
			Calls: report.tools.byName[name], Results: result.results,
			Errors: result.errors, ErrorRate: rate, Truncations: result.truncations,
		}
	}
	payload := struct {
		Version   int                 `json:"version"`
		Path      string              `json:"path"`
		SessionID string              `json:"session_id"`
		Provider  string              `json:"provider"`
		Model     string              `json:"model"`
		Prompts   int                 `json:"prompts"`
		Turns     int                 `json:"turns"`
		Tools     map[string]toolJSON `json:"tools"`
		Errors    ErrorSummary        `json:"errors"`
		Telemetry TelemetryAnalysis   `json:"telemetry"`
		Build     BuildMetadata       `json:"build"`
		Runtime   RuntimeProfile      `json:"runtime"`
		Usage     UsageAnalysis       `json:"usage"`
		Storage   StorageAnalysis     `json:"storage"`
	}{
		Version: AnalysisVersion, Path: dir, SessionID: report.root.state.ID,
		Provider: report.root.state.Provider, Model: report.root.state.Model,
		Prompts: report.root.prompts, Turns: report.root.turns,
		Tools: tools, Errors: report.errors, Telemetry: report.telemetry,
		Build: report.root.state.Build, Runtime: report.root.state.Runtime,
		Usage: analysis.Usage, Storage: analysis.Storage,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("session: write stats json: %w", err)
	}
	return nil
}

type statsReport struct {
	path                string
	root                collectedSessionStats
	delegates           []*delegateStats
	tools               toolStats
	delegateTools       toolStats
	compactions         compactionStats
	delegateCompactions compactionStats
	statusCounts        map[string]int
	terminationCounts   map[string]int
	directUsage         llm.Usage
	delegateDirectUsage llm.Usage
	directModelCalls    int
	delegateModelCalls  int
	directMaintCalls    int
	delegateMaintCalls  int
	errors              ErrorSummary
	telemetry           TelemetryAnalysis
}

type collectedSessionStats struct {
	state             Session
	checkpointed      bool
	prompts           int
	turns             int
	modelCalls        int
	retries           int
	maintenanceCalls  int
	maintenanceUsage  llm.Usage
	directUsage       llm.Usage
	navigations       int
	terminationCounts map[string]int
	checkpoints       checkpointStats
	retention         retentionStats
	idleCompactions   idleCompactionStats
	tree              treeStats
	tools             toolStats
	compactions       compactionStats
	context           contextCompositionStats
	latestContext     *ContextSnapshot
	telemetry         TelemetryAnalysis
}

type treeStats struct {
	entries              int
	branches             int
	leaves               int
	maxDepth             int
	activeDepth          int
	contextResets        int
	snapshotResetEntries int
	deltaResetEntries    int
	snapshotResetBytes   int
	deltaResetBytes      int
}

type delegateStats struct {
	meta  ChildMeta
	stats collectedSessionStats
}

type toolStats struct {
	calls               int
	byName              map[string]int
	commands            commandStats
	parallel            parallelStats
	turns               int
	soloTodoTurns       int
	singleInspectTurns  int
	resultErrors        int
	resultTruncations   int
	resultOriginalBytes int
	resultShownBytes    int
	resultTotalMS       int64
	resultMaxMS         int64
	resultsByName       map[string]toolResultStats
	callShapes          map[string]map[string]int
	skillReads          int
	skillReadPaths      map[string]int
	skillActivations    map[string]int
}

type commandStats struct {
	calls        int
	foreground   int
	background   int
	shell        int
	argv         int
	stepBatches  int
	stepCommands int
	stepShell    int
	stepArgv     int
}

type toolResultStats struct {
	results       int
	errors        int
	truncations   int
	originalBytes int
	shownBytes    int
	totalMS       int64
	maxMS         int64
}

type contextCompositionStats struct {
	messages               int
	blocks                 int
	userTextBytes          int
	assistantTextBytes     int
	toolInputBytes         int
	toolResultBytes        int
	reasoningTextBytes     int
	reasoningOpaqueBytes   int
	interactionTextBytes   int
	interactionOpaqueBytes int
	imageEncodedBytes      int
	otherBytes             int
}

type parallelStats struct {
	batches int
	calls   int
	largest int
}

type compactionStats struct {
	runs                   int
	messageCount           int
	modelSummaries         int
	deterministicSummaries int
	unknownSummaries       int
	fallbackReasons        map[string]int
	usage                  llm.Usage
}

type checkpointStats struct {
	saves      int
	totalMS    int64
	maxMS      int64
	lagTurns   int
	lagSeconds float64
}

type retentionStats struct {
	epochs                  int
	pressureEpochs          int
	agePasses               int
	blocksTrimmed           int
	bytesTrimmed            int
	responseStateResets     int
	measurementAnchorResets int
	continuationResets      int
	statefulRequests        int
	fullContextRequests     int
	statelessRequests       int
	unknownRequests         int
}

type idleCompactionStats struct {
	attempts           int
	applied            int
	discarded          int
	failed             int
	noChange           int
	totalMS            int64
	maxMS              int64
	appliedBeforeTotal int
	appliedAfterTotal  int
}

func collectStats(dir string) (statsReport, error) {
	root, err := collectSessionStats(dir)
	if err != nil {
		return statsReport{}, fmt.Errorf("collect root session: %w", err)
	}
	delegates, err := collectDelegateStats(dir)
	if err != nil {
		return statsReport{}, err
	}

	report := statsReport{
		path:              dir,
		root:              root,
		delegates:         delegates,
		tools:             cloneToolStats(root.tools),
		compactions:       root.compactions,
		statusCounts:      make(map[string]int),
		terminationCounts: make(map[string]int),
		directUsage:       root.directUsage,
		directModelCalls:  root.modelCalls,
		directMaintCalls:  root.maintenanceCalls,
		telemetry:         root.telemetry,
	}
	for _, child := range delegates {
		report.tools.add(child.stats.tools)
		report.delegateTools.add(child.stats.tools)
		report.compactions.add(child.stats.compactions)
		report.delegateCompactions.add(child.stats.compactions)
		report.statusCounts[child.meta.Status]++
		if child.meta.TerminationReason != "" {
			report.terminationCounts[child.meta.TerminationReason]++
		}
		report.directUsage = addUsage(report.directUsage, child.stats.directUsage)
		report.delegateDirectUsage = addUsage(report.delegateDirectUsage, child.stats.directUsage)
		report.directModelCalls += child.stats.modelCalls
		report.delegateModelCalls += child.stats.modelCalls
		report.directMaintCalls += child.stats.maintenanceCalls
		report.delegateMaintCalls += child.stats.maintenanceCalls
		report.telemetry.add(child.stats.telemetry)
	}
	errorAnalysis, err := AnalyzeErrors(dir, ErrorFilter{}, time.Time{})
	if err != nil {
		return statsReport{}, err
	}
	report.errors = errorAnalysis.Summary
	return report, nil
}

func collectSessionStats(dir string) (collectedSessionStats, error) {
	return collectSessionStatsWithFallback(dir, nil)
}

func collectSessionStatsWithFallback(dir string, child *ChildMeta) (collectedSessionStats, error) {
	state, err := Load(dir)
	if err != nil {
		if child == nil || !errors.Is(err, os.ErrNotExist) {
			return collectedSessionStats{}, fmt.Errorf("load state in %s: %w", dir, err)
		}
		state = Session{
			ID:       child.ID,
			Provider: child.Provider,
			Model:    child.Model,
			Agent:    child.Agent,
			Created:  child.Created,
			Updated:  child.Updated,
			Usage: UsageTotals{
				Usage:   child.Usage,
				CostUSD: child.Usage.CostUSD,
			},
		}
	}
	checkpointed := err == nil
	events, err := readEvents(dir)
	if err != nil {
		if child == nil || !errors.Is(err, os.ErrNotExist) {
			return collectedSessionStats{}, fmt.Errorf("read replay in %s: %w", dir, err)
		}
		events = nil
	}
	tools, err := collectToolStats(events)
	if err != nil {
		return collectedSessionStats{}, fmt.Errorf("read tool activity in %s: %w", dir, err)
	}
	compactions, parallel, err := collectCompactionStats(dir, state.Tree)
	if err != nil {
		return collectedSessionStats{}, err
	}
	tools.parallel = parallel

	prompts := make(map[int]struct{})
	turns := make(map[[2]int]struct{})
	attemptedTurns := make(map[[2]int]struct{})
	modelCalls := 0
	maintenanceCalls := 0
	navigations := 0
	var terminationCounts map[string]int
	var maintenanceUsage llm.Usage
	var directUsage llm.Usage
	var latestContext *ContextSnapshot
	for _, ev := range events {
		if ev.Context != nil {
			snapshot := *ev.Context
			latestContext = &snapshot
		}
		switch ev.Type {
		case EventUser:
			if ev.Prompt > 0 {
				prompts[ev.Prompt] = struct{}{}
			}
		case EventTurnAttemptUsage:
			modelCalls++
			if ev.Usage != nil {
				directUsage = addUsage(directUsage, *ev.Usage)
			}
			if ev.Prompt > 0 && ev.Turn > 0 {
				attemptedTurns[[2]int{ev.Prompt, ev.Turn}] = struct{}{}
			}
		case EventTurnComplete:
			if ev.Prompt > 0 && ev.Turn > 0 {
				turns[[2]int{ev.Prompt, ev.Turn}] = struct{}{}
			}
		case EventMaintenanceUsage:
			maintenanceCalls++
			if ev.Usage != nil {
				maintenanceUsage = addUsage(maintenanceUsage, *ev.Usage)
				directUsage = addUsage(directUsage, *ev.Usage)
			}
		case EventBranch:
			navigations++
		case EventPromptUsage:
			// Host continuations such as REPL /continue deliberately have no
			// EventUser, but their fresh accounting IDs still represent prompts.
			if ev.Prompt > 0 {
				prompts[ev.Prompt] = struct{}{}
			}
			if ev.TerminationReason != "" {
				if terminationCounts == nil {
					terminationCounts = make(map[string]int)
				}
				terminationCounts[ev.TerminationReason]++
			}
		}
	}
	retries := modelCalls - len(attemptedTurns)
	if retries < 0 {
		retries = 0
	}
	return collectedSessionStats{
		state:             state,
		checkpointed:      checkpointed,
		prompts:           len(prompts),
		turns:             len(turns),
		modelCalls:        modelCalls,
		retries:           retries,
		maintenanceCalls:  maintenanceCalls,
		maintenanceUsage:  maintenanceUsage,
		directUsage:       directUsage,
		navigations:       navigations,
		terminationCounts: terminationCounts,
		checkpoints:       collectCheckpointStats(events),
		retention:         collectRetentionStats(events),
		idleCompactions:   collectIdleCompactionStats(events),
		tree:              collectTreeStats(state.Tree),
		tools:             tools,
		compactions:       compactions,
		context:           collectContextComposition(state.Messages),
		latestContext:     latestContext,
		telemetry:         deriveTelemetry(events, child),
	}, nil
}

func collectContextComposition(messages []llm.Message) contextCompositionStats {
	stats := contextCompositionStats{messages: len(messages)}
	for _, message := range messages {
		for _, block := range message.Content {
			stats.blocks++
			switch block.Kind {
			case llm.BlockText:
				if message.Role == llm.RoleUser {
					stats.userTextBytes += len(block.Text)
				} else {
					stats.assistantTextBytes += len(block.Text)
				}
			case llm.BlockImage:
				stats.imageEncodedBytes += len(block.ImageData)
			case llm.BlockToolUse:
				stats.toolInputBytes += len(block.ToolInput)
			case llm.BlockToolResult:
				stats.toolResultBytes += len(block.ResultText)
				for _, content := range block.ResultContent {
					if content.Kind == llm.BlockImage {
						stats.imageEncodedBytes += len(content.ImageData)
					}
				}
			case llm.BlockThinking:
				stats.reasoningTextBytes += len(block.Thinking)
				stats.reasoningOpaqueBytes += len(block.ThinkingSignature)
			case llm.BlockRedactedThinking:
				stats.reasoningOpaqueBytes += len(block.RedactedData)
			case llm.BlockReasoning:
				stats.reasoningOpaqueBytes += len(block.ReasoningID) + len(block.ReasoningEncrypted)
			case llm.BlockInteractionThought:
				stats.interactionTextBytes += len(block.InteractionThoughtSummary)
				stats.interactionOpaqueBytes += len(block.InteractionThoughtSignature)
			case llm.BlockInteractionStep:
				stats.otherBytes += len(block.InteractionStep)
			case llm.BlockProviderCompaction:
				for _, item := range block.ProviderCompaction {
					stats.otherBytes += len(item)
				}
			default:
				stats.otherBytes += len(block.Text) + len(block.ResultText) + len(block.ToolInput)
			}
		}
	}
	return stats
}

func collectRetentionStats(events []Event) retentionStats {
	var stats retentionStats
	for _, event := range events {
		if event.Type != EventRetention || event.Retention == nil {
			continue
		}
		retention := event.Retention
		stats.epochs++
		switch retention.Policy {
		case "pressure_epoch":
			stats.pressureEpochs++
		case "age", "auto_age":
			stats.agePasses++
		}
		stats.blocksTrimmed += retention.BlocksTrimmed
		removed := retention.BytesRemoved
		if removed == 0 {
			removed = max(retention.BytesBefore-retention.BytesAfter, 0)
		}
		stats.bytesTrimmed += removed
		if retention.ResponseStateReset {
			stats.responseStateResets++
		}
		if retention.MeasurementAnchorReset {
			stats.measurementAnchorResets++
		}
		if retention.ContinuationStateReset {
			stats.continuationResets++
		}
		switch retention.NextRequestMode {
		case "stateful_suffix":
			stats.statefulRequests++
		case "full":
			stats.fullContextRequests++
		case "stateless":
			stats.statelessRequests++
		default:
			if retention.NextRequestStateful {
				stats.statefulRequests++
			} else {
				stats.unknownRequests++
			}
		}
	}
	return stats
}

func collectIdleCompactionStats(events []Event) idleCompactionStats {
	var stats idleCompactionStats
	for _, event := range events {
		if event.Type != EventIdleCompaction || event.IdleCompaction == nil {
			continue
		}
		stats.attempts++
		stats.totalMS += event.DurationMS
		stats.maxMS = max(stats.maxMS, event.DurationMS)
		switch event.IdleCompaction.Outcome {
		case "applied":
			stats.applied++
			stats.appliedBeforeTotal += event.IdleCompaction.ContextTokensBefore
			stats.appliedAfterTotal += event.IdleCompaction.ContextTokensAfter
		case "discarded":
			stats.discarded++
		case "failed":
			stats.failed++
		case "no_change":
			stats.noChange++
		}
	}
	return stats
}

func collectCheckpointStats(events []Event) checkpointStats {
	var stats checkpointStats
	lastClosed := -1
	for i, event := range events {
		if event.Type != EventCheckpoint || event.Purpose != "closed_turn" {
			continue
		}
		stats.saves++
		stats.totalMS += event.DurationMS
		stats.maxMS = max(stats.maxMS, event.DurationMS)
		lastClosed = i
	}
	if stats.saves == 0 {
		return stats
	}
	var firstLag time.Time
	var latest time.Time
	for i := lastClosed + 1; i < len(events); i++ {
		event := events[i]
		if event.Time.After(latest) {
			latest = event.Time
		}
		if event.Type != EventTurnComplete {
			continue
		}
		stats.lagTurns++
		if firstLag.IsZero() || event.Time.Before(firstLag) {
			firstLag = event.Time
		}
	}
	if !firstLag.IsZero() && latest.After(firstLag) {
		stats.lagSeconds = latest.Sub(firstLag).Seconds()
	}
	return stats
}

func collectToolStats(events []Event) (toolStats, error) {
	stats := toolStats{
		byName:           make(map[string]int),
		resultsByName:    make(map[string]toolResultStats),
		callShapes:       make(map[string]map[string]int),
		skillReadPaths:   make(map[string]int),
		skillActivations: make(map[string]int),
	}
	turnCalls := make(map[[2]int][]string)
	for _, ev := range events {
		if ev.Type == EventSkillActivation {
			key := strings.Trim(strings.TrimSpace(ev.Purpose)+"/"+strings.TrimSpace(ev.Summary), "/")
			if key == "" {
				key = "unknown"
			}
			stats.skillActivations[key]++
			continue
		}
		if ev.Type == EventToolResult {
			stats.resultErrors += boolInt(ev.ResultError)
			stats.resultTruncations += boolInt(ev.ResultTruncated)
			stats.resultOriginalBytes += ev.ResultOriginalBytes
			stats.resultShownBytes += ev.ResultShownBytes
			stats.resultTotalMS += ev.DurationMS
			stats.resultMaxMS = max(stats.resultMaxMS, ev.DurationMS)
			if ev.Tool != "" {
				result := stats.resultsByName[ev.Tool]
				result.results++
				result.errors += boolInt(ev.ResultError)
				result.truncations += boolInt(ev.ResultTruncated)
				result.originalBytes += ev.ResultOriginalBytes
				result.shownBytes += ev.ResultShownBytes
				result.totalMS += ev.DurationMS
				result.maxMS = max(result.maxMS, ev.DurationMS)
				stats.resultsByName[ev.Tool] = result
			}
			continue
		}
		if ev.Type != EventToolStart {
			continue
		}
		stats.calls++
		stats.byName[ev.Tool]++
		shape := llm.NormalizedToolCallHash(ev.Input)
		if stats.callShapes[ev.Tool] == nil {
			stats.callShapes[ev.Tool] = make(map[string]int)
		}
		stats.callShapes[ev.Tool][shape]++
		if ev.Prompt > 0 && ev.Turn > 0 {
			key := [2]int{ev.Prompt, ev.Turn}
			turnCalls[key] = append(turnCalls[key], ev.Tool)
		}
		if ev.Tool == "read" {
			for _, pathHash := range skillReadPathHashes(ev.Input) {
				stats.skillReads++
				stats.skillReadPaths[pathHash]++
			}
		}
		if ev.Tool != "shell" {
			continue
		}
		var input struct {
			Command    string          `json:"command"`
			Argv       json.RawMessage `json:"argv"`
			Background bool            `json:"background"`
			Steps      []struct {
				Command string          `json:"command"`
				Argv    json.RawMessage `json:"argv"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(ev.Input, &input); err != nil {
			// Malformed shell is a model error, not a stats failure.
			continue
		}
		var argv []string
		if len(input.Argv) > 0 {
			if err := json.Unmarshal(input.Argv, &argv); err != nil {
				var s string
				if err2 := json.Unmarshal(input.Argv, &s); err2 == nil {
					_ = json.Unmarshal([]byte(s), &argv)
				}
			}
		}
		hasArgv := len(argv) > 0
		// Steps argv classification best-effort.
		hasSteps := len(input.Steps) > 0
		if hasSteps {
			// Reuse argv decode for steps loosely.
			_ = hasArgv // keep for branching below; steps block handles its own counts.
		}
		stats.commands.calls++
		if input.Background {
			stats.commands.background++
		} else {
			stats.commands.foreground++
		}
		if input.Command != "" {
			stats.commands.shell++
		} else if hasArgv {
			stats.commands.argv++
		}
		if len(input.Steps) > 0 {
			stats.commands.stepBatches++
			stats.commands.stepCommands += len(input.Steps)
			for _, step := range input.Steps {
				if step.Command != "" {
					stats.commands.stepShell++
				} else {
					var sArgv []string
					if len(step.Argv) > 0 {
						if err := json.Unmarshal(step.Argv, &sArgv); err != nil {
							var s string
							if err2 := json.Unmarshal(step.Argv, &s); err2 == nil {
								_ = json.Unmarshal([]byte(s), &sArgv)
							}
						}
					}
					if len(sArgv) > 0 {
						stats.commands.stepArgv++
					}
				}
			}
		}
	}
	stats.turns = len(turnCalls)
	for _, names := range turnCalls {
		if len(names) != 1 {
			continue
		}
		if names[0] == "update_todos" {
			stats.soloTodoTurns++
		}
		switch names[0] {
		case "read", "git_readonly":
			stats.singleInspectTurns++
		}
	}
	return stats, nil
}

func skillReadPathHashes(input json.RawMessage) []string {
	var args struct {
		Path          string `json:"path"`
		FilePath      string `json:"file_path"`
		FilePathCamel string `json:"filePath"`
		File          string `json:"file"`
		Filename      string `json:"filename"`
		FilepathAlt   string `json:"filepath"`
		AbsolutePath  string `json:"absolute_path"`
		TargetFile    string `json:"target_file"`
	}
	if json.Unmarshal(input, &args) != nil {
		return nil
	}
	path := args.Path
	if path == "" {
		path = firstNonEmptyString(
			args.FilePath,
			args.FilePathCamel,
			args.File,
			args.Filename,
			args.FilepathAlt,
			args.AbsolutePath,
			args.TargetFile,
		)
	}
	if filepath.Base(filepath.Clean(path)) != "SKILL.md" {
		return nil
	}
	return []string{fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(path))))}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func collectCompactionStats(dir string, tree *Tree) (compactionStats, parallelStats, error) {
	var compactions compactionStats
	var parallel parallelStats
	seenBatches := make(map[string]struct{})
	if tree != nil {
		for _, entry := range tree.Entries {
			switch entry.Type {
			case EntrySegment:
				collectParallelBatches(entry.Messages, seenBatches, &parallel)
			case EntryContextReset:
				collectParallelBatches(entry.Messages, seenBatches, &parallel)
				if entry.ContextDelta != nil {
					for _, splice := range entry.ContextDelta.Splices {
						collectParallelBatches(splice.Messages, seenBatches, &parallel)
					}
				}
			case EntryCompaction:
				if entry.Checkpoint != nil {
					collectParallelBatches([]llm.Message{*entry.Checkpoint}, seenBatches, &parallel)
				}
			}
		}
	}

	base := filepath.Join(dir, "compactions")
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return compactions, parallel, nil
		}
		return compactions, parallel, fmt.Errorf("read compactions in %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".meta.json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		metaPath := filepath.Join(base, name)
		var meta compactionMetadata
		// Metadata is additive. Decode the canonical fields while tolerating
		// fields written by a newer Harness; malformed JSON and trailing values
		// are still rejected by decodeJSONFile.
		if err := decodeJSONFile(metaPath, &meta, false); err != nil {
			return compactionStats{}, parallelStats{}, fmt.Errorf("decode compaction metadata %s: %w", metaPath, err)
		}
		var messages []llm.Message
		inputPath := filepath.Join(dir, meta.Input)
		if err := decodeJSONFile(inputPath, &messages, false); err != nil {
			return compactionStats{}, parallelStats{}, fmt.Errorf("decode compaction input %s: %w", inputPath, err)
		}
		compactions.runs++
		compactions.messageCount += meta.MessageCount
		switch meta.SummarySource {
		case "model":
			compactions.modelSummaries++
		case "deterministic":
			compactions.deterministicSummaries++
		default:
			compactions.unknownSummaries++
		}
		if meta.FallbackReason != "" {
			if compactions.fallbackReasons == nil {
				compactions.fallbackReasons = make(map[string]int)
			}
			compactions.fallbackReasons[meta.FallbackReason]++
		}
		compactions.usage = addUsage(compactions.usage, meta.Usage)
		collectParallelBatches(messages, seenBatches, &parallel)
	}
	return compactions, parallel, nil
}

func collectTreeStats(tree *Tree) treeStats {
	if tree == nil {
		return treeStats{}
	}
	stats := treeStats{entries: len(tree.Entries)}
	parents := make(map[string]bool, len(tree.Entries))
	for _, entry := range tree.Entries {
		if entry.ParentID != "" {
			parents[entry.ParentID] = true
		}
		switch entry.Type {
		case EntryBranch:
			stats.branches++
		case EntryContextReset:
			stats.contextResets++
			if entry.ContextDelta != nil {
				stats.deltaResetEntries++
				if payload, err := json.Marshal(entry.ContextDelta); err == nil {
					stats.deltaResetBytes += len(payload)
				}
			} else {
				stats.snapshotResetEntries++
				if len(entry.Messages) > 0 {
					if payload, err := json.Marshal(entry.Messages); err == nil {
						stats.snapshotResetBytes += len(payload)
					}
				}
			}
		}
		path, err := tree.Path(entry.ID)
		if err == nil {
			stats.maxDepth = max(stats.maxDepth, len(path))
		}
	}
	for _, entry := range tree.Entries {
		if !parents[entry.ID] {
			stats.leaves++
		}
	}
	if path, err := tree.Path(tree.ActiveLeaf); err == nil {
		stats.activeDepth = len(path)
	}
	return stats
}

func collectParallelBatches(messages []llm.Message, seen map[string]struct{}, stats *parallelStats) {
	for _, msg := range messages {
		for _, batch := range msg.ParallelToolBatches {
			keyBytes, _ := json.Marshal(batch.ToolUseIDs)
			key := string(keyBytes)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			stats.batches++
			stats.calls += len(batch.ToolUseIDs)
			stats.largest = max(stats.largest, len(batch.ToolUseIDs))
		}
	}
}

func collectDelegateStats(rootDir string) ([]*delegateStats, error) {
	var delegates []*delegateStats
	seenIDs := make(map[string]struct{})
	var collect func(string) error
	collect = func(parentDir string) error {
		childrenDir := filepath.Join(parentDir, "children")
		info, err := os.Lstat(childrenDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read delegates in %s: %w", parentDir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		entries, err := os.ReadDir(childrenDir)
		if err != nil {
			return fmt.Errorf("read delegates in %s: %w", parentDir, err)
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				continue
			}
			dir := filepath.Join(childrenDir, entry.Name())
			metaPath := filepath.Join(dir, "meta.json")
			var meta ChildMeta
			if err := decodeJSONFile(metaPath, &meta, true); err != nil {
				return fmt.Errorf("decode delegate metadata %s: %w", metaPath, err)
			}
			if meta.ID == "" {
				return fmt.Errorf("decode delegate metadata %s: missing id", metaPath)
			}
			if _, ok := seenIDs[meta.ID]; ok {
				return fmt.Errorf("decode delegate metadata %s: duplicate id %q", metaPath, meta.ID)
			}
			seenIDs[meta.ID] = struct{}{}
			stats, err := collectSessionStatsWithFallback(dir, &meta)
			if err != nil {
				return fmt.Errorf("collect delegate %s: %w", meta.ID, err)
			}
			delegates = append(delegates, &delegateStats{meta: meta, stats: stats})
			if err := collect(dir); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(rootDir); err != nil {
		return nil, err
	}
	return delegates, nil
}

func decodeJSONFile(path string, dst any, strict bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func (stats *toolStats) add(other toolStats) {
	if stats.byName == nil {
		stats.byName = make(map[string]int)
	}
	if stats.resultsByName == nil {
		stats.resultsByName = make(map[string]toolResultStats)
	}
	if stats.callShapes == nil {
		stats.callShapes = make(map[string]map[string]int)
	}
	if stats.skillReadPaths == nil {
		stats.skillReadPaths = make(map[string]int)
	}
	if stats.skillActivations == nil {
		stats.skillActivations = make(map[string]int)
	}
	stats.calls += other.calls
	stats.turns += other.turns
	stats.soloTodoTurns += other.soloTodoTurns
	stats.singleInspectTurns += other.singleInspectTurns
	stats.resultErrors += other.resultErrors
	stats.resultTruncations += other.resultTruncations
	stats.resultOriginalBytes += other.resultOriginalBytes
	stats.resultShownBytes += other.resultShownBytes
	stats.resultTotalMS += other.resultTotalMS
	stats.resultMaxMS = max(stats.resultMaxMS, other.resultMaxMS)
	stats.skillReads += other.skillReads
	for name, count := range other.byName {
		stats.byName[name] += count
	}
	for name, result := range other.resultsByName {
		combined := stats.resultsByName[name]
		combined.add(result)
		stats.resultsByName[name] = combined
	}
	for name, shapes := range other.callShapes {
		if stats.callShapes[name] == nil {
			stats.callShapes[name] = make(map[string]int)
		}
		for shape, count := range shapes {
			stats.callShapes[name][shape] += count
		}
	}
	for path, count := range other.skillReadPaths {
		stats.skillReadPaths[path] += count
	}
	for activation, count := range other.skillActivations {
		stats.skillActivations[activation] += count
	}
	stats.commands.add(other.commands)
	stats.parallel.add(other.parallel)
}

func cloneToolStats(stats toolStats) toolStats {
	clone := stats
	clone.byName = make(map[string]int, len(stats.byName))
	for name, count := range stats.byName {
		clone.byName[name] = count
	}
	clone.resultsByName = make(map[string]toolResultStats, len(stats.resultsByName))
	for name, result := range stats.resultsByName {
		clone.resultsByName[name] = result
	}
	clone.callShapes = make(map[string]map[string]int, len(stats.callShapes))
	for name, shapes := range stats.callShapes {
		clone.callShapes[name] = make(map[string]int, len(shapes))
		for shape, count := range shapes {
			clone.callShapes[name][shape] = count
		}
	}
	clone.skillReadPaths = make(map[string]int, len(stats.skillReadPaths))
	for path, count := range stats.skillReadPaths {
		clone.skillReadPaths[path] = count
	}
	clone.skillActivations = make(map[string]int, len(stats.skillActivations))
	for activation, count := range stats.skillActivations {
		clone.skillActivations[activation] = count
	}
	return clone
}

func (stats *commandStats) add(other commandStats) {
	stats.calls += other.calls
	stats.foreground += other.foreground
	stats.background += other.background
	stats.shell += other.shell
	stats.argv += other.argv
	stats.stepBatches += other.stepBatches
	stats.stepCommands += other.stepCommands
	stats.stepShell += other.stepShell
	stats.stepArgv += other.stepArgv
}

func (stats *toolResultStats) add(other toolResultStats) {
	stats.results += other.results
	stats.errors += other.errors
	stats.truncations += other.truncations
	stats.originalBytes += other.originalBytes
	stats.shownBytes += other.shownBytes
	stats.totalMS += other.totalMS
	stats.maxMS = max(stats.maxMS, other.maxMS)
}

func (stats *parallelStats) add(other parallelStats) {
	stats.batches += other.batches
	stats.calls += other.calls
	stats.largest = max(stats.largest, other.largest)
}

func (stats *compactionStats) add(other compactionStats) {
	stats.runs += other.runs
	stats.messageCount += other.messageCount
	stats.modelSummaries += other.modelSummaries
	stats.deterministicSummaries += other.deterministicSummaries
	stats.unknownSummaries += other.unknownSummaries
	if len(other.fallbackReasons) > 0 && stats.fallbackReasons == nil {
		stats.fallbackReasons = make(map[string]int)
	}
	for reason, count := range other.fallbackReasons {
		stats.fallbackReasons[reason] += count
	}
	stats.usage = addUsage(stats.usage, other.usage)
}

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:        a.InputTokens + b.InputTokens,
		OutputTokens:       a.OutputTokens + b.OutputTokens,
		CacheReadTokens:    a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:   a.CacheWriteTokens + b.CacheWriteTokens,
		CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens,
		ReasoningTokens:    a.ReasoningTokens + b.ReasoningTokens,
		CostUSD:            a.CostUSD + b.CostUSD,
		CostKnown:          a.CostKnown || b.CostKnown,
	}
}

func totalTokens(usage llm.Usage) int {
	return usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens + usage.CacheWrite1hTokens + usage.OutputTokens + usage.ReasoningTokens
}

func writeStats(report statsReport, w io.Writer) error {
	var b strings.Builder
	writeSessionStats(&b, report)
	writeConversationStats(&b, report.root)
	writeTreeStats(&b, report.root.tree)
	writeActiveContextStats(&b, report.root)
	writeOverallToolStats(&b, report)
	writeErrorStats(&b, report)
	fmt.Fprintln(&b, "Operational telemetry")
	writeTelemetryText(&b, "  ", report.telemetry)
	writeRootUsage(&b, report.root.state)
	writeDirectUsage(&b, report)
	writeOverallCompactions(&b, report)
	writeDelegates(&b, report)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeSessionStats(w io.Writer, report statsReport) {
	state := report.root.state
	fmt.Fprintln(w, "Session")
	fmt.Fprintf(w, "  path: %s\n", report.path)
	fmt.Fprintf(w, "  id: %s\n", state.ID)
	if state.ParentSession != "" {
		fmt.Fprintf(w, "  parent: %s@%s\n", state.ParentSession, state.ParentEntryID)
	}
	if state.CWD != "" {
		fmt.Fprintf(w, "  cwd: %s\n", state.CWD)
	}
	fmt.Fprintf(w, "  agent: %s\n", state.Agent)
	fmt.Fprintf(w, "  provider/model: %s/%s\n", state.Provider, state.Model)
	fmt.Fprintf(w, "  created: %s\n", state.Created.Format(time.RFC3339))
	fmt.Fprintf(w, "  updated: %s\n", state.Updated.Format(time.RFC3339))
	fmt.Fprintf(w, "  duration: %s\n", formatDuration(state.Updated.Sub(state.Created)))
	if state.Build.Version != "" {
		fmt.Fprintf(w, "  build: %s", state.Build.Version)
		if state.Build.Commit != "" {
			fmt.Fprintf(w, " (%s)", state.Build.Commit)
		}
		if state.Build.Modified {
			fmt.Fprint(w, " modified")
		}
		fmt.Fprintln(w)
	}
	if state.Runtime.SearchBackend != "" {
		fmt.Fprintf(w, "  runtime: retention=%s context=%d search=%s delegate-max=%d active=%d descendants=%d prewarm=%t\n",
			state.Runtime.RetentionPolicy, state.Runtime.ContextWindow, state.Runtime.SearchBackend,
			state.Runtime.DelegateMaxTurns, state.Runtime.DelegateMaxActive, state.Runtime.DelegateMaxDescendants,
			state.Runtime.Prewarm)
	}
}

func writeConversationStats(w io.Writer, stats collectedSessionStats) {
	fmt.Fprintln(w, "Conversation")
	writeConversationValues(w, "  ", stats)
}

func writeConversationValues(w io.Writer, indent string, stats collectedSessionStats) {
	fmt.Fprintf(w, "%sprompts: %d\n", indent, stats.prompts)
	fmt.Fprintf(w, "%sturns: %d\n", indent, stats.turns)
	fmt.Fprintf(w, "%smodel calls: %d\n", indent, stats.modelCalls)
	fmt.Fprintf(w, "%sretries: %d\n", indent, stats.retries)
	fmt.Fprintf(w, "%smaintenance calls: %d\n", indent, stats.maintenanceCalls)
	fmt.Fprintf(w, "%snavigations: %d\n", indent, stats.navigations)
	if stats.maintenanceCalls > 0 {
		fmt.Fprintf(w, "%smaintenance usage: %d in / %d out\n", indent, stats.maintenanceUsage.InputTokens, stats.maintenanceUsage.OutputTokens)
	}
	if !stats.checkpointed {
		fmt.Fprintf(w, "%scheckpoint: unavailable\n", indent)
	}
	if stats.checkpoints.saves > 0 {
		averageMS := stats.checkpoints.totalMS / int64(stats.checkpoints.saves)
		fmt.Fprintf(w, "%sclosed-turn checkpoints: %d\n", indent, stats.checkpoints.saves)
		fmt.Fprintf(
			w,
			"%scheckpoint save time: average %s / max %s\n",
			indent,
			formatDuration(time.Duration(averageMS)*time.Millisecond),
			formatDuration(time.Duration(stats.checkpoints.maxMS)*time.Millisecond),
		)
		fmt.Fprintf(w, "%scheckpoint lag turns: %d\n", indent, stats.checkpoints.lagTurns)
		fmt.Fprintf(w, "%scheckpoint lag seconds: %.3f\n", indent, stats.checkpoints.lagSeconds)
	}
	if stats.retention.epochs > 0 {
		fmt.Fprintf(w, "%sretention epochs: %d\n", indent, stats.retention.epochs)
		fmt.Fprintf(w, "%s  pressure/age: %d / %d\n", indent, stats.retention.pressureEpochs, stats.retention.agePasses)
		fmt.Fprintf(w, "%s  blocks trimmed: %d\n", indent, stats.retention.blocksTrimmed)
		fmt.Fprintf(w, "%s  bytes trimmed: %d\n", indent, stats.retention.bytesTrimmed)
		fmt.Fprintf(w, "%s  response-state resets: %d\n", indent, stats.retention.responseStateResets)
		fmt.Fprintf(w, "%s  measurement-anchor/continuation resets: %d / %d\n", indent, stats.retention.measurementAnchorResets, stats.retention.continuationResets)
		fmt.Fprintf(w, "%s  next requests stateful/full/stateless/unknown: %d / %d / %d / %d\n", indent,
			stats.retention.statefulRequests, stats.retention.fullContextRequests, stats.retention.statelessRequests, stats.retention.unknownRequests)
	}
	if stats.idleCompactions.attempts > 0 {
		averageMS := stats.idleCompactions.totalMS / int64(stats.idleCompactions.attempts)
		fmt.Fprintf(w, "%sidle compaction attempts: %d\n", indent, stats.idleCompactions.attempts)
		fmt.Fprintf(
			w,
			"%s  outcomes applied/discarded/failed/no-change: %d / %d / %d / %d\n",
			indent,
			stats.idleCompactions.applied,
			stats.idleCompactions.discarded,
			stats.idleCompactions.failed,
			stats.idleCompactions.noChange,
		)
		fmt.Fprintf(
			w,
			"%s  wall time average/max: %s / %s\n",
			indent,
			formatDuration(time.Duration(averageMS)*time.Millisecond),
			formatDuration(time.Duration(stats.idleCompactions.maxMS)*time.Millisecond),
		)
		if stats.idleCompactions.applied > 0 {
			fmt.Fprintf(
				w,
				"%s  applied context average before/after: %d / %d\n",
				indent,
				stats.idleCompactions.appliedBeforeTotal/stats.idleCompactions.applied,
				stats.idleCompactions.appliedAfterTotal/stats.idleCompactions.applied,
			)
		}
	}
	fmt.Fprintf(w, "%sactive messages: %d\n", indent, len(stats.state.Messages))
	if len(stats.terminationCounts) > 0 {
		fmt.Fprintf(w, "%sprompt termination reasons:\n", indent)
		for _, reason := range sortedMapKeys(stats.terminationCounts) {
			fmt.Fprintf(w, "%s  %s: %d\n", indent, reason, stats.terminationCounts[reason])
		}
	}
}

func writeTreeStats(w io.Writer, stats treeStats) {
	fmt.Fprintln(w, "Tree")
	fmt.Fprintf(w, "  entries: %d\n", stats.entries)
	fmt.Fprintf(w, "  branches: %d\n", stats.branches)
	fmt.Fprintf(w, "  leaves: %d\n", stats.leaves)
	fmt.Fprintf(w, "  maximum depth: %d\n", stats.maxDepth)
	fmt.Fprintf(w, "  active depth: %d\n", stats.activeDepth)
	fmt.Fprintf(w, "  context resets: %d\n", stats.contextResets)
	fmt.Fprintf(w, "  context resets snapshot/legacy-delta: %d / %d\n", stats.snapshotResetEntries, stats.deltaResetEntries)
	fmt.Fprintf(w, "  context reset payload bytes snapshot/legacy-delta: %d / %d\n", stats.snapshotResetBytes, stats.deltaResetBytes)
}

func writeActiveContextStats(w io.Writer, stats collectedSessionStats) {
	context := stats.context
	fmt.Fprintln(w, "Active context")
	fmt.Fprintf(w, "  messages/content blocks: %d / %d\n", context.messages, context.blocks)
	textBytes := context.userTextBytes + context.assistantTextBytes +
		context.toolInputBytes + context.toolResultBytes +
		context.reasoningTextBytes + context.interactionTextBytes
	opaqueBytes := context.reasoningOpaqueBytes + context.interactionOpaqueBytes + context.otherBytes
	payloadBytes := textBytes + opaqueBytes + context.imageEncodedBytes
	estimatedTokens := divideRoundUp(textBytes, 4) + divideRoundUp(opaqueBytes, 8)
	fmt.Fprintf(w, "  active payload: %d B (est. %d text/tool tokens; images excluded)\n", payloadBytes, estimatedTokens)
	fmt.Fprintf(w, "  user/assistant text: %d B / %d B\n", context.userTextBytes, context.assistantTextBytes)
	fmt.Fprintf(w, "  tool inputs/results: %d B / %d B\n", context.toolInputBytes, context.toolResultBytes)
	fmt.Fprintf(w, "  reasoning text/opaque: %d B / %d B\n",
		context.reasoningTextBytes+context.interactionTextBytes,
		context.reasoningOpaqueBytes+context.interactionOpaqueBytes)
	if context.imageEncodedBytes > 0 {
		fmt.Fprintf(w, "  encoded images: %d B\n", context.imageEncodedBytes)
	}
	if context.otherBytes > 0 {
		fmt.Fprintf(w, "  other opaque payload: %d B\n", context.otherBytes)
	}
	if snapshot := stats.latestContext; snapshot != nil {
		fmt.Fprintf(w, "  latest request estimate: %d / %d tokens (%s)\n", snapshot.Total, snapshot.Window, snapshot.Source)
		fmt.Fprintf(w, "    system/tools/messages: %d / %d / %d\n", snapshot.System, snapshot.Tools, snapshot.Messages)
		if snapshot.PayloadTotal > 0 {
			fmt.Fprintf(w, "    measured payload system/tools/messages: %d / %d / %d\n",
				snapshot.PayloadSystem, snapshot.PayloadTools, snapshot.PayloadMessages)
		}
	}
}

func divideRoundUp(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}

func writeOverallToolStats(w io.Writer, report statsReport) {
	root := report.root.tools
	delegates := report.delegateTools
	all := report.tools
	fmt.Fprintln(w, "Tools")
	writeSplitValue(w, "  ", "tool calls", all.calls, root.calls, delegates.calls)
	writeSplitValue(w, "  ", "tool-bearing turns", all.turns, root.turns, delegates.turns)
	if all.turns > 0 {
		fmt.Fprintf(w, "  calls per tool-bearing turn: %.2f\n", float64(all.calls)/float64(all.turns))
	}
	writeSplitValue(w, "  ", "standalone TODO turns", all.soloTodoTurns, root.soloTodoTurns, delegates.soloTodoTurns)
	writeSplitValue(w, "  ", "single inspection turns", all.singleInspectTurns, root.singleInspectTurns, delegates.singleInspectTurns)
	fmt.Fprintf(w, "  results: %d errors / %d truncated / %d B shown / %d B original\n",
		all.resultErrors, all.resultTruncations, all.resultShownBytes, all.resultOriginalBytes)
	if all.calls > 0 {
		fmt.Fprintf(w, "  tool wall time: average %s / max %s\n",
			formatDuration(time.Duration(all.resultTotalMS/int64(all.calls))*time.Millisecond),
			formatDuration(time.Duration(all.resultMaxMS)*time.Millisecond))
	}
	if len(all.resultsByName) > 0 {
		fmt.Fprintln(w, "  result volume by tool (largest first):")
		for _, name := range topResultTools(all.resultsByName, 8) {
			result := all.resultsByName[name]
			fmt.Fprintf(w, "    %s: %d results, %d errors, %d truncated, %d B shown / %d B original\n",
				name, result.results, result.errors, result.truncations, result.shownBytes, result.originalBytes)
		}
	}
	repeats := repeatedToolCalls(all.callShapes)
	if len(repeats) > 0 {
		fmt.Fprintln(w, "  repeated normalized calls (inputs redacted):")
		for i, repeat := range repeats {
			if i == 8 {
				break
			}
			fmt.Fprintf(w, "    %s: %d duplicate executions across %d repeated inputs (max %d identical)\n",
				repeat.name, repeat.duplicates, repeat.inputs, repeat.maximum)
		}
	}
	if all.skillReads > 0 {
		repeats := 0
		for _, count := range all.skillReadPaths {
			repeats += max(count-1, 0)
		}
		fmt.Fprintf(w, "  SKILL.md tool reads: %d (%d unique paths, %d re-reads)\n",
			all.skillReads, len(all.skillReadPaths), repeats)
	}
	if len(all.skillActivations) > 0 {
		total := 0
		for _, count := range all.skillActivations {
			total += count
		}
		fmt.Fprintf(w, "  skill activations: %d\n", total)
		for _, activation := range sortedMapKeys(all.skillActivations) {
			fmt.Fprintf(w, "    %s: %d\n", activation, all.skillActivations[activation])
		}
	}
	if len(all.byName) == 0 {
		fmt.Fprintln(w, "  by tool: none")
	} else {
		fmt.Fprintln(w, "  by tool:")
		for _, name := range sortedMapKeys(all.byName) {
			writeSplitValue(w, "    ", name, all.byName[name], root.byName[name], delegates.byName[name])
		}
	}
	writeSplitValue(w, "  ", "command calls", all.commands.calls, root.commands.calls, delegates.commands.calls)
	writeSplitValue(w, "    ", "foreground", all.commands.foreground, root.commands.foreground, delegates.commands.foreground)
	writeSplitValue(w, "    ", "background", all.commands.background, root.commands.background, delegates.commands.background)
	writeSplitValue(w, "    ", "shell-string", all.commands.shell, root.commands.shell, delegates.commands.shell)
	writeSplitValue(w, "    ", "argv", all.commands.argv, root.commands.argv, delegates.commands.argv)
	writeSplitValue(w, "    ", "step batches", all.commands.stepBatches, root.commands.stepBatches, delegates.commands.stepBatches)
	writeSplitValue(w, "    ", "step commands", all.commands.stepCommands, root.commands.stepCommands, delegates.commands.stepCommands)
	writeSplitValue(w, "      ", "step shell-string", all.commands.stepShell, root.commands.stepShell, delegates.commands.stepShell)
	writeSplitValue(w, "      ", "step argv", all.commands.stepArgv, root.commands.stepArgv, delegates.commands.stepArgv)
	writeSplitValue(w, "  ", "parallel batches", all.parallel.batches, root.parallel.batches, delegates.parallel.batches)
	writeSplitValue(w, "  ", "parallel calls", all.parallel.calls, root.parallel.calls, delegates.parallel.calls)
	fmt.Fprintf(w, "  largest parallel batch: %d (root %d, delegates %d)\n", all.parallel.largest, root.parallel.largest, delegates.parallel.largest)
}

// writeErrorStats prints the Errors section: classified tool and model
// failures across root and delegate sessions. It is omitted entirely when
// nothing failed, keeping clean sessions quiet.
func writeErrorStats(w io.Writer, report statsReport) {
	summary := report.errors
	if summary.FailedToolResults == 0 && summary.FailedCommandResults == 0 && summary.ModelRequestFailures == 0 {
		return
	}
	fmt.Fprintln(w, "Errors")
	fmt.Fprintf(w, "  failed tool results: %d/%d (%.1f%%)\n", summary.FailedToolResults, summary.ToolResults, summary.ToolErrorRate*100)
	if summary.CommandResults > 0 {
		fmt.Fprintf(w, "  command execution failures: %d/%d (%.1f%%); effective failures: %d/%d (%.1f%%); cancelled: %d\n",
			summary.FailedCommandResults, summary.CommandResults, summary.CommandFailureRate*100,
			summary.EffectiveFailedResults, summary.ToolResults, summary.EffectiveFailureRate*100,
			summary.CancelledCommandResults)
	}
	if summary.ModelRequestFailures > 0 {
		fmt.Fprintf(w, "  model request failures: %d\n", summary.ModelRequestFailures)
	}
	if len(summary.ByTool) > 0 {
		fmt.Fprintf(w, "  by tool: %s\n", formatErrorRates(summary.ByTool, summary.ResultsByTool))
	}
	if len(summary.ByKind) > 0 {
		fmt.Fprintf(w, "  by kind: %s\n", formatErrorCounts(summary.ByKind))
	}
	if len(summary.ByModel) > 0 {
		fmt.Fprintf(w, "  by model: %s\n", formatErrorRates(summary.ByModel, summary.ResultsByModel))
	}
	if len(summary.CompositeDiagnostics) > 0 {
		fmt.Fprintf(w, "  composite diagnostics: %s\n", formatErrorCounts(summary.CompositeDiagnostics))
	}
	if len(summary.Repeats) > 0 {
		fmt.Fprintln(w, "  repeated failures:")
		for _, repeat := range summary.Repeats {
			tool := repeat.Tool
			if tool == "" {
				tool = "-"
			}
			fmt.Fprintf(w, "    %s: %s (%d consecutive)\n", tool, repeat.Kind, repeat.Consecutive)
		}
	}
}

func formatErrorRates(errorsByKey, resultsByKey map[string]int) string {
	keys := make([]string, 0, len(errorsByKey))
	for key := range errorsByKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := errorsByKey[keys[i]], errorsByKey[keys[j]]
		if left != right {
			return left > right
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, key := range keys {
		results := resultsByKey[key]
		rate := 0.0
		if results > 0 {
			rate = float64(errorsByKey[key]) / float64(results) * 100
		}
		parts[i] = fmt.Sprintf("%s (%d/%d, %.1f%%)", key, errorsByKey[key], results, rate)
	}
	return strings.Join(parts, ", ")
}

// formatErrorCounts renders a count map as "name (n), ...", highest counts
// first with alphabetical tie-breaks for deterministic output.
func formatErrorCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = fmt.Sprintf("%s (%d)", key, counts[key])
	}
	return strings.Join(parts, ", ")
}

type repeatedToolCall struct {
	name       string
	duplicates int
	inputs     int
	maximum    int
}

func repeatedToolCalls(callShapes map[string]map[string]int) []repeatedToolCall {
	var repeated []repeatedToolCall
	for name, shapes := range callShapes {
		item := repeatedToolCall{name: name}
		for _, count := range shapes {
			if count < 2 {
				continue
			}
			item.inputs++
			item.duplicates += count - 1
			item.maximum = max(item.maximum, count)
		}
		if item.duplicates > 0 {
			repeated = append(repeated, item)
		}
	}
	sort.Slice(repeated, func(i, j int) bool {
		if repeated[i].duplicates != repeated[j].duplicates {
			return repeated[i].duplicates > repeated[j].duplicates
		}
		return repeated[i].name < repeated[j].name
	})
	return repeated
}

func topResultTools(results map[string]toolResultStats, limit int) []string {
	names := sortedMapKeys(results)
	sort.SliceStable(names, func(i, j int) bool {
		left, right := results[names[i]], results[names[j]]
		if left.originalBytes != right.originalBytes {
			return left.originalBytes > right.originalBytes
		}
		if left.results != right.results {
			return left.results > right.results
		}
		return names[i] < names[j]
	})
	if len(names) > limit {
		names = names[:limit]
	}
	return names
}

func writeSplitValue(w io.Writer, indent, label string, total, root, delegates int) {
	fmt.Fprintf(w, "%s%s: %d total (%d root, %d delegates)\n", indent, label, total, root, delegates)
}

func writeRootUsage(w io.Writer, state Session) {
	fmt.Fprintln(w, "Usage (includes delegates)")
	writeUsageValues(w, "  ", state.Usage.Usage, state.Usage.CostUSD)
	writeReasoningReplay(w, "  ", state)
	if len(state.UsageByModel) == 0 {
		return
	}
	fmt.Fprintln(w, "  by model:")
	for _, name := range sortedMapKeys(state.UsageByModel) {
		usage := state.UsageByModel[name]
		fmt.Fprintf(w, "    %s:\n", name)
		writeUsageValues(w, "      ", usage.Usage, usage.CostUSD)
	}
}

// writeReasoningReplay surfaces the reasoning-replay share of the active
// branch: the persisted thinking payloads the Anthropic dialect replays
// verbatim on every request while thinking is enabled (some providers bill
// that replay — Kimi counts preserved thinking toward token consumption).
// The token estimate mirrors the agent estimator's weights: prose at 4
// bytes/token, provider-opaque blobs (signatures, redacted/encrypted
// reasoning) at 8. The cumulative reasoning-token figure is already in the
// usage lines above.
func writeReasoningReplay(w io.Writer, indent string, state Session) {
	blocks, textBytes, opaqueBytes := 0, 0, 0
	for _, m := range state.Messages {
		for _, b := range m.Content {
			text := len(b.Thinking)
			opaque := len(b.ThinkingSignature) + len(b.RedactedData) + len(b.ReasoningEncrypted)
			if text+opaque == 0 {
				continue
			}
			blocks++
			textBytes += text
			opaqueBytes += opaque
		}
	}
	if blocks == 0 {
		return
	}
	est := textBytes/4 + opaqueBytes/8
	fmt.Fprintf(w, "%sreasoning replay: %d blocks, %d bytes (est. %d tokens of active context)\n", indent, blocks, textBytes+opaqueBytes, est)
}

func writeDirectUsage(w io.Writer, report statsReport) {
	fmt.Fprintln(w, "Direct model activity (non-overlapping)")
	writeSplitValue(w, "  ", "conversational calls", report.directModelCalls,
		report.root.modelCalls, report.delegateModelCalls)
	writeSplitValue(w, "  ", "maintenance calls", report.directMaintCalls,
		report.root.maintenanceCalls, report.delegateMaintCalls)
	fmt.Fprintln(w, "  usage (turn attempts plus maintenance):")
	writeUsageValues(w, "    ", report.directUsage, report.directUsage.CostUSD)
	fmt.Fprintln(w, "  usage split:")
	writeCompactUsage(w, "    root", report.root.directUsage)
	writeCompactUsage(w, "    delegates", report.delegateDirectUsage)
}

func writeCompactUsage(w io.Writer, label string, usage llm.Usage) {
	cacheWrite := usage.CacheWriteTokens + usage.CacheWrite1hTokens
	fmt.Fprintf(w, "%s: %d tokens (%d uncached in, %d cache read, %d cache write, %d output, %d reasoning), $%.4f\n",
		label,
		totalTokens(usage),
		usage.InputTokens,
		usage.CacheReadTokens,
		cacheWrite,
		usage.OutputTokens,
		usage.ReasoningTokens,
		usage.CostUSD,
	)
}

func writeUsageValues(w io.Writer, indent string, usage llm.Usage, cost float64) {
	fmt.Fprintf(w, "%suncached input: %d\n", indent, usage.InputTokens)
	fmt.Fprintf(w, "%scache read: %d\n", indent, usage.CacheReadTokens)
	fmt.Fprintf(w, "%scache write: %d\n", indent, usage.CacheWriteTokens)
	if usage.CacheWrite1hTokens > 0 {
		fmt.Fprintf(w, "%scache write (1h): %d\n", indent, usage.CacheWrite1hTokens)
	}
	fmt.Fprintf(w, "%soutput: %d\n", indent, usage.OutputTokens)
	fmt.Fprintf(w, "%sreasoning: %d\n", indent, usage.ReasoningTokens)
	fmt.Fprintf(w, "%stotal tokens: %d\n", indent, totalTokens(usage))
	fmt.Fprintf(w, "%scost: $%.4f\n", indent, cost)
}

func writeOverallCompactions(w io.Writer, report statsReport) {
	root := report.root.compactions
	delegates := report.delegateCompactions
	all := report.compactions
	fmt.Fprintln(w, "Compactions")
	writeSplitValue(w, "  ", "runs", all.runs, root.runs, delegates.runs)
	writeSplitValue(w, "  ", "compacted messages", all.messageCount, root.messageCount, delegates.messageCount)
	if all.runs > 0 {
		fmt.Fprintf(w, "  summary sources: model %d, deterministic %d, unknown %d\n", all.modelSummaries, all.deterministicSummaries, all.unknownSummaries)
	}
	if len(all.fallbackReasons) > 0 {
		fmt.Fprintf(w, "  fallback reasons: %s\n", formatErrorCounts(all.fallbackReasons))
	}
	fmt.Fprintln(w, "  usage (already included in session usage):")
	writeUsageValues(w, "    ", all.usage, all.usage.CostUSD)
}

func writeDelegates(w io.Writer, report statsReport) {
	fmt.Fprintf(w, "Delegates (%d)\n", len(report.delegates))
	if len(report.statusCounts) == 0 {
		fmt.Fprintln(w, "  statuses: none")
	} else {
		fmt.Fprintln(w, "  statuses:")
		for _, status := range sortedMapKeys(report.statusCounts) {
			fmt.Fprintf(w, "    %s: %d\n", status, report.statusCounts[status])
		}
	}
	if len(report.terminationCounts) == 0 {
		fmt.Fprintln(w, "  termination reasons: none")
	} else {
		fmt.Fprintln(w, "  termination reasons:")
		for _, reason := range sortedMapKeys(report.terminationCounts) {
			fmt.Fprintf(w, "    %s: %d\n", reason, report.terminationCounts[reason])
		}
	}
	if len(report.delegates) == 0 {
		return
	}
	writeTopDelegates(w, report.delegates)

	byID := make(map[string]*delegateStats, len(report.delegates))
	children := make(map[string][]*delegateStats)
	var roots []*delegateStats
	for _, child := range report.delegates {
		byID[child.meta.ID] = child
		children[child.meta.ParentID] = append(children[child.meta.ParentID], child)
		if child.meta.ParentID == "" {
			roots = append(roots, child)
		}
	}
	for parentID := range children {
		sortDelegateSiblings(children[parentID])
	}
	sortDelegateSiblings(roots)

	rendered := make(map[string]bool, len(report.delegates))
	visiting := make(map[string]bool, len(report.delegates))
	var render func(*delegateStats, string)
	render = func(child *delegateStats, indent string) {
		if rendered[child.meta.ID] || visiting[child.meta.ID] {
			return
		}
		visiting[child.meta.ID] = true
		writeDelegate(w, child, indent)
		for _, nested := range children[child.meta.ID] {
			render(nested, indent+"  ")
		}
		delete(visiting, child.meta.ID)
		rendered[child.meta.ID] = true
	}
	for _, root := range roots {
		render(root, "  ")
	}

	var remaining []string
	for id := range byID {
		if !rendered[id] {
			remaining = append(remaining, id)
		}
	}
	sort.Strings(remaining)
	for _, id := range remaining {
		render(byID[id], "  ")
	}
}

func writeTopDelegates(w io.Writer, delegates []*delegateStats) {
	ranked := append([]*delegateStats(nil), delegates...)
	sort.Slice(ranked, func(i, j int) bool {
		left := totalTokens(ranked[i].stats.directUsage)
		right := totalTokens(ranked[j].stats.directUsage)
		if left != right {
			return left > right
		}
		return ranked[i].meta.ID < ranked[j].meta.ID
	})
	if len(ranked) > 5 {
		ranked = ranked[:5]
	}
	fmt.Fprintln(w, "  highest direct token use:")
	for _, child := range ranked {
		fmt.Fprintf(w, "    %s (%s): %d tokens, %d model calls\n",
			child.meta.ID,
			child.meta.Agent,
			totalTokens(child.stats.directUsage),
			child.stats.modelCalls,
		)
	}
}

func sortDelegateSiblings(delegates []*delegateStats) {
	sort.Slice(delegates, func(i, j int) bool {
		if !delegates[i].meta.Created.Equal(delegates[j].meta.Created) {
			return delegates[i].meta.Created.Before(delegates[j].meta.Created)
		}
		return delegates[i].meta.ID < delegates[j].meta.ID
	})
}

func writeDelegate(w io.Writer, child *delegateStats, indent string) {
	meta := child.meta
	stats := child.stats
	detail := indent + "  "
	fmt.Fprintf(w, "%sDelegate %s\n", indent, meta.ID)
	fmt.Fprintf(w, "%sstatus: %s\n", detail, meta.Status)
	if meta.Mode != "" {
		fmt.Fprintf(w, "%smode: %s\n", detail, meta.Mode)
	}
	if meta.ContinuedFrom != "" {
		fmt.Fprintf(w, "%scontinued from: %s\n", detail, meta.ContinuedFrom)
		if meta.ContinuationMode != "" {
			fmt.Fprintf(w, "%scontinuation mode: %s\n", detail, meta.ContinuationMode)
		}
		if meta.ContinuationWindow > 0 {
			fmt.Fprintf(
				w,
				"%scontinuation context: %d → %d tokens (window %d)\n",
				detail,
				meta.ContinuationBefore,
				meta.ContinuationAfter,
				meta.ContinuationWindow,
			)
		}
	}
	if meta.ParentID != "" {
		fmt.Fprintf(w, "%sparent: %s\n", detail, meta.ParentID)
	}
	if meta.ResourceKey != "" {
		fmt.Fprintf(w, "%sresource: %s (%s)\n", detail, meta.ResourceKey, meta.Access)
	}
	fmt.Fprintf(w, "%sagent: %s\n", detail, meta.Agent)
	fmt.Fprintf(w, "%sprovider/model: %s/%s\n", detail, meta.Provider, meta.Model)
	fmt.Fprintf(w, "%stask: %s\n", detail, oneLine(meta.TaskPreview))
	fmt.Fprintf(w, "%screated: %s\n", detail, meta.Created.Format(time.RFC3339))
	fmt.Fprintf(w, "%supdated: %s\n", detail, meta.Updated.Format(time.RFC3339))
	fmt.Fprintf(w, "%sduration: %s\n", detail, formatDuration(meta.Updated.Sub(meta.Created)))
	if meta.EffectiveMaxTurns > 0 {
		if meta.RequestedMaxTurns != nil {
			fmt.Fprintf(w, "%sturn budget: %d requested, %d effective\n", detail, *meta.RequestedMaxTurns, meta.EffectiveMaxTurns)
		} else {
			fmt.Fprintf(w, "%sturn budget: %d effective (configured default)\n", detail, meta.EffectiveMaxTurns)
		}
		fmt.Fprintf(w, "%sturns used: %d\n", detail, meta.TurnsUsed)
	}
	if meta.TerminationReason != "" {
		fmt.Fprintf(w, "%stermination reason: %s\n", detail, meta.TerminationReason)
	}
	writeConversationValues(w, detail, stats)
	fmt.Fprintf(w, "%stool calls: %d\n", detail, stats.tools.calls)
	if len(stats.tools.byName) == 0 {
		fmt.Fprintf(w, "%sby tool: none\n", detail)
	} else {
		fmt.Fprintf(w, "%sby tool:\n", detail)
		for _, name := range sortedMapKeys(stats.tools.byName) {
			fmt.Fprintf(w, "%s  %s: %d\n", detail, name, stats.tools.byName[name])
		}
	}
	fmt.Fprintf(w, "%scommand calls: %d\n", detail, stats.tools.commands.calls)
	fmt.Fprintf(w, "%s  foreground: %d\n", detail, stats.tools.commands.foreground)
	fmt.Fprintf(w, "%s  background: %d\n", detail, stats.tools.commands.background)
	fmt.Fprintf(w, "%s  shell-string: %d\n", detail, stats.tools.commands.shell)
	fmt.Fprintf(w, "%s  argv: %d\n", detail, stats.tools.commands.argv)
	fmt.Fprintf(w, "%sparallel batches: %d\n", detail, stats.tools.parallel.batches)
	fmt.Fprintf(w, "%sparallel calls: %d\n", detail, stats.tools.parallel.calls)
	fmt.Fprintf(w, "%slargest parallel batch: %d\n", detail, stats.tools.parallel.largest)
	if stats.compactions.runs != 0 || stats.compactions.messageCount != 0 || totalTokens(stats.compactions.usage) != 0 || stats.compactions.usage.CostUSD != 0 {
		fmt.Fprintf(w, "%scompactions: %d runs, %d messages\n", detail, stats.compactions.runs, stats.compactions.messageCount)
		fmt.Fprintf(w, "%scompaction summary sources: model %d, deterministic %d, unknown %d\n", detail, stats.compactions.modelSummaries, stats.compactions.deterministicSummaries, stats.compactions.unknownSummaries)
		if len(stats.compactions.fallbackReasons) > 0 {
			fmt.Fprintf(w, "%scompaction fallback reasons: %s\n", detail, formatErrorCounts(stats.compactions.fallbackReasons))
		}
		fmt.Fprintf(w, "%scompaction usage:\n", detail)
		writeUsageValues(w, detail+"  ", stats.compactions.usage, stats.compactions.usage.CostUSD)
	}
	fmt.Fprintf(w, "%susage (includes nested delegates):\n", detail)
	writeUsageValues(w, detail+"  ", stats.state.Usage.Usage, stats.state.Usage.CostUSD)
	if meta.Error != "" {
		fmt.Fprintf(w, "%serror: %s\n", detail, oneLine(meta.Error))
	}
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
