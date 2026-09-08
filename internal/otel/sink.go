package otel

import (
	"context"
	"strings"
	"sync"
	"time"

	"harness/internal/agent"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/skills"
	"harness/internal/tools"
)

// Sink observes agent and UI lifecycle events and records OTLP metrics.

type Sink struct {
	exp             *Exporter
	delegate        bool
	registry        *tools.Registry
	workGroup       *execution.Group
	sessionID       string
	provider        string
	model           string
	agentName       string
	sessionRecorded bool
	mu              sync.Mutex
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
// model/agent switches and /clear session rotation. Session completion dedup
// resets only for a new session; the session ID never leaves this sink.
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
		s.sessionRecorded = false
	}
}

func (s *Sink) baseAttrs(extra map[string]string) map[string]string {
	s.mu.Lock()
	provider, model, agentName := s.provider, s.model, s.agentName
	s.mu.Unlock()
	m := map[string]string{"delegate": s.delegateLabel()}
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

// ToolResultWithName is a compatibility adapter. Worker duration belongs only
// to WorkFinish; a returned timeout does not mean its worker has finished.
func (s *Sink) ToolResultWithName(toolName string, result llm.ToolResult, durationMS int64, activity tools.Activity) {
	if s == nil {
		return
	}
	outcome := "completed"
	if result.IsError {
		outcome = "failed"
	}
	e := execution.WorkEvent{Identity: s.Scope().Identity, Kind: execution.WorkTool, Phase: execution.WorkResult, Tool: toolName, Outcome: outcome, Activity: string(activity.Class), ErrorKind: string(result.ErrorKind), Count: 1, ResultBytes: max(result.ShownBytes, len(result.Text)), OriginalBytes: result.OriginalBytes, Truncated: result.Truncated}
	if result.BackgroundJobID == "" {
		e.Metrics = result.Metrics
	}
	s.ObserveWork(e)
}

func sanitizeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	known := map[string]bool{
		"read": true, "view_image": true, "edit": true, "write": true, "shell": true, "web_fetch": true, "delegate": true, "background_jobs": true, "update_todos": true, "record_plan": true, "agent_sessions": true, "acp": true, "tool_catalog": true, "task_notes": true, "history_search": true, "history_read": true, "history_list": true, "get_context_remaining": true, "new_context": true,
	}
	if known[name] {
		return truncate(name, 64)
	}
	if strings.HasPrefix(name, "mcp_") {
		return "mcp"
	}
	if strings.HasPrefix(name, "lsp_") {
		return "lsp"
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
	case "read", "view_image", "web_fetch":
		return true
	default:
		return false
	}
}

// TurnProgress is a compatibility adapter. Production turn observations arrive
// from the core with their captured execution identity.
func (s *Sink) TurnProgress(p agent.TurnProgress) {
	if s == nil {
		return
	}
	s.ObserveTurn(execution.TurnEvent{Identity: s.Scope().Identity, ToolCalls: p.ToolCalls, Operations: p.Operations, SingleLookupCount: p.SingleLookupCount, InspectionNoProgressRun: p.InspectionNoProgressRun, Activity: dominantActivity(p.Activity), SteerReason: string(p.SteerReason)})
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

// PromptComplete is a non-billing compatibility summary. Source observations
// own all exclusive requests, usage, retries, and applied compactions.
func (s *Sink) PromptComplete(usage agent.PromptUsage, duration time.Duration) {
	if s == nil {
		return
	}
	s.ObservePrompt(execution.PromptEvent{Identity: s.Scope().Identity, Duration: duration, Turns: usage.Turns, Termination: string(usage.TerminationReason), ClosureTrigger: string(usage.ClosureTrigger)})
}

// MaintenanceComplete and ModelRequestEvent are legacy diagnostics, not physical
// source observations. They cannot safely count or price model requests.
func (s *Sink) MaintenanceComplete(agent.MaintenanceUsage) {}

// RecordParallel does not reconstruct physical execution from transcripts.
// WorkParallel supplies batch count and final actual size directly.
func (s *Sink) RecordParallel([][]string) {}

// RecordCommands no longer reconstructs execution from a launch payload.
// WorkCommand observations count only commands actually executed.
func (s *Sink) RecordCommands([]byte) {}

// RecordSkillCatalog records startup catalog budget pressure, not an activation.
func (s *Sink) RecordSkillCatalog(report skills.CatalogReport) {
	if s == nil {
		return
	}
	s.ObserveSkill(execution.SkillEvent{Identity: s.Scope().Identity, Source: "startup", Status: "catalog", Omitted: report.Omitted, Truncated: report.TruncatedCount})
}

// RecordSkill is the compatibility entry point for actual root UI injections.
func (s *Sink) RecordSkill(source, status string) {
	if s == nil {
		return
	}
	s.ObserveSkill(execution.SkillEvent{Identity: s.Scope().Identity, Source: source, Status: status})
}

func (s *Sink) RecordTurnSummary(toolNames []string) {
	if s == nil {
		return
	}
	s.ObserveTurn(execution.TurnEvent{Identity: s.Scope().Identity, ToolNames: toolNames})
}

// RecordSession records inclusive root-session distributions, never fleet
// billing. A session may span many configured identities, so none is attached.
func (s *Sink) RecordSession(costUSD float64, totalTokens int) {
	if s == nil || s.exp == nil || s.delegate {
		return
	}
	attrs := map[string]string{"scope": "root_session_inclusive", "delegate": "false"}
	s.mu.Lock()
	if s.sessionRecorded {
		s.mu.Unlock()
		return
	}
	s.sessionRecorded = true
	s.mu.Unlock()
	s.exp.RecordSum("harness.session.total", "{session}", 1, attrs)
	s.exp.RecordHistogram("harness.session.cost", "USD", costUSD, attrs, []float64{0, .01, .1, 1, 5, 10, 50, 100})
	s.exp.RecordHistogram("harness.session.tokens", "{token}", float64(max(0, totalTokens)), attrs, tokenBounds)
}

// RecordDelegate deliberately ignores inclusive child metadata. Child execution
// observations carry the actual model and exclusive billing instead.
func (s *Sink) RecordDelegate(string, string, string, int, llm.Usage, int) {}

// ContextComposition remains an alias for compatibility; the execution
// contract is the canonical owner of the numeric request snapshot.
type ContextComposition = execution.ContextComposition

// RecordContext is a compatibility adapter. Production request snapshots arrive
// through ObserveContext with the caller's captured identity.
func (s *Sink) RecordContext(c ContextComposition) {
	if s == nil || s.exp == nil {
		return
	}
	s.recordContextComposition(s.Scope().Identity, c)
}

// RetentionApplied is retained for interface compatibility. ObserveContext is
// the canonical retention observation, including actual reclamation and resets.
func (s *Sink) RetentionApplied(agent.RetentionEvent)   {}
func (s *Sink) ModelRequestEvent(llm.ModelRequestEvent) {}

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
		ctx, cancel := context.WithTimeout(context.Background(), DefaultExportTimeout)
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
