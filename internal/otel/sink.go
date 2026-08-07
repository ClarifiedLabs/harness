package otel

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"sync"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/tools"
)

var jsonUnmarshal = json.Unmarshal

// Sink observes agent and UI lifecycle events and records OTLP metrics.

type Sink struct {
	exp                 *Exporter
	delegate            bool
	registry            *tools.Registry
	sessionID           string
	provider            string
	model               string
	agentName           string
	parallelSeen        map[string]struct{}
	parallelLargest     int
	maintenanceAccepted map[string]int
	maintenanceRequests map[string]int
	mu                  sync.Mutex
}

func NewSink(exp *Exporter, registry *tools.Registry, provider, model, agentName string, delegate bool) *Sink {
	if exp == nil {
		return nil
	}
	return &Sink{exp: exp, registry: registry, provider: provider, model: model, agentName: agentName, delegate: delegate}
}

func (s *Sink) delegateLabel() string {
	if s.delegate {
		return "true"
	}
	return "false"
}

// SetIdentity updates the dynamic metric-point identity used after REPL
// model/agent switches and /clear session rotation. Identity-local dedup and
// maximum state resets so the new series starts independently.
func (s *Sink) SetIdentity(sessionID, provider, model, agentName string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID == sessionID && s.provider == provider && s.model == model && s.agentName == agentName {
		return
	}
	sessionChanged := s.sessionID != sessionID
	s.sessionID = sessionID
	s.provider = provider
	s.model = model
	s.agentName = agentName
	if sessionChanged {
		s.parallelSeen = nil
	}
	s.parallelLargest = 0
	s.maintenanceAccepted = nil
	s.maintenanceRequests = nil
}

func (s *Sink) baseAttrs(extra map[string]string) map[string]string {
	s.mu.Lock()
	sessionID, provider, model, agentName := s.sessionID, s.provider, s.model, s.agentName
	s.mu.Unlock()
	m := map[string]string{"delegate": s.delegateLabel()}
	if sessionID != "" {
		m["session_id"] = truncate(sessionID, 64)
	}
	if provider != "" {
		m["provider"] = truncate(provider, 64)
	}
	if model != "" {
		m["model"] = truncate(model, 128)
	}
	if agentName != "" {
		m["agent"] = truncate(agentName, 64)
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// ToolStart is not needed: tool accounting is via ToolResult + prompt/turn aggregates.

// ToolResult records per-tool metrics when tool name is unknown; it is a
// no-op placeholder because llm.ToolResult has no name. Use ToolResultWithName
// instead. It exists so Sink satisfies agent.EventSink when wrapped via a tee.
func (s *Sink) ToolResult(result llm.ToolResult) {
	_ = result
}

func (s *Sink) ToolResultWithName(toolName string, result llm.ToolResult, durationMS int64, activity tools.Activity) {
	if s == nil || s.exp == nil {
		return
	}
	tool := sanitizeToolName(toolName)
	ac := string(activity.Class)
	if ac == "" {
		ac = "other"
	}
	attrs := s.baseAttrs(map[string]string{"tool": tool, "activity_class": ac})
	s.exp.RecordSum("harness.tool.calls", "{call}", 1, attrs)
	if result.IsError {
		ek := string(result.ErrorKind)
		if ek == "" {
			ek = "result_error"
		}
		attrsErr := s.baseAttrs(map[string]string{"tool": tool, "activity_class": ac, "error_kind": truncate(ek, 64)})
		s.exp.RecordSum("harness.tool.errors", "{error}", 1, attrsErr)
	}
	if result.Truncated {
		s.exp.RecordSum("harness.tool.truncations", "{truncation}", 1, s.baseAttrs(map[string]string{"tool": tool}))
	}
	if durationMS >= 0 {
		s.exp.RecordHistogram("harness.tool.duration", "ms", float64(durationMS), s.baseAttrs(map[string]string{"tool": tool, "activity_class": ac}), []float64{1, 5, 10, 50, 100, 250, 500, 1000, 5000, 30000})
	}
	if result.ShownBytes > 0 || result.OriginalBytes > 0 {
		bytesVal := result.ShownBytes
		if bytesVal == 0 {
			bytesVal = result.OriginalBytes
		}
		s.exp.RecordHistogram("harness.tool.results.bytes", "By", float64(bytesVal), s.baseAttrs(map[string]string{"tool": tool}), []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576})
	}
	// Single-tool turn counters for parity with session stats.
	if tool == "update_todos" {
		// Will be de-duplicated at turn level when ToolCalls==1; keeping per-call here would overcount batched.
	}
}

func sanitizeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	known := map[string]bool{
		"read_file": true, "edit": true, "write_file": true, "apply_patch": true, "run_command": true, "search": true, "rg": true, "grep": true, "glob": true, "list_dir": true, "git_readonly": true, "delegate": true, "background_jobs": true, "update_todos": true, "request_implementation": true, "view_image": true, "web_fetch": true,
	}
	if known[name] {
		return truncate(name, 64)
	}
	if strings.HasPrefix(name, "mcp_") || strings.HasPrefix(name, "lsp_") {
		return truncate(name, 64)
	}
	return "other"
}

func sanitizeStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "completed", "failed", "canceled", "abandoned", "running":
		return status
	default:
		return "unknown"
	}
}

func sanitizeTerminationReason(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch reason {
	case "model_completed", "turn_limit", "token_limit", "cost_limit", "repeat_guard", "error_guard", "cancelled", "error", "unknown":
		return reason
	case "":
		return "unknown"
	default:
		return "unknown"
	}
}

func isSoloTodoTurn(toolNames []string) bool {
	normalized := make([]string, 0, len(toolNames))
	for _, n := range toolNames {
		normalized = append(normalized, sanitizeToolName(n))
	}
	return len(normalized) == 1 && normalized[0] == "update_todos"
}
func isSingleInspectTurn(toolNames []string) bool {
	normalized := make([]string, 0, len(toolNames))
	for _, n := range toolNames {
		normalized = append(normalized, sanitizeToolName(n))
	}
	if len(normalized) != 1 {
		return false
	}
	switch normalized[0] {
	case "read_file", "search", "rg", "grep", "glob", "list_dir", "git_readonly":
		return true
	default:
		return false
	}
}

// TurnProgress records inspection/steer metrics.
func (s *Sink) TurnProgress(p agent.TurnProgress) {
	if s == nil || s.exp == nil {
		return
	}
	ac := dominantActivity(p.Activity)
	if p.ToolCalls > 0 {
		s.exp.RecordHistogram("harness.tools_per_turn", "{tool}", float64(p.ToolCalls), s.baseAttrs(map[string]string{"activity_class": ac}), []float64{1, 2, 3, 4, 8, 16})
	}
	if p.Operations > 0 {
		s.exp.RecordHistogram("harness.operations_per_turn", "{operation}", float64(p.Operations), s.baseAttrs(map[string]string{"activity_class": ac}), []float64{1, 2, 3, 4, 8, 16, 32})
	}
	if p.BatchedOperationCount > 0 {
		s.exp.RecordSum("harness.batched_operations", "{operation}", int64(p.BatchedOperationCount), s.baseAttrs(map[string]string{"tool": "read_file"}))
	}
	if p.SingleLookupCount == 1 && p.ToolCalls == 1 {
		s.exp.RecordSum("harness.single_lookup_turns", "{turn}", 1, s.baseAttrs(nil))
	}
	if p.InspectionNoProgressRun > 0 {
		s.exp.RecordHistogram("harness.inspection_no_progress_streak", "{turn}", float64(p.InspectionNoProgressRun), s.baseAttrs(nil), []float64{1, 2, 3, 5, 8, 12, 20})
	}
	if p.SteerReason != "" {
		s.exp.RecordSum("harness.guard.steers", "{steer}", 1, s.baseAttrs(map[string]string{"reason": string(p.SteerReason)}))
	}
	// Solo-todo and inspect-only single-lookup turns for stats parity.
	if p.ToolCalls == 1 {
		// We don't have tool name here, but TurnProgress alone can't tell update_todos vs inspect.
		// Solo-todo is tracked in ToolResultWithName fallback; largest_batch via RecordParallel.
	}
}

// TurnComplete currently delegates to TurnProgress for the common batch/steer signals;
// it remains a stable hook for per-turn gauges added in later plan slices.

func dominantActivity(a agent.ToolActivityCounts) string {
	max := a.Inspect
	dom := "inspect"
	if a.Mutate > max {
		max = a.Mutate
		dom = "mutate"
	}
	if a.Verify > max {
		max = a.Verify
		dom = "verify"
	}
	if a.Wait > max {
		max = a.Wait
		dom = "wait"
	}
	if a.Coordinate > max {
		max = a.Coordinate
		dom = "coordinate"
	}
	if a.Other > max {
		dom = "other"
	}
	return dom
}

// TurnComplete records tools/operations and batch counters per turn.
func (s *Sink) TurnComplete(usage agent.TurnUsage) {
	if s == nil || s.exp == nil {
		return
	}
	// The TurnProgress already covered most; TurnComplete is just a hook for future use.
	_ = usage
}

// PromptComplete records prompt-level fleet metrics.
func (s *Sink) PromptComplete(usage agent.PromptUsage, duration time.Duration) {
	if s == nil || s.exp == nil {
		return
	}
	term := sanitizeTerminationReason(string(usage.TerminationReason))
	closure := strings.TrimSpace(string(usage.ClosureTrigger))
	if closure != "" {
		closure = truncate(closure, 32)
	}
	attrs := s.baseAttrs(map[string]string{"termination_reason": truncate(term, 64), "closure_trigger": closure})
	s.exp.RecordSum("harness.prompt.total", "{prompt}", 1, attrs)
	s.exp.RecordHistogram("harness.prompt.turns", "{turn}", float64(usage.Turns), attrs, []float64{1, 2, 3, 5, 10, 20, 50})
	if duration > 0 {
		s.exp.RecordHistogram("harness.prompt.duration", "s", duration.Seconds(), attrs, []float64{1, 5, 10, 30, 60, 120, 300, 600})
	}
	u := usage.Usage
	costKnown := "false"
	if u.CostKnown {
		costKnown = "true"
	}
	tokAttrs := s.baseAttrs(map[string]string{"cost_known": costKnown})
	s.exp.RecordSum("harness.tokens.input", "{token}", int64(u.InputTokens), tokAttrs)
	s.exp.RecordSum("harness.tokens.output", "{token}", int64(u.OutputTokens), tokAttrs)
	s.exp.RecordSum("harness.tokens.cache_read", "{token}", int64(u.CacheReadTokens), tokAttrs)
	s.exp.RecordSum("harness.tokens.cache_write", "{token}", int64(u.CacheWriteTokens), tokAttrs)
	s.exp.RecordSum("harness.tokens.cache_write_1h", "{token}", int64(u.CacheWrite1hTokens), tokAttrs)
	s.exp.RecordSum("harness.tokens.reasoning", "{token}", int64(u.ReasoningTokens), tokAttrs)
	total := u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.CacheWrite1hTokens + u.ReasoningTokens
	s.exp.RecordSum("harness.tokens.total", "{token}", int64(total), tokAttrs)
	if u.CostKnown {
		s.exp.RecordSumFloat("harness.cost.usd", "USD", u.CostUSD, s.baseAttrs(map[string]string{"cost_known": "true"}))
	} else {
		s.exp.RecordSum("harness.cost.unpriced_calls", "{call}", 1, s.baseAttrs(nil))
	}
	s.exp.RecordSum("harness.compactions.total", "{compaction}", int64(usage.Compactions), s.baseAttrs(nil))
	// Wasted (retried) token cost
	if w := usage.Wasted; w.InputTokens > 0 || w.OutputTokens > 0 || w.CacheReadTokens > 0 || w.CacheWriteTokens > 0 {
		s.exp.RecordSum("harness.retries.total", "{retry}", 1, s.baseAttrs(nil))
	}
}

// MaintenanceComplete records a maintenance model call when no accepted
// lifecycle event already accounted for it.
func (s *Sink) MaintenanceComplete(usage agent.MaintenanceUsage) {
	if s == nil || s.exp == nil {
		return
	}
	purpose := string(llm.NormalizeRequestPurpose(llm.RequestPurpose(usage.Purpose)))
	s.mu.Lock()
	if s.maintenanceRequests[purpose] > 0 {
		s.maintenanceRequests[purpose]--
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.exp.RecordSum("harness.model.requests", "{request}", 1, s.baseAttrs(map[string]string{"purpose": truncate(purpose, 32)}))
}

func (s *Sink) RecordParallel(batches [][]string) {
	if s == nil || s.exp == nil || len(batches) == 0 {
		return
	}
	s.mu.Lock()
	if s.parallelSeen == nil {
		s.parallelSeen = make(map[string]struct{})
	}
	var toRecord [][]string
	largest := s.parallelLargest
	for _, ids := range batches {
		if len(ids) < 2 {
			continue
		}
		key := strings.Join(ids, ",")
		if _, ok := s.parallelSeen[key]; ok {
			continue
		}
		s.parallelSeen[key] = struct{}{}
		toRecord = append(toRecord, ids)
		if len(ids) > largest {
			largest = len(ids)
		}
	}
	largestChanged := largest > s.parallelLargest
	if largestChanged {
		s.parallelLargest = largest
	}
	s.mu.Unlock()
	for _, ids := range toRecord {
		size := len(ids)
		s.exp.RecordSum("harness.parallel.batches", "{batch}", 1, s.baseAttrs(nil))
		s.exp.RecordSum("harness.parallel.calls", "{call}", int64(size), s.baseAttrs(nil))
		s.exp.RecordHistogram("harness.parallel.batch_size", "{call}", float64(size), s.baseAttrs(nil), []float64{2, 3, 4, 6, 8, 12})
	}
	if largestChanged {
		s.exp.RecordGauge("harness.parallel.largest_batch", "{call}", int64(largest), s.baseAttrs(nil))
	}
}

// RecordCommands records harness.commands.* from a decoded run_command payload.
func (s *Sink) RecordCommands(input []byte) {
	if s == nil || s.exp == nil || len(input) == 0 {
		return
	}
	type step struct {
		Command string   `json:"command"`
		Argv    []string `json:"argv"`
	}
	type payload struct {
		Command    string   `json:"command"`
		Argv       []string `json:"argv"`
		Background bool     `json:"background"`
		Steps      []step   `json:"steps"`
	}
	var p payload
	if err := jsonUnmarshal(input, &p); err != nil {
		return
	}
	mode := "foreground"
	if p.Background {
		mode = "background"
	}
	kind := "shell"
	if p.Command == "" && len(p.Argv) > 0 {
		kind = "argv"
	}
	s.exp.RecordSum("harness.commands.total", "{command}", 1, s.baseAttrs(map[string]string{"mode": mode, "kind": kind}))
	if len(p.Steps) > 0 {
		s.exp.RecordHistogram("harness.commands.steps_per_batch", "{step}", float64(len(p.Steps)), s.baseAttrs(nil), []float64{1, 2, 3, 5, 8})
	}
}

// RecordSearch records harness.search.* bounded-result signals.
func (s *Sink) RecordSearch(tool string, display string, metrics map[string]int) {
	if s == nil || s.exp == nil {
		return
	}
	tool = sanitizeToolName(tool)
	if tool != "search" && tool != "rg" && tool != "grep" {
		return
	}
	if strings.Contains(strings.ToLower(display), "no matches") {
		s.exp.RecordSum("harness.search.no_matches", "{search}", 1, s.baseAttrs(map[string]string{"tool": tool}))
	}
	if metrics != nil && (metrics["results_bounded"] != 0 || metrics["context_bounded"] != 0) {
		s.exp.RecordSum("harness.search.bounded_calls", "{search}", 1, s.baseAttrs(nil))
	}
	if metrics != nil && metrics["search_batch_calls"] > 0 {
		s.exp.RecordSum("harness.search.batch.calls", "{search}", 1, s.baseAttrs(nil))
	}
}

// RecordSkill records harness.skill.activations.
func (s *Sink) RecordSkill(source, status string) {
	if s == nil || s.exp == nil {
		return
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	s.exp.RecordSum("harness.skill.activations", "{activation}", 1, s.baseAttrs(map[string]string{"source": truncate(source, 32)}))
	_ = status
}

func (s *Sink) RecordTurnSummary(toolNames []string) {
	if s == nil || s.exp == nil {
		return
	}
	if len(toolNames) == 0 {
		return
	}
	names := make([]string, 0, len(toolNames))
	for _, n := range toolNames {
		sanitized := sanitizeToolName(n)
		if sanitized == "other" {
			// Preserve unknown as "other" bucket but keep count correct
		}
		names = append(names, sanitized)
	}
	if isSoloTodoTurn(names) {
		s.exp.RecordSum("harness.solo_todo_turns", "{turn}", 1, s.baseAttrs(nil))
	}
	if isSingleInspectTurn(names) {
		s.exp.RecordSum("harness.single_inspect_turns", "{turn}", 1, s.baseAttrs(nil))
	}
}

// RecordSession emits harness.session.* gauges at session-exit.
func (s *Sink) RecordSession(costUSD float64, totalTokens int) {
	if s == nil || s.exp == nil {
		return
	}
	s.exp.RecordGaugeFloat("harness.session.cost", "USD", costUSD, s.baseAttrs(nil))
	s.exp.RecordGauge("harness.session.tokens", "{token}", int64(totalTokens), s.baseAttrs(nil))
}

// RecordDelegate records harness.delegate.* for one ChildMeta.
func (s *Sink) RecordDelegate(agentName, status, terminationReason string, turns int, usage llm.Usage, compactions int) {
	if s == nil || s.exp == nil {
		return
	}
	agentName = truncate(strings.TrimSpace(agentName), 64)
	if agentName == "" {
		agentName = "unknown"
	}
	status = sanitizeStatus(status)
	term := sanitizeTerminationReason(terminationReason)
	attrs := map[string]string{"agent": agentName, "delegate": "true", "status": status, "termination_reason": term}
	s.exp.RecordSum("harness.delegate.sessions", "{session}", 1, s.baseAttrs(attrs))
	delegateAttrs := map[string]string{"agent": agentName, "delegate": "true", "status": status}
	s.exp.RecordHistogram("harness.delegate.turns", "{turn}", float64(turns), s.baseAttrs(delegateAttrs), []float64{1, 2, 5, 10, 20})
	totalTokens := usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens + usage.CacheWrite1hTokens + usage.ReasoningTokens
	s.exp.RecordSum("harness.delegate.tokens", "{token}", int64(totalTokens), s.baseAttrs(delegateAttrs))
	if usage.CostKnown {
		costAttrs := maps.Clone(delegateAttrs)
		costAttrs["cost_known"] = "true"
		s.exp.RecordSumFloat("harness.delegate.cost", "USD", usage.CostUSD, s.baseAttrs(costAttrs))
	}
	if compactions > 0 {
		s.exp.RecordSum("harness.delegate.compactions", "{compaction}", int64(compactions), s.baseAttrs(map[string]string{"agent": agentName, "delegate": "true"}))
	}
}

type ContextComposition struct {
	Messages             int
	Blocks               int
	UserTextBytes        int
	AssistantTextBytes   int
	ToolInputBytes       int
	ToolResultBytes      int
	ReasoningTextBytes   int
	ReasoningOpaqueBytes int
	ImageEncodedBytes    int
}

func (s *Sink) RecordContext(c ContextComposition) {
	if s == nil || s.exp == nil {
		return
	}
	s.exp.RecordGauge("harness.context.messages", "{message}", int64(c.Messages), s.baseAttrs(nil))
	s.exp.RecordGauge("harness.context.blocks", "{block}", int64(c.Blocks), s.baseAttrs(nil))
	by := func(x int) int64 { return int64(x) }
	s.exp.RecordGauge("harness.context.bytes", "By", by(c.UserTextBytes), s.baseAttrs(map[string]string{"component": "user_text"}))
	s.exp.RecordGauge("harness.context.bytes", "By", by(c.AssistantTextBytes), s.baseAttrs(map[string]string{"component": "assistant_text"}))
	s.exp.RecordGauge("harness.context.bytes", "By", by(c.ToolInputBytes), s.baseAttrs(map[string]string{"component": "tool_input"}))
	s.exp.RecordGauge("harness.context.bytes", "By", by(c.ToolResultBytes), s.baseAttrs(map[string]string{"component": "tool_result"}))
	s.exp.RecordGauge("harness.context.bytes", "By", by(c.ReasoningTextBytes), s.baseAttrs(map[string]string{"component": "reasoning_text"}))
	s.exp.RecordGauge("harness.context.bytes", "By", by(c.ReasoningOpaqueBytes), s.baseAttrs(map[string]string{"component": "reasoning_opaque"}))
	s.exp.RecordGauge("harness.context.bytes", "By", by(c.ImageEncodedBytes), s.baseAttrs(map[string]string{"component": "image"}))
}

// RetentionApplied records retention epochs.
func (s *Sink) RetentionApplied(event agent.RetentionEvent) {
	if s == nil || s.exp == nil {
		return
	}
	policy := event.Policy
	if policy == "" {
		policy = "unknown"
	}
	trigger := event.Trigger
	if trigger == "" {
		trigger = "unknown"
	}
	s.exp.RecordSum("harness.retention.epochs", "{epoch}", 1, s.baseAttrs(map[string]string{"policy": truncate(policy, 32), "trigger": truncate(trigger, 32)}))
}

// ModelRequestEvent records each request once at acceptance and records only
// terminal lifecycle failures as request errors.
func (s *Sink) ModelRequestEvent(event llm.ModelRequestEvent) {
	if s == nil || s.exp == nil {
		return
	}
	purpose := string(llm.NormalizeRequestPurpose(event.Purpose))
	maintenancePurpose := purpose != string(llm.RequestPurposeTurn)
	if event.State == llm.ModelRequestAccepted {
		if maintenancePurpose {
			s.mu.Lock()
			if s.maintenanceAccepted == nil {
				s.maintenanceAccepted = make(map[string]int)
			}
			s.maintenanceAccepted[purpose]++
			s.mu.Unlock()
		}
		s.exp.RecordSum("harness.model.requests", "{request}", 1, s.baseAttrs(map[string]string{"purpose": truncate(purpose, 32), "api_type": truncate(event.APIType, 32)}))
	}

	terminalError := event.State == llm.ModelRequestFailed || event.State == llm.ModelRequestCancelled ||
		(event.State == llm.ModelRequestUpstreamAttemptFailed && event.Outcome == llm.ModelRequestOutcomeTerminal)
	if maintenancePurpose && (event.State == llm.ModelRequestCompleted || terminalError) {
		s.mu.Lock()
		if s.maintenanceAccepted[purpose] > 0 {
			s.maintenanceAccepted[purpose]--
			if event.State == llm.ModelRequestCompleted {
				if s.maintenanceRequests == nil {
					s.maintenanceRequests = make(map[string]int)
				}
				s.maintenanceRequests[purpose]++
			}
		}
		s.mu.Unlock()
	}
	if terminalError {
		s.exp.RecordSum("harness.model.request.errors", "{error}", 1, s.baseAttrs(map[string]string{
			"stage": truncate(string(event.Stage), 32),
			"code":  truncate(event.Code, 64),
			"state": truncate(string(event.State), 32),
		}))
	}
}

func (s *Sink) Flush(ctx context.Context) error {
	if s == nil || s.exp == nil {
		return nil
	}
	return s.exp.Export(ctx)
}

func (s *Sink) FlushAsync() {
	if s == nil || s.exp == nil {
		return
	}
	exp := s.exp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exp.Export(ctx)
	}()
}

func (s *Sink) Exporter() *Exporter {
	if s == nil {
		return nil
	}
	return s.exp
}

// Ensure Sink implements optional agent sinks for static checks.
var _ agent.TurnProgressSink = (*Sink)(nil)
var _ agent.RetentionEventSink = (*Sink)(nil)
var _ agent.MaintenanceSink = (*Sink)(nil)
var _ agent.ModelRequestEventSink = (*Sink)(nil)
