package session

import (
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

type statsReport struct {
	path                string
	root                collectedSessionStats
	delegates           []*delegateStats
	tools               toolStats
	delegateTools       toolStats
	compactions         compactionStats
	delegateCompactions compactionStats
	statusCounts        map[string]int
}

type collectedSessionStats struct {
	state       Session
	userTurns   int
	modelTurns  int
	modelCalls  int
	retries     int
	tools       toolStats
	compactions compactionStats
}

type delegateStats struct {
	meta  ChildMeta
	stats collectedSessionStats
}

type toolStats struct {
	calls    int
	byName   map[string]int
	commands commandStats
	parallel parallelStats
}

type commandStats struct {
	calls      int
	foreground int
	background int
	shell      int
	argv       int
}

type parallelStats struct {
	batches int
	calls   int
	largest int
}

type compactionStats struct {
	runs         int
	messageCount int
	usage        llm.Usage
}

type compactionMeta struct {
	Time         time.Time `json:"time"`
	Usage        llm.Usage `json:"usage"`
	MessageCount int       `json:"message_count"`
	Input        string    `json:"input"`
	Summary      string    `json:"summary"`
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
		path:         dir,
		root:         root,
		delegates:    delegates,
		tools:        cloneToolStats(root.tools),
		compactions:  root.compactions,
		statusCounts: make(map[string]int),
	}
	for _, child := range delegates {
		report.tools.add(child.stats.tools)
		report.delegateTools.add(child.stats.tools)
		report.compactions.add(child.stats.compactions)
		report.delegateCompactions.add(child.stats.compactions)
		report.statusCounts[child.meta.Status]++
	}
	return report, nil
}

func collectSessionStats(dir string) (collectedSessionStats, error) {
	state, err := Load(dir)
	if err != nil {
		return collectedSessionStats{}, fmt.Errorf("load state in %s: %w", dir, err)
	}
	events, err := readEvents(dir)
	if err != nil {
		return collectedSessionStats{}, fmt.Errorf("read replay in %s: %w", dir, err)
	}
	tools, err := collectToolStats(events)
	if err != nil {
		return collectedSessionStats{}, fmt.Errorf("read tool activity in %s: %w", dir, err)
	}
	compactions, parallel, err := collectCompactionStats(dir, state.Messages)
	if err != nil {
		return collectedSessionStats{}, err
	}
	tools.parallel = parallel

	userTurns := make(map[int]struct{})
	modelTurns := make(map[[2]int]struct{})
	modelCalls := 0
	for _, ev := range events {
		switch ev.Type {
		case EventUser:
			if ev.Turn > 0 {
				userTurns[ev.Turn] = struct{}{}
			}
		case EventModelTurnUsage:
			modelCalls++
			modelTurns[[2]int{ev.Turn, ev.ModelTurns}] = struct{}{}
		}
	}
	retries := modelCalls - len(modelTurns)
	if retries < 0 {
		retries = 0
	}
	return collectedSessionStats{
		state:       state,
		userTurns:   len(userTurns),
		modelTurns:  len(modelTurns),
		modelCalls:  modelCalls,
		retries:     retries,
		tools:       tools,
		compactions: compactions,
	}, nil
}

func collectToolStats(events []Event) (toolStats, error) {
	stats := toolStats{byName: make(map[string]int)}
	for _, ev := range events {
		if ev.Type != EventToolStart {
			continue
		}
		stats.calls++
		stats.byName[ev.Tool]++
		if ev.Tool != "run_command" {
			continue
		}
		var input struct {
			Command    string   `json:"command"`
			Argv       []string `json:"argv"`
			Background bool     `json:"background"`
		}
		if err := json.Unmarshal(ev.Input, &input); err != nil {
			return toolStats{}, fmt.Errorf("decode run_command input for %s: %w", ev.ToolID, err)
		}
		stats.commands.calls++
		if input.Background {
			stats.commands.background++
		} else {
			stats.commands.foreground++
		}
		if input.Command != "" {
			stats.commands.shell++
		} else if len(input.Argv) != 0 {
			stats.commands.argv++
		}
	}
	return stats, nil
}

func collectCompactionStats(dir string, active []llm.Message) (compactionStats, parallelStats, error) {
	var compactions compactionStats
	var parallel parallelStats
	seenBatches := make(map[string]struct{})
	collectParallelBatches(active, seenBatches, &parallel)

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
		var meta compactionMeta
		if err := decodeJSONFile(metaPath, &meta, true); err != nil {
			return compactionStats{}, parallelStats{}, fmt.Errorf("decode compaction metadata %s: %w", metaPath, err)
		}
		var messages []llm.Message
		inputPath := filepath.Join(dir, meta.Input)
		if err := decodeJSONFile(inputPath, &messages, false); err != nil {
			return compactionStats{}, parallelStats{}, fmt.Errorf("decode compaction input %s: %w", inputPath, err)
		}
		compactions.runs++
		compactions.messageCount += meta.MessageCount
		compactions.usage = addUsage(compactions.usage, meta.Usage)
		collectParallelBatches(messages, seenBatches, &parallel)
	}
	return compactions, parallel, nil
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
	childrenDir := filepath.Join(rootDir, "children")
	entries, err := os.ReadDir(childrenDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read delegates in %s: %w", rootDir, err)
	}
	var delegates []*delegateStats
	seenIDs := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(childrenDir, entry.Name())
		metaPath := filepath.Join(dir, "meta.json")
		var meta ChildMeta
		if err := decodeJSONFile(metaPath, &meta, true); err != nil {
			return nil, fmt.Errorf("decode delegate metadata %s: %w", metaPath, err)
		}
		if meta.ID == "" {
			return nil, fmt.Errorf("decode delegate metadata %s: missing id", metaPath)
		}
		if _, ok := seenIDs[meta.ID]; ok {
			return nil, fmt.Errorf("decode delegate metadata %s: duplicate id %q", metaPath, meta.ID)
		}
		seenIDs[meta.ID] = struct{}{}
		stats, err := collectSessionStats(dir)
		if err != nil {
			return nil, fmt.Errorf("collect delegate %s: %w", meta.ID, err)
		}
		delegates = append(delegates, &delegateStats{meta: meta, stats: stats})
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
	stats.calls += other.calls
	for name, count := range other.byName {
		stats.byName[name] += count
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
	return clone
}

func (stats *commandStats) add(other commandStats) {
	stats.calls += other.calls
	stats.foreground += other.foreground
	stats.background += other.background
	stats.shell += other.shell
	stats.argv += other.argv
}

func (stats *parallelStats) add(other parallelStats) {
	stats.batches += other.batches
	stats.calls += other.calls
	stats.largest = max(stats.largest, other.largest)
}

func (stats *compactionStats) add(other compactionStats) {
	stats.runs += other.runs
	stats.messageCount += other.messageCount
	stats.usage = addUsage(stats.usage, other.usage)
}

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		CostUSD:          a.CostUSD + b.CostUSD,
		CostKnown:        a.CostKnown || b.CostKnown,
	}
}

func totalTokens(usage llm.Usage) int {
	return usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens + usage.OutputTokens + usage.ReasoningTokens
}

func writeStats(report statsReport, w io.Writer) error {
	var b strings.Builder
	writeSessionStats(&b, report)
	writeConversationStats(&b, report.root)
	writeOverallToolStats(&b, report)
	writeRootUsage(&b, report.root.state)
	writeOverallCompactions(&b, report)
	writeDelegates(&b, report)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeSessionStats(w io.Writer, report statsReport) {
	state := report.root.state
	fmt.Fprintln(w, "Session")
	fmt.Fprintf(w, "  path: %s\n", report.path)
	fmt.Fprintf(w, "  agent: %s\n", state.Agent)
	fmt.Fprintf(w, "  provider/model: %s/%s\n", state.Provider, state.Model)
	fmt.Fprintf(w, "  created: %s\n", state.Created.Format(time.RFC3339))
	fmt.Fprintf(w, "  updated: %s\n", state.Updated.Format(time.RFC3339))
	fmt.Fprintf(w, "  duration: %s\n", formatDuration(state.Updated.Sub(state.Created)))
}

func writeConversationStats(w io.Writer, stats collectedSessionStats) {
	fmt.Fprintln(w, "Conversation")
	writeConversationValues(w, "  ", stats)
}

func writeConversationValues(w io.Writer, indent string, stats collectedSessionStats) {
	fmt.Fprintf(w, "%suser turns: %d\n", indent, stats.userTurns)
	fmt.Fprintf(w, "%smodel turns: %d\n", indent, stats.modelTurns)
	fmt.Fprintf(w, "%smodel calls: %d\n", indent, stats.modelCalls)
	fmt.Fprintf(w, "%sretries: %d\n", indent, stats.retries)
	fmt.Fprintf(w, "%sactive messages: %d\n", indent, len(stats.state.Messages))
}

func writeOverallToolStats(w io.Writer, report statsReport) {
	root := report.root.tools
	delegates := report.delegateTools
	all := report.tools
	fmt.Fprintln(w, "Tools")
	writeSplitValue(w, "  ", "tool calls", all.calls, root.calls, delegates.calls)
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
	writeSplitValue(w, "  ", "parallel batches", all.parallel.batches, root.parallel.batches, delegates.parallel.batches)
	writeSplitValue(w, "  ", "parallel calls", all.parallel.calls, root.parallel.calls, delegates.parallel.calls)
	fmt.Fprintf(w, "  largest parallel batch: %d (root %d, delegates %d)\n", all.parallel.largest, root.parallel.largest, delegates.parallel.largest)
}

func writeSplitValue(w io.Writer, indent, label string, total, root, delegates int) {
	fmt.Fprintf(w, "%s%s: %d total (%d root, %d delegates)\n", indent, label, total, root, delegates)
}

func writeRootUsage(w io.Writer, state Session) {
	fmt.Fprintln(w, "Usage (includes delegates)")
	writeUsageValues(w, "  ", state.Usage.Usage, state.Usage.CostUSD)
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

func writeUsageValues(w io.Writer, indent string, usage llm.Usage, cost float64) {
	fmt.Fprintf(w, "%suncached input: %d\n", indent, usage.InputTokens)
	fmt.Fprintf(w, "%scache read: %d\n", indent, usage.CacheReadTokens)
	fmt.Fprintf(w, "%scache write: %d\n", indent, usage.CacheWriteTokens)
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
	if len(report.delegates) == 0 {
		return
	}

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
	if meta.ParentID != "" {
		fmt.Fprintf(w, "%sparent: %s\n", detail, meta.ParentID)
	}
	fmt.Fprintf(w, "%sagent: %s\n", detail, meta.Agent)
	fmt.Fprintf(w, "%sprovider/model: %s/%s\n", detail, meta.Provider, meta.Model)
	fmt.Fprintf(w, "%stask: %s\n", detail, oneLine(meta.TaskPreview))
	fmt.Fprintf(w, "%screated: %s\n", detail, meta.Created.Format(time.RFC3339))
	fmt.Fprintf(w, "%supdated: %s\n", detail, meta.Updated.Format(time.RFC3339))
	fmt.Fprintf(w, "%sduration: %s\n", detail, formatDuration(meta.Updated.Sub(meta.Created)))
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
