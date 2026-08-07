package otel

import (
	"context"
	"strings"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/tools"
)

// Sink is a decorator that observes agent events and records OTLP metrics.
// It implements the minimal agent sink interfaces via type-asserted optional methods.

type Sink struct {
	exp       *Exporter
	delegate bool
	registry  *tools.Registry // optional, for activity class
	provider  string
	model     string
	agentName string
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

func (s *Sink) baseAttrs(extra map[string]string) map[string]string {
	m := map[string]string{
		"provider": truncate(s.provider, 64),
		"model":    truncate(s.model, 128),
		"agent":    truncate(s.agentName, 64),
		"delegate": s.delegateLabel(),
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
	// Duration histogram
	if durationMS >= 0 {
		s.exp.RecordHistogram("harness.tool.duration", "ms", float64(durationMS), s.baseAttrs(map[string]string{"tool": tool, "activity_class": ac}), []float64{1, 5, 10, 50, 100, 250, 500, 1000, 5000, 30000})
	}
	// Bytes histogram (shown bytes is user-visible, original is total)
	if result.ShownBytes > 0 || result.OriginalBytes > 0 {
		bytesVal := result.ShownBytes
		if bytesVal == 0 {
			bytesVal = result.OriginalBytes
		}
		s.exp.RecordHistogram("harness.tool.results.bytes", "By", float64(bytesVal), s.baseAttrs(map[string]string{"tool": tool}), []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576})
	}
}

func sanitizeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	// Allowlist bounded set: known builtins plus mcp_*/lsp_* prefix
	known := map[string]bool{
		"read_file": true, "edit": true, "write_file": true, "apply_patch": true, "run_command": true, "search": true, "rg": true, "grep": true, "glob": true, "list_dir": true, "git_readonly": true, "delegate": true, "background_jobs": true, "update_todos": true, "request_implementation": true, "view_image": true, "web_fetch": true,
	}
	if known[name] {
		return truncate(name, 64)
	}
	if strings.HasPrefix(name, "mcp_") || strings.HasPrefix(name, "lsp_") {
		return truncate(name, 64)
	}
	// Unknown: bucket as other to bound cardinality
	return "other"
}

// TurnProgress records inspection/steer metrics.
func (s *Sink) TurnProgress(p agent.TurnProgress) {
	if s == nil || s.exp == nil {
		return
	}
	// Tools/ops per turn histograms
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
	// Inspection streak histogram (only when >0)
	if p.InspectionNoProgressRun > 0 {
		s.exp.RecordHistogram("harness.inspection_no_progress_streak", "{turn}", float64(p.InspectionNoProgressRun), s.baseAttrs(nil), []float64{1, 2, 3, 5, 8, 12, 20})
	}
	if p.SteerReason != "" {
		s.exp.RecordSum("harness.guard.steers", "{steer}", 1, s.baseAttrs(map[string]string{"reason": string(p.SteerReason)}))
	}
}

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
	term := string(usage.TerminationReason)
	if term == "" {
		term = "unknown"
	}
	closure := string(usage.ClosureTrigger)
	attrs := s.baseAttrs(map[string]string{"termination_reason": truncate(term, 64), "closure_trigger": truncate(closure, 64)})
	s.exp.RecordSum("harness.prompt.total", "{prompt}", 1, attrs)
	s.exp.RecordHistogram("harness.prompt.turns", "{turn}", float64(usage.Turns), attrs, []float64{1, 2, 3, 5, 10, 20, 50})
	if duration > 0 {
		s.exp.RecordHistogram("harness.prompt.duration", "s", duration.Seconds(), attrs, []float64{1, 5, 10, 30, 60, 120, 300, 600})
	}
	// Tokens
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
	if usage.Maintenance.InputTokens != 0 || usage.Maintenance.OutputTokens != 0 {
		// Maintenance is already included in Usage, but track separately? plan says maintenanceCalls via compactionStats; we'll just record compactions already
	}
}

// MaintenanceComplete records a maintenance model call (compaction etc).
func (s *Sink) MaintenanceComplete(usage agent.MaintenanceUsage) {
	if s == nil || s.exp == nil {
		return
	}
	s.exp.RecordSum("harness.model.requests", "{request}", 1, s.baseAttrs(map[string]string{"purpose": truncate(string(llm.NormalizeRequestPurpose(llm.RequestPurpose(usage.Purpose))), 32)}))
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

// ModelRequestEvent records request mix and errors.
func (s *Sink) ModelRequestEvent(event llm.ModelRequestEvent) {
	if s == nil || s.exp == nil {
		return
	}
	purpose := string(llm.NormalizeRequestPurpose(event.Purpose))
	s.exp.RecordSum("harness.model.requests", "{request}", 1, s.baseAttrs(map[string]string{"purpose": truncate(purpose, 32), "api_type": truncate(event.APIType, 32)}))
	if event.State == llm.ModelRequestFailed {
		s.exp.RecordSum("harness.model.request.errors", "{error}", 1, s.baseAttrs(map[string]string{"stage": truncate(string(event.Stage), 32), "code": truncate(event.Code, 64)}))
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

// Ensure Sink implements optional agent sinks for static checks.
var _ agent.TurnProgressSink = (*Sink)(nil)
var _ agent.RetentionEventSink = (*Sink)(nil)
var _ agent.MaintenanceSink = (*Sink)(nil)
var _ agent.ModelRequestEventSink = (*Sink)(nil)
