package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/hooks"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/sse"
	"harness/internal/tools"
)

const agentOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

// recordSink captures every sink callback so tests can assert what the UI would
// have been told.
type recordSink struct {
	text            strings.Builder
	attemptStarts   []turnAttemptEvent
	contexts        []ContextEstimate
	attemptUsage    []TurnAttemptUsage
	reasoning       []string
	phases          []string
	toolUses        []llm.ToolCall
	argDeltas       []string
	starts          []llm.ToolCall
	results         []llm.ToolResult
	abandoned       []turnAttemptEvent
	notices         []string
	promptUsage     []PromptUsage
	completedTurns  []TurnUsage
	turnCounts      []int
	maintenance     []MaintenanceUsage
	retention       []RetentionEvent
	progress        []TurnProgress
	hookDiagnostics []hooks.Diagnostic
}

type turnAttemptEvent struct {
	turn    int
	attempt int
}

type diagnosticRecordSink struct {
	recordSink
	diagnostics []ModelErrorDiagnostic
}

type modelRequestRecordSink struct {
	recordSink
	events []llm.ModelRequestEvent
}

type workflowRecordSink struct {
	recordSink
	status   WorkflowStatus
	closures []ClosureEvent
}

func (s *workflowRecordSink) WorkflowStatus() WorkflowStatus { return s.status }
func (s *workflowRecordSink) ClosureStarted(event ClosureEvent) {
	s.closures = append(s.closures, event)
}

func (s *modelRequestRecordSink) ModelRequestEvent(event llm.ModelRequestEvent) {
	s.events = append(s.events, event)
}

func (s *diagnosticRecordSink) ModelErrorDiagnostic(diagnostic ModelErrorDiagnostic) {
	s.diagnostics = append(s.diagnostics, diagnostic)
}

func (s *recordSink) TextDelta(t string) { s.text.WriteString(t) }
func (s *recordSink) ReasoningSummary(t string) {
	s.reasoning = append(s.reasoning, t)
}
func (s *recordSink) AssistantPhase(phase string) {
	s.phases = append(s.phases, phase)
}
func (s *recordSink) TurnAttemptStart(turn, attempt int, ctx ContextEstimate) {
	s.attemptStarts = append(s.attemptStarts, turnAttemptEvent{turn: turn, attempt: attempt})
	s.contexts = append(s.contexts, ctx)
}
func (s *recordSink) TurnAttemptComplete(u TurnAttemptUsage) {
	s.attemptUsage = append(s.attemptUsage, u)
}
func (s *recordSink) TurnAttemptAbandoned(turn, attempt int) {
	s.abandoned = append(s.abandoned, turnAttemptEvent{turn: turn, attempt: attempt})
}
func (s *recordSink) TurnComplete(u TurnUsage) {
	s.completedTurns = append(s.completedTurns, u)
}
func (s *recordSink) ToolUseStart(c llm.ToolCall) { s.toolUses = append(s.toolUses, c) }
func (s *recordSink) ToolUseDelta(_ int, delta string) {
	s.argDeltas = append(s.argDeltas, delta)
}
func (s *recordSink) ToolStart(c llm.ToolCall)    { s.starts = append(s.starts, c) }
func (s *recordSink) ToolResult(r llm.ToolResult) { s.results = append(s.results, r) }
func (s *recordSink) Notice(msg string)           { s.notices = append(s.notices, msg) }
func (s *recordSink) PromptComplete(u PromptUsage) {
	s.promptUsage = append(s.promptUsage, u)
	s.turnCounts = append(s.turnCounts, u.Turns)
}
func (s *recordSink) RetentionApplied(event RetentionEvent) {
	s.retention = append(s.retention, event)
}
func (s *recordSink) TurnProgress(progress TurnProgress) {
	s.progress = append(s.progress, progress)
}
func (s *recordSink) HookDiagnostic(diagnostic hooks.Diagnostic) {
	s.hookDiagnostics = append(s.hookDiagnostics, diagnostic)
}
func (s *recordSink) MaintenanceComplete(u MaintenanceUsage) {
	s.maintenance = append(s.maintenance, u)
}

func assertPromptTermination(t *testing.T, sink *recordSink, want TerminationReason) {
	t.Helper()
	if len(sink.promptUsage) != 1 {
		t.Fatalf("PromptComplete calls = %d, want 1", len(sink.promptUsage))
	}
	if got := sink.promptUsage[0].TerminationReason; got != want {
		t.Fatalf("termination reason = %q, want %q", got, want)
	}
}

func TestReportMaintenanceSkipsLocalZeroUsage(t *testing.T) {
	sink := &recordSink{}
	reportMaintenance(sink, "compaction", llm.Usage{})
	if len(sink.maintenance) != 0 {
		t.Fatalf("zero-usage local compaction recorded as a model call: %+v", sink.maintenance)
	}

	reportMaintenance(sink, "compaction", llm.Usage{InputTokens: 20, OutputTokens: 4})
	if len(sink.maintenance) != 1 || sink.maintenance[0].Usage.InputTokens != 20 || sink.maintenance[0].Usage.OutputTokens != 4 {
		t.Fatalf("model-backed compaction maintenance = %+v", sink.maintenance)
	}
}

func TestModelRequestEventFromErrorCopiesProxyIdentity(t *testing.T) {
	event := modelRequestEventFromError(&llm.APIError{
		Message: "failed",
		Diagnostic: &llm.APIErrorDiagnostic{
			ProxyInstanceID: "proxy-a",
			ProxyRequestID:  42,
		},
	}, llm.ModelRequestFailed)
	if event.ProxyInstanceID != "proxy-a" || event.ProxyRequestID != 42 {
		t.Fatalf("model request event identity = %+v", event)
	}
}

func TestModelRequestTelemetryNeverEntersTranscriptOrNextRequest(t *testing.T) {
	const providerMessage = "private quota diagnostic"
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				{Kind: llm.EventModelRequest, ModelRequest: &llm.ModelRequestEvent{
					State:          llm.ModelRequestUpstreamAttemptFailed,
					Outcome:        llm.ModelRequestOutcomeRetrying,
					ProxyRequestID: 77,
					StatusCode:     429,
					Message:        providerMessage,
					RetryDelayMS:   100,
				}},
				textDelta("first"),
				toolDone(0, "call_1", "probe", `{}`),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second")}, Stop: llm.StopEndTurn},
	)
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "probe", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}})
	a := newAgent(fp, reg, Options{})
	sink := &modelRequestRecordSink{}
	if err := a.RunPrompt(context.Background(), "inspect", sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 || sink.events[0].ProxyRequestID != 77 {
		t.Fatalf("request telemetry = %+v", sink.events)
	}
	transcript, err := json.Marshal(a.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcript), providerMessage) {
		t.Fatalf("telemetry leaked into transcript: %s", transcript)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want two turns", len(fp.Requests))
	}
	nextRequest, err := json.Marshal(fp.Requests[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(nextRequest), providerMessage) {
		t.Fatalf("telemetry leaked into next provider request: %s", nextRequest)
	}
	mustValid(t, a.Transcript())
}

type promptWorkSink struct {
	recordSink
	pending        bool
	contextPending bool
	usage          llm.Usage
	usageDelivered bool
	waitErr        error
	waits          int
	progress       []string
}

func (s *promptWorkSink) RequestContext() []string {
	if !s.contextPending {
		return nil
	}
	s.contextPending = false
	return []string{"[background delegate completed]\nchild report"}
}

func (s *promptWorkSink) PendingPromptWork() bool {
	return s.pending || s.contextPending
}

func (s *promptWorkSink) WaitForPromptWork(context.Context) (llm.Usage, error) {
	s.waits++
	s.pending = false
	s.contextPending = true
	if s.usageDelivered {
		return llm.Usage{}, s.waitErr
	}
	s.usageDelivered = true
	return s.usage, s.waitErr
}

func (s *promptWorkSink) DrainPromptWorkUsage() llm.Usage { return llm.Usage{} }

func (s *promptWorkSink) PromptWorkWaitStart() {
	s.progress = append(s.progress, "start")
}

func (s *promptWorkSink) PromptWorkWaitComplete() {
	s.progress = append(s.progress, "complete")
}

type diffRecordSink struct {
	recordSink
	diffs []string
	paths []string
}

func (s *diffRecordSink) ToolDiff(_ llm.ToolCall, path, text string) {
	s.paths = append(s.paths, path)
	s.diffs = append(s.diffs, text)
}

type archiveSink struct {
	recordSink
	archive    ToolResultArchive
	archiveErr error
	archived   []llm.ToolResult
}

type countingProvider struct {
	*llmtest.FakeProvider
	count int
	scope llm.InputTokenCountScope
	err   error
}

func (p *countingProvider) CountInputTokens(context.Context, llm.Request) (llm.InputTokenCount, error) {
	if p.err != nil {
		return llm.InputTokenCount{}, p.err
	}
	return llm.InputTokenCount{InputTokens: p.count, Source: "test", Scope: p.scope}, nil
}

func (s *archiveSink) ArchiveToolResult(r llm.ToolResult) (ToolResultArchive, error) {
	s.archived = append(s.archived, r)
	return s.archive, s.archiveErr
}

// recordTool is a fake tool whose Run is scriptable; it records inputs in
// completion-independent entry order. The mutex guards default-parallel calls.
type recordTool struct {
	name       string
	readOnly   bool
	sequential bool
	run        func(ctx context.Context, input json.RawMessage) (string, error)
	mu         sync.Mutex
	inputs     []string
}

type resultRecordTool struct {
	recordTool
	result tools.RunResult
}

type mutationRecordTool struct {
	*recordTool
	paths func(json.RawMessage) ([]string, error)
}

func (t *mutationRecordTool) MutatedPaths(input json.RawMessage) ([]string, error) {
	return t.paths(input)
}

func (t *resultRecordTool) RunResult(context.Context, json.RawMessage) (tools.RunResult, error) {
	return t.result, nil
}

func (t *recordTool) Name() string                  { return t.name }
func (t *recordTool) Description() string           { return "fake tool" }
func (t *recordTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (t *recordTool) ReadOnly(json.RawMessage) bool { return t.readOnly }
func (t *recordTool) RequiresSequential(json.RawMessage) bool {
	return t.sequential
}
func (t *recordTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	t.mu.Lock()
	t.inputs = append(t.inputs, string(input))
	t.mu.Unlock()
	return t.run(ctx, input)
}

type meteredRecordTool struct {
	*recordTool
	usage llm.Usage
}

type richRecordTool struct {
	*recordTool
	result   tools.RichResult
	modality string
}

func (t *richRecordTool) RunRich(ctx context.Context, input json.RawMessage) (tools.RichResult, error) {
	if _, err := t.recordTool.Run(ctx, input); err != nil {
		return tools.RichResult{}, err
	}
	return t.result, nil
}

func (t *richRecordTool) RequiredInputModality() string { return t.modality }

func (t *meteredRecordTool) RunMetered(ctx context.Context, input json.RawMessage) (tools.MeteredResult, error) {
	out, err := t.recordTool.Run(ctx, input)
	return tools.MeteredResult{Text: out, Usage: t.usage}, err
}

func textDelta(s string) llm.StreamEvent {
	return llm.StreamEvent{Kind: llm.EventTextDelta, Text: s}
}

func toolDone(index int, id, name, input string) llm.StreamEvent {
	return llm.StreamEvent{
		Kind:      llm.EventToolCallDone,
		Index:     index,
		ToolID:    id,
		ToolName:  name,
		ToolInput: json.RawMessage(input),
	}
}

func invalidToolDone(index int, id, name, inputErr string) llm.StreamEvent {
	return llm.StreamEvent{
		Kind:              llm.EventToolCallDone,
		Index:             index,
		ToolID:            id,
		ToolName:          name,
		ToolInput:         llm.InvalidToolInputObject(errors.New(inputErr)),
		InvalidInputError: inputErr,
	}
}

func toolUseStart(index int, id, name string) llm.StreamEvent {
	return llm.StreamEvent{
		Kind:     llm.EventToolCallStart,
		Index:    index,
		ToolID:   id,
		ToolName: name,
	}
}

func toolUseDelta(index int, delta string) llm.StreamEvent {
	return llm.StreamEvent{Kind: llm.EventToolCallDelta, Index: index, ArgsDelta: delta}
}

func reasoningSummary(s string) llm.StreamEvent {
	return llm.StreamEvent{Kind: llm.EventReasoningSummary, Text: s}
}

func assistantPhaseEvent(phase string) llm.StreamEvent {
	return llm.StreamEvent{Kind: llm.EventAssistantPhase, Phase: phase}
}

func mustValid(t *testing.T, msgs []llm.Message) {
	t.Helper()
	if err := llm.ValidateTranscript(msgs); err != nil {
		t.Fatalf("transcript invalid: %v\n%s", err, dump(msgs))
	}
}

func dump(msgs []llm.Message) string {
	b, _ := json.MarshalIndent(msgs, "", "  ")
	return string(b)
}

func newAgent(p llm.Provider, reg *tools.Registry, opts Options) *Agent {
	if opts.Registry == nil {
		opts.Registry = llm.NewRegistry(map[string]llm.ModelInfo{
			"claude-opus-4-8": {
				ContextWindow: 1_000_000,
				Price:         llm.Price{Input: 5.0, Output: 25.0, CacheRead: 0.5, CacheWrite: 6.25},
			},
		})
	}
	return New(p, reg, opts)
}

func testHookRunner(t *testing.T, body string) *hooks.Runner {
	t.Helper()
	cfg, err := hooks.DecodeEventMap([]byte(body))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	return &hooks.Runner{Config: cfg}
}

func TestRichToolResultCapabilityGateAndDynamicModelSwitch(t *testing.T) {
	tool := &richRecordTool{
		recordTool: &recordTool{name: "image_tool", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) { return "legacy", nil }},
		modality:   "image",
		result: tools.RichResult{
			Text:    "image attached: screen.png (image/png, 3 bytes, 1x1, detail=high)",
			Content: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: agentOnePixelPNG, ImageDetail: "high", ImageName: "screen.png", ImageWidth: 1, ImageHeight: 1}},
		},
	}
	reg := &tools.Registry{}
	reg.Register(tool)
	models := llm.NewRegistry(map[string]llm.ModelInfo{
		"text-model":   {InputModalities: []string{"text"}},
		"vision-model": {InputModalities: []string{"text", "image"}},
	})
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "call_1", "image_tool", `{}`)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("unsupported handled")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "call_2", "image_tool", `{}`)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("image handled")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{Model: "text-model", Registry: models})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "first", sink); err != nil {
		t.Fatal(err)
	}
	if len(tool.inputs) != 0 {
		t.Fatalf("capability gate performed tool I/O: inputs = %v", tool.inputs)
	}
	if len(sink.results) != 1 || !sink.results[0].IsError || len(sink.results[0].Content) != 0 || !strings.Contains(sink.results[0].Text, "does not advertise image support") {
		t.Fatalf("unsupported result = %+v", sink.results)
	}

	a.SetModel("vision-model", 0)
	if err := a.RunPrompt(context.Background(), "second", sink); err != nil {
		t.Fatal(err)
	}
	if len(tool.inputs) != 1 {
		t.Fatalf("tool executions after model switch = %d, want 1", len(tool.inputs))
	}
	if len(sink.results) != 2 || sink.results[1].IsError || len(sink.results[1].Content) != 1 {
		t.Fatalf("supported result = %+v", sink.results)
	}
	if sink.results[1].Content[0].ImageData != "" || sink.results[1].Content[0].ImageBytes != 69 {
		t.Fatalf("sink result was not safely redacted: %+v", sink.results[1])
	}
	if len(fp.Requests) != 4 {
		t.Fatalf("provider requests = %d, want 4", len(fp.Requests))
	}
	var rich *llm.ContentBlock
	for i := range fp.Requests[3].Messages {
		for j := range fp.Requests[3].Messages[i].Content {
			block := &fp.Requests[3].Messages[i].Content[j]
			if block.Kind == llm.BlockToolResult && block.ResultForID == "call_2" {
				rich = block
			}
		}
	}
	if rich == nil || len(rich.ResultContent) != 1 || rich.ResultContent[0].ImageData != agentOnePixelPNG {
		t.Fatalf("next request missing rich result: %s", dump(fp.Requests[3].Messages))
	}
	mustValid(t, a.Transcript())
}

func TestAcceptRichResultRejectsDynamicImagesForTextModel(t *testing.T) {
	accepted := 0
	result := llm.ToolResult{ForID: "mcp", Text: "attached", Content: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: "YWJj"}}}
	got := acceptRichResult(result, &accepted, false)
	if !got.IsError || len(got.Content) != 0 || !strings.Contains(got.Text, "does not advertise image support") {
		t.Fatalf("acceptRichResult = %+v", got)
	}
}

func TestAcceptRichResultEnforcesAggregateEncodedLimit(t *testing.T) {
	payload := strings.Repeat("A", 11*1024*1024)
	accepted := 2 * len(payload)
	result := llm.ToolResult{ForID: "third", Text: "attached", Content: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: payload}}}
	got := acceptRichResult(result, &accepted, true)
	if !got.IsError || len(got.Content) != 0 || !strings.Contains(got.Text, "encoded total") {
		t.Fatalf("acceptRichResult = %+v", got)
	}
	if accepted != 2*len(payload) {
		t.Fatalf("accepted total changed after rejection: %d", accepted)
	}
}

func TestAcceptRichResultsAccountInEmissionOrder(t *testing.T) {
	total := inputimage.MaxTotalEncodedBytes - 1
	first := llm.ToolResult{ForID: "first", Content: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: "A"}}}
	second := llm.ToolResult{ForID: "second", Content: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: "B"}}}
	if got := acceptRichResult(first, &total, true); got.IsError || total != inputimage.MaxTotalEncodedBytes {
		t.Fatalf("first result = %+v, total=%d", got, total)
	}
	if got := acceptRichResult(second, &total, true); !got.IsError || len(got.Content) != 0 || total != inputimage.MaxTotalEncodedBytes {
		t.Fatalf("second result = %+v, total=%d", got, total)
	}
}

func TestDispatchCallsIncludesRetainedTranscriptInRichBudget(t *testing.T) {
	tool := &richRecordTool{
		recordTool: &recordTool{name: "image_tool", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
			return "legacy", nil
		}},
		modality: "image",
		result: tools.RichResult{
			Text: "attached",
			Content: []llm.ContentBlock{{
				Kind:           llm.BlockImage,
				ImageMediaType: "image/png",
				ImageData:      agentOnePixelPNG,
			}},
		},
	}
	reg := &tools.Registry{}
	reg.Register(tool)
	models := llm.NewRegistry(map[string]llm.ModelInfo{
		"vision-model": {InputModalities: []string{"text", "image"}},
	})
	a := newAgent(llmtest.New("fake"), reg, Options{Model: "vision-model", Registry: models})

	part := inputimage.MaxEncodedBytes - 1
	remainder := inputimage.MaxTotalEncodedBytes - 2*part - 50
	a.SetTranscript([]llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, ImageData: strings.Repeat("A", part)},
			{Kind: llm.BlockImage, ImageData: strings.Repeat("B", part)},
			{Kind: llm.BlockImage, ImageData: strings.Repeat("C", remainder)},
		},
	}})
	sink := &recordSink{}
	blocks, _, _ := a.dispatchCalls(
		context.Background(),
		[]llm.ToolCall{{ID: "call", Name: "image_tool", Input: json.RawMessage(`{}`)}},
		1,
		1,
		sink,
	)
	if len(blocks) != 1 || !blocks[0].ResultError || len(blocks[0].ResultContent) != 0 || !strings.Contains(blocks[0].ResultText, "encoded total") {
		t.Fatalf("tool result block = %+v", blocks)
	}
	if len(sink.results) != 1 || !sink.results[0].IsError || len(sink.results[0].Content) != 0 {
		t.Fatalf("sink results = %+v", sink.results)
	}
}

func TestTextOnlyTurn(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hello "), textDelta("world")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 10, OutputTokens: 5},
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (user+assistant), got %d:\n%s", len(msgs), dump(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Content[0].Text != "hi" {
		t.Errorf("first message should be the user prompt, got %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant {
		t.Errorf("second message should be the assistant reply, got role %q", msgs[1].Role)
	}
	if got := sink.text.String(); got != "hello world" {
		t.Errorf("text deltas = %q, want %q", got, "hello world")
	}
	if len(fp.Requests) != 1 {
		t.Errorf("provider called %d times, want 1", len(fp.Requests))
	} else if fp.Requests[0].Purpose != llm.RequestPurposeTurn {
		t.Errorf("request purpose = %q, want %q", fp.Requests[0].Purpose, llm.RequestPurposeTurn)
	}
}

func TestModelRequestStampsContextBudgetHints(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 100_000})
	a.SetSystem("system prompt")

	if err := a.RunPrompt(context.Background(), "hello", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fp.Requests))
	}
	req := fp.Requests[0]
	if req.ContextWindowHint != 100_000 {
		t.Fatalf("ContextWindowHint = %d, want 100000", req.ContextWindowHint)
	}
	if req.EstimatedInputTokens <= 0 {
		t.Fatalf("EstimatedInputTokens = %d, want positive", req.EstimatedInputTokens)
	}
}

func TestRunPromptUsesProviderInputTokenCount(t *testing.T) {
	fp := &countingProvider{
		FakeProvider: llmtest.New("responses", llmtest.Step{
			Events: []llm.StreamEvent{textDelta("ok")},
			Stop:   llm.StopEndTurn,
		}),
		count: 12_345,
	}
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 100_000})
	a.SetSystem("system prompt")
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hello", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fp.Requests))
	}
	if got := fp.Requests[0].EstimatedInputTokens; got != 12_345 {
		t.Fatalf("EstimatedInputTokens = %d, want provider count 12345", got)
	}
	if len(sink.contexts) == 0 || sink.contexts[0].Total != 12_345 || sink.contexts[0].PayloadTotal != 12_345 {
		t.Fatalf("model start context = %+v, want logical and payload totals 12345", sink.contexts)
	}
	if got := sink.contexts[0]; got.ProviderInputTokens != 12_345 || got.ProviderInputSource != "test" || got.ProviderInputScope != llm.InputTokenCountScopeUnknown {
		t.Fatalf("provider observation = %+v, want unknown-scope test count", got)
	}
}

func TestCountModelRequestInputSeparatesStatefulScopes(t *testing.T) {
	baseEstimate := ContextEstimate{
		Total: 100, System: 8, Tools: 4, Messages: 88, Source: ContextEstimateSourceBytes,
		PayloadTotal: 22, PayloadSystem: 8, PayloadTools: 4, PayloadMessages: 10, PayloadSource: ContextEstimateSourceBytes,
	}
	tests := []struct {
		name            string
		scope           llm.InputTokenCountScope
		count           int
		wantTotal       int
		wantMessages    int
		wantPayload     int
		wantPayloadMsgs int
		wantRequest     int
	}{
		{name: "effective context", scope: llm.InputTokenCountScopeEffectiveContext, count: 30, wantTotal: 30, wantMessages: 18, wantPayload: 22, wantPayloadMsgs: 10, wantRequest: 30},
		{name: "request payload below static sections", scope: llm.InputTokenCountScopeRequestPayload, count: 5, wantTotal: 100, wantMessages: 88, wantPayload: 5, wantPayloadMsgs: 10, wantRequest: 100},
		{name: "unknown", scope: llm.InputTokenCountScope("future_scope"), count: 7, wantTotal: 100, wantMessages: 88, wantPayload: 22, wantPayloadMsgs: 10, wantRequest: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &countingProvider{FakeProvider: llmtest.New("responses"), count: tt.count, scope: tt.scope}
			a := &Agent{provider: provider}
			got := a.countModelRequestInput(context.Background(), modelRequest{
				request:      llm.Request{EstimatedInputTokens: 100, PreviousResponseID: "resp_1"},
				estimate:     baseEstimate,
				usedPrevious: true,
			})
			if got.estimate.Total != tt.wantTotal || got.estimate.Messages != tt.wantMessages ||
				got.estimate.PayloadTotal != tt.wantPayload || got.estimate.PayloadMessages != tt.wantPayloadMsgs {
				t.Fatalf("estimate = %+v, want totals/messages %d/%d and %d/%d", got.estimate, tt.wantTotal, tt.wantMessages, tt.wantPayload, tt.wantPayloadMsgs)
			}
			if got.request.EstimatedInputTokens != tt.wantRequest {
				t.Fatalf("EstimatedInputTokens = %d, want %d", got.request.EstimatedInputTokens, tt.wantRequest)
			}
			if got.estimate.ProviderInputTokens != tt.count || got.estimate.ProviderInputSource != "test" ||
				got.estimate.ProviderInputScope != llm.NormalizeInputTokenCountScope(tt.scope) {
				t.Fatalf("provider observation = %+v", got.estimate)
			}
			for name, value := range map[string]int{
				"total": got.estimate.Total, "system": got.estimate.System, "tools": got.estimate.Tools, "messages": got.estimate.Messages,
				"payload_total": got.estimate.PayloadTotal, "payload_system": got.estimate.PayloadSystem,
				"payload_tools": got.estimate.PayloadTools, "payload_messages": got.estimate.PayloadMessages,
			} {
				if value < 0 {
					t.Fatalf("%s = %d, want nonnegative", name, value)
				}
			}
		})
	}
}

func TestContextOverflowLearnsWindowAndRetries(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 9}}}, Err: &llm.APIError{
			StatusCode: 400,
			Message:    "This endpoint's maximum context length is 262144 tokens. However, you requested about 266580 tokens.",
		}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 4}},
	)
	a := newAgent(fp, tools.Default(), Options{
		Model:         "local",
		ContextWindow: 1_000_000,
	})

	sink := &changingRequestContextSink{}
	if err := a.RunPrompt(context.Background(), "hello", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(fp.Requests))
	}
	if fp.Requests[0].ContextWindowHint != 1_000_000 {
		t.Fatalf("first ContextWindowHint = %d, want 1000000", fp.Requests[0].ContextWindowHint)
	}
	if fp.Requests[1].ContextWindowHint != 262_144 {
		t.Fatalf("retry ContextWindowHint = %d, want 262144", fp.Requests[1].ContextWindowHint)
	}
	if sink.requestCalls != 1 {
		t.Fatalf("RequestContext calls = %d, want 1 for one logical round", sink.requestCalls)
	}
	for request, got := range fp.Requests {
		if context := strings.Join(got.RequestContext, "\n"); !strings.Contains(context, "[request context 1]") {
			t.Fatalf("provider request %d context = %q, want preserved one-shot context", request+1, context)
		}
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}, {turn: 1, attempt: 2}}; !slices.Equal(sink.attemptStarts, want) {
		t.Fatalf("attempt starts = %+v, want %+v", sink.attemptStarts, want)
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}}; !slices.Equal(sink.abandoned, want) {
		t.Fatalf("abandoned = %+v, want %+v", sink.abandoned, want)
	}
	if got := sink.completedTurns[0]; got.Attempts != 2 || got.Wasted.InputTokens != 9 || got.Usage.InputTokens != 13 {
		t.Fatalf("turn usage = %+v, want attempts=2 wasted=9 total=13", got)
	}
}

func TestContextOverflowWithoutWindowShrinksCurrentTurnAndRetries(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&recordTool{
		name:     "big_read",
		readOnly: true,
		run: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", 20_000), nil
		},
	})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_1", "big_read", `{}`)},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 100},
		},
		llmtest.Step{Err: &llm.APIError{
			StatusCode: 400,
			Code:       "context_length_exceeded",
			Message:    "Your input exceeds the context window of this model. Please adjust your input and try again.",
		}},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("recovered")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 100, OutputTokens: 5},
		},
	)
	a := newAgent(fp, reg, Options{ContextWindow: 10_000, DisableAutoCompaction: true})
	sink := &changingRequestContextSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if got := sink.text.String(); got != "recovered" {
		t.Fatalf("text = %q, want recovered", got)
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(fp.Requests))
	}
	toolResultText := func(req llm.Request) string {
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Kind == llm.BlockToolResult && b.ResultForID == "call_1" {
					return b.ResultText
				}
			}
		}
		return ""
	}
	full := toolResultText(fp.Requests[1])
	trimmed := toolResultText(fp.Requests[2])
	failedContext := strings.Join(fp.Requests[1].RequestContext, "\n")
	retryContext := strings.Join(fp.Requests[2].RequestContext, "\n")
	if !strings.Contains(failedContext, "[request context 2]") ||
		!strings.Contains(retryContext, "[request context 2]") ||
		!strings.Contains(retryContext, "[request context 3]") {
		t.Fatalf("overflow contexts: failed=%q retry=%q, want old context preserved and refreshed context merged", failedContext, retryContext)
	}
	if len(full) != 20_000 {
		t.Fatalf("failed request tool result length = %d, want 20000", len(full))
	}
	if len(trimmed) >= len(full) || !strings.Contains(trimmed, retentionTrimMarker) {
		t.Fatalf("retry tool result was not trimmed: len=%d text=%q", len(trimmed), trimmed)
	}
	if !slices.Contains(sink.notices, "[context overflow: compacting and retrying request]") {
		t.Fatalf("notices = %+v, want context overflow retry notice", sink.notices)
	}
	if sink.rewriteCalls != 1 {
		t.Fatalf("TranscriptRewritten calls = %d, want 1 for local overflow degradation", sink.rewriteCalls)
	}
}

func TestContextOverflowRetryResetsAppendBoundary(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&recordTool{
		name:     "big_read",
		readOnly: true,
		run: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", 1_000), nil
		},
	})
	reg.Register(&recordTool{
		name:     "small_read",
		readOnly: true,
		run: func(context.Context, json.RawMessage) (string, error) {
			return "ok", nil
		},
	})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_1", "big_read", `{}`)},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 100},
		},
		llmtest.Step{Err: &llm.APIError{
			StatusCode: 400,
			Code:       "context_length_exceeded",
			Message:    "input exceeds the model context window",
		}},
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_2", "small_read", `{}`)},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 700},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 100},
		},
		// A stale append boundary can spuriously trigger a summary request before
		// the final step above; keep a fallback so the assertion reports the extra
		// call rather than failing for an exhausted script.
		llmtest.Step{Events: []llm.StreamEvent{textDelta("fallback")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{
		ContextWindow:             1_000,
		CompactTriggerPercent:     90,
		CompactKeepTurns:          1,
		CompactToolResultMaxBytes: 128,
	})

	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 4 {
		t.Fatalf("provider requests = %d, want 4 without redundant post-overflow compaction", len(fp.Requests))
	}
}

func TestReasoningSummaryUsesDedicatedSinkOnly(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{reasoningSummary("Checked the repo."), textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.reasoning) != 1 || sink.reasoning[0] != "Checked the repo." {
		t.Fatalf("reasoning summaries = %v", sink.reasoning)
	}
	if len(sink.notices) != 0 {
		t.Fatalf("reasoning summary should not be a notice, got notices = %v", sink.notices)
	}
	msgs := a.Transcript()
	asst := msgs[len(msgs)-1]
	if len(asst.Content) != 1 || asst.Content[0].Kind != llm.BlockText || asst.Content[0].Text != "done" {
		t.Fatalf("assistant transcript should contain only answer text, got:\n%s", dump([]llm.Message{asst}))
	}
}

func TestAssistantPhaseForwardedToSink(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			assistantPhaseEvent(llm.AssistantPhaseCommentary),
			textDelta("I have enough to answer."),
			assistantPhaseEvent(llm.AssistantPhaseFinal),
			textDelta("Yes, with limits."),
		},
		Stop: llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}

	want := []string{llm.AssistantPhaseCommentary, llm.AssistantPhaseFinal}
	if !slices.Equal(sink.phases, want) {
		t.Fatalf("phases = %#v, want %#v", sink.phases, want)
	}
	msgs := a.Transcript()
	if got := msgs[len(msgs)-1].Phase; got != llm.AssistantPhaseFinal {
		t.Fatalf("assistant phase = %q, want final_answer", got)
	}
	if got := sink.text.String(); got != "I have enough to answer.Yes, with limits." {
		t.Fatalf("text = %q", got)
	}
}

func TestSignedReasoningPersistedAndReplayed(t *testing.T) {
	// A signed thinking block (Anthropic) must be persisted into the transcript
	// so it can be replayed verbatim on the next turn; the display summary still
	// goes to the dedicated sink only.
	signed := llm.StreamEvent{Kind: llm.EventReasoningSummary, Text: "weighing options", Signature: "sig-abc"}
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{signed, textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.reasoning) != 1 || sink.reasoning[0] != "weighing options" {
		t.Fatalf("reasoning summaries = %v", sink.reasoning)
	}

	msgs := a.Transcript()
	asst := msgs[len(msgs)-1]
	if len(asst.Content) != 2 {
		t.Fatalf("assistant content = %d blocks, want thinking+text:\n%s", len(asst.Content), dump([]llm.Message{asst}))
	}
	think := asst.Content[0]
	if think.Kind != llm.BlockThinking || think.Thinking != "weighing options" || think.ThinkingSignature != "sig-abc" {
		t.Fatalf("first block = %+v, want signed thinking persisted verbatim", think)
	}
	if asst.Content[1].Kind != llm.BlockText || asst.Content[1].Text != "done" {
		t.Fatalf("second block = %+v, want text answer", asst.Content[1])
	}
}

func TestEncryptedReasoningPersistedAndReplayed(t *testing.T) {
	// An OpenAI Responses encrypted reasoning item (stateless store=false mode)
	// must be persisted as a BlockReasoning so it round-trips on the next turn,
	// sparing the model from re-reasoning. The persist event carries no display
	// text, so the dedicated reasoning sink stays empty.
	encrypted := llm.StreamEvent{Kind: llm.EventReasoningSummary, ReasoningID: "rs_1", ReasoningEncrypted: "ENC-1"}
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{encrypted, textDelta("answer")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("again")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.reasoning) != 0 {
		t.Fatalf("encrypted reasoning must not surface as a display summary, got %v", sink.reasoning)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	asst := msgs[len(msgs)-1]
	if len(asst.Content) != 2 {
		t.Fatalf("assistant content = %d blocks, want reasoning+text:\n%s", len(asst.Content), dump([]llm.Message{asst}))
	}
	r := asst.Content[0]
	if r.Kind != llm.BlockReasoning || r.ReasoningID != "rs_1" || r.ReasoningEncrypted != "ENC-1" {
		t.Fatalf("first block = %+v, want encrypted reasoning persisted verbatim", r)
	}
	if asst.Content[1].Kind != llm.BlockText || asst.Content[1].Text != "answer" {
		t.Fatalf("second block = %+v, want text answer", asst.Content[1])
	}

	if err := a.RunPrompt(context.Background(), "more", sink); err != nil {
		t.Fatalf("RunPrompt 2: %v", err)
	}
	replayed := false
	for _, m := range fp.Requests[1].Messages {
		for _, b := range m.Content {
			if b.Kind == llm.BlockReasoning && b.ReasoningEncrypted == "ENC-1" {
				replayed = true
			}
		}
	}
	if !replayed {
		t.Fatalf("encrypted reasoning block not replayed in the next request:\n%s", dump(fp.Requests[1].Messages))
	}
}

func TestOpaqueReasoningReplayRequiresMatchingDomain(t *testing.T) {
	encrypted := llm.StreamEvent{
		Kind:               llm.EventReasoningSummary,
		ReasoningID:        "rs_fugu",
		ReasoningEncrypted: "FUGU-ONLY",
	}
	first := llmtest.New("responses",
		llmtest.Step{Events: []llm.StreamEvent{encrypted, textDelta("first")}, Stop: llm.StopEndTurn},
	)
	second := llmtest.New("responses",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(first, tools.Default(), Options{
		Model:                 "sakana:fugu-ultra",
		ReasoningReplayDomain: "sakana:fugu-ultra",
	})

	if err := a.RunPrompt(context.Background(), "one", &recordSink{}); err != nil {
		t.Fatalf("first RunPrompt: %v", err)
	}
	var persisted bool
	for _, message := range a.Transcript() {
		for _, block := range message.Content {
			if block.ReasoningEncrypted == "FUGU-ONLY" {
				persisted = block.ReasoningReplayDomain == "sakana:fugu-ultra"
			}
		}
	}
	if !persisted {
		t.Fatalf("reasoning provenance not persisted:\n%s", dump(a.Transcript()))
	}

	a.SetProvider(second)
	a.SetModel("openai-codex:gpt-5.6-sol", 0)
	a.SetReasoningReplayDomain("openai-codex:gpt-5.6-sol")
	if err := a.RunPrompt(context.Background(), "two", &recordSink{}); err != nil {
		t.Fatalf("second RunPrompt: %v", err)
	}
	if requestHasReasoningEncrypted(second.Requests[0], "FUGU-ONLY") {
		t.Fatalf("cross-domain reasoning reached request:\n%s", dump(second.Requests[0].Messages))
	}
	if !requestHasReasoningEncrypted(llm.Request{Messages: a.Transcript()}, "FUGU-ONLY") {
		t.Fatalf("request filtering mutated durable transcript:\n%s", dump(a.Transcript()))
	}
}

func TestOpaqueReasoningReplaysAcrossModelsInSameDomain(t *testing.T) {
	encrypted := llm.StreamEvent{
		Kind:               llm.EventReasoningSummary,
		ReasoningID:        "rs_k3",
		ReasoningEncrypted: "K3-FAMILY",
	}
	fp := llmtest.New("responses",
		llmtest.Step{Events: []llm.StreamEvent{encrypted, textDelta("first")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("second")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{
		Model:                 "kimi:k3-256k",
		ReasoningReplayDomain: "kimi:k3-family",
	})

	if err := a.RunPrompt(context.Background(), "one", &recordSink{}); err != nil {
		t.Fatalf("first RunPrompt: %v", err)
	}
	a.SetModel("kimi:k3", 0)
	a.SetReasoningReplayDomain("kimi:k3-family")
	if err := a.RunPrompt(context.Background(), "two", &recordSink{}); err != nil {
		t.Fatalf("second RunPrompt: %v", err)
	}
	if !requestHasReasoningEncrypted(fp.Requests[1], "K3-FAMILY") {
		t.Fatalf("same-domain reasoning was not replayed:\n%s", dump(fp.Requests[1].Messages))
	}
}

func TestInvalidEncryptedContentRetriesWithoutOpaqueReasoning(t *testing.T) {
	fp := llmtest.New("responses",
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 11, OutputTokens: 2}}},
			Err: &llm.APIError{
				StatusCode: 400,
				Code:       "invalid_encrypted_content",
				Message:    "encrypted content could not be verified",
			},
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("recovered")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("still recovered")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{
		Model:                 "openai:gpt",
		ReasoningReplayDomain: "openai:gpt",
	})
	a.SetTranscript([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "old"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{
				Kind:                  llm.BlockReasoning,
				ReasoningReplayDomain: "openai:gpt",
				ReasoningID:           "rs_bad",
				ReasoningEncrypted:    "INVALID",
			},
			{Kind: llm.BlockText, Text: "old answer"},
		}},
	})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "next", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want failed request plus fallback", len(fp.Requests))
	}
	if !requestHasReasoningEncrypted(fp.Requests[0], "INVALID") {
		t.Fatalf("first request did not exercise encrypted replay:\n%s", dump(fp.Requests[0].Messages))
	}
	if requestHasReasoningEncrypted(fp.Requests[1], "INVALID") {
		t.Fatalf("fallback still replayed rejected reasoning:\n%s", dump(fp.Requests[1].Messages))
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}, {turn: 1, attempt: 2}}; !slices.Equal(sink.attemptStarts, want) {
		t.Fatalf("attempt starts = %+v, want %+v", sink.attemptStarts, want)
	}
	if len(sink.attemptUsage) != 2 ||
		sink.attemptUsage[0].Turn != 1 || sink.attemptUsage[0].Attempt != 1 || sink.attemptUsage[0].Usage.InputTokens != 11 ||
		sink.attemptUsage[1].Turn != 1 || sink.attemptUsage[1].Attempt != 2 || sink.attemptUsage[1].Usage.InputTokens != 7 {
		t.Fatalf("attempt completions = %+v, want attempts 1 then 2 with their exact usage", sink.attemptUsage)
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}}; !slices.Equal(sink.abandoned, want) {
		t.Fatalf("abandoned attempts = %+v, want %+v", sink.abandoned, want)
	}
	turnUsage := sink.completedTurns[0]
	if turnUsage.Attempts != 2 || turnUsage.Wasted.InputTokens != 11 || turnUsage.Usage.InputTokens != 18 || turnUsage.Usage.OutputTokens != 5 {
		t.Fatalf("turn usage = %+v, want attempts=2 wasted=11/2 total=18/5", turnUsage)
	}
	if got := sink.promptUsage[0]; got.Wasted.InputTokens != 11 || got.Usage.InputTokens != 18 || got.Usage.OutputTokens != 5 {
		t.Fatalf("prompt usage = %+v, want exact rejected+accepted accounting", got)
	}
	if a.disabledReasoningReplay["some-other-domain"] {
		t.Fatal("encrypted fallback disabled an unrelated replay domain")
	}
	const notice = "[reasoning replay disabled: provider rejected encrypted content; retrying without opaque reasoning]"
	if !slices.Contains(sink.notices, notice) {
		t.Fatalf("notices = %v, want %q", sink.notices, notice)
	}
	if !requestHasReasoningEncrypted(llm.Request{Messages: a.Transcript()}, "INVALID") {
		t.Fatalf("fallback mutated durable transcript:\n%s", dump(a.Transcript()))
	}

	if err := a.RunPrompt(context.Background(), "again", sink); err != nil {
		t.Fatalf("second RunPrompt: %v", err)
	}
	if requestHasReasoningEncrypted(fp.Requests[2], "INVALID") {
		t.Fatalf("disabled domain replayed reasoning on a later turn:\n%s", dump(fp.Requests[2].Messages))
	}
}

func TestProviderVisibleRequestBuildersShareFilteredSnapshot(t *testing.T) {
	largeMismatch := strings.Repeat("A", 80_000)
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{ReasoningReplayDomain: "domain-b"})
	a.SetTranscript([]llm.Message{
		userText("question"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Kind: llm.BlockReasoning, ReasoningReplayDomain: "domain-a", ReasoningEncrypted: largeMismatch},
			{Kind: llm.BlockReasoning, ReasoningReplayDomain: "domain-b", ReasoningEncrypted: "KEEP-B"},
			{Kind: llm.BlockReasoning, ReasoningEncrypted: "LEGACY"},
			{Kind: llm.BlockText, Text: "answer"},
		}},
	})
	before := dump(a.Transcript())

	contextReq := a.ContextRequestWithContext([]string{"extra"})
	prewarm, ok := a.PrewarmRequest()
	if !ok {
		t.Fatal("PrewarmRequest ok=false")
	}
	modelReq := a.modelRequest([]string{"extra"})
	for name, req := range map[string]llm.Request{"context": contextReq, "prewarm": prewarm, "model": modelReq.request} {
		if requestHasReasoningEncrypted(req, largeMismatch) || requestHasReasoningEncrypted(req, "LEGACY") {
			t.Fatalf("%s request retained incompatible reasoning: %s", name, dump(req.Messages))
		}
		if !requestHasReasoningEncrypted(req, "KEEP-B") {
			t.Fatalf("%s request dropped same-domain reasoning: %s", name, dump(req.Messages))
		}
	}
	wantEstimate := estimateRequest(llm.Request{
		System:         a.system,
		Messages:       modelReq.request.Messages,
		Tools:          a.toolSpecs,
		ServerTools:    a.serverTools,
		RequestContext: []string{"extra"},
	}, a.window())
	if modelReq.estimate.Total != wantEstimate.Total || modelReq.request.EstimatedInputTokens != wantEstimate.Total {
		t.Fatalf("model estimate = %+v request=%d, want filtered total %d", modelReq.estimate, modelReq.request.EstimatedInputTokens, wantEstimate.Total)
	}
	if got, want := modelReq.request.CachePolicy.StableMessagePrefix, a.stableMessagePrefixIn(modelReq.request.Messages); got != want {
		t.Fatalf("cache stable prefix = %d, want %d from provider-visible messages", got, want)
	}

	a.disableCurrentReasoningReplay()
	if requestHasReasoningEncrypted(a.ContextRequest(), "KEEP-B") {
		t.Fatal("disabled current domain remained provider-visible")
	}
	if after := dump(a.Transcript()); after != before {
		t.Fatalf("request builders mutated durable transcript:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestEncryptedFallbackUsesOneShotGuard(t *testing.T) {
	invalid := &llm.APIError{StatusCode: 400, Code: "invalid_encrypted_content", Message: "bad encrypted content"}
	fp := llmtest.New("responses", llmtest.Step{Err: invalid}, llmtest.Step{Err: invalid}, llmtest.Step{Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{ReasoningReplayDomain: "domain"})
	a.SetTranscript([]llm.Message{userText("old"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Kind: llm.BlockReasoning, ReasoningReplayDomain: "domain", ReasoningEncrypted: "opaque"},
		{Kind: llm.BlockText, Text: "answer"},
	}}})
	if err := a.RunPrompt(context.Background(), "next", &recordSink{}); !invalidEncryptedContent(err) {
		t.Fatalf("RunPrompt error = %v, want fallback invalid_encrypted_content", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("requests = %d, want one fallback only", len(fp.Requests))
	}
}

func TestMixedStreamRetryAndEncryptedFallbackShareAttemptSequence(t *testing.T) {
	fp := llmtest.New("responses",
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 10}}}, Err: &llm.APIError{StatusCode: 503, Message: "retry", Retryable: true}},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 20}}}, Err: &llm.APIError{StatusCode: 400, Code: "invalid_encrypted_content", Message: "bad"}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 30}},
	)
	a := newAgent(fp, tools.Default(), Options{ReasoningReplayDomain: "domain"})
	a.SetSleep(func(time.Duration) {})
	a.SetTranscript([]llm.Message{userText("old"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Kind: llm.BlockReasoning, ReasoningReplayDomain: "domain", ReasoningEncrypted: "opaque"},
		{Kind: llm.BlockText, Text: "answer"},
	}}})
	sink := &modelRequestRecordSink{}
	if err := a.RunPrompt(context.Background(), "next", sink); err != nil {
		t.Fatal(err)
	}
	wantAttempts := []turnAttemptEvent{{turn: 1, attempt: 1}, {turn: 1, attempt: 2}, {turn: 1, attempt: 3}}
	if !slices.Equal(sink.attemptStarts, wantAttempts) {
		t.Fatalf("attempt starts = %+v, want %+v", sink.attemptStarts, wantAttempts)
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}, {turn: 1, attempt: 2}}; !slices.Equal(sink.abandoned, want) {
		t.Fatalf("abandoned = %+v, want %+v", sink.abandoned, want)
	}
	var retryScheduled *llm.ModelRequestEvent
	for i := range sink.events {
		if sink.events[i].State == llm.ModelRequestRetryScheduled {
			retryScheduled = &sink.events[i]
		}
	}
	if retryScheduled == nil || retryScheduled.Attempt != 2 {
		t.Fatalf("retry events = %+v, want retry scheduled for absolute attempt 2", sink.events)
	}
	got := sink.completedTurns[0]
	if got.Attempts != 3 || got.Wasted.InputTokens != 30 || got.Usage.InputTokens != 60 {
		t.Fatalf("turn usage = %+v, want attempts=3 wasted=30 total=60", got)
	}
}

func TestFinalizeWithSummaryInvalidEncryptedFallback(t *testing.T) {
	invalid := llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 5, OutputTokens: 1}}},
		Err:    &llm.APIError{StatusCode: 400, Code: "invalid_encrypted_content", Message: "bad encrypted content"},
	}
	fp := llmtest.New("responses", invalid, llmtest.Step{
		Events: []llm.StreamEvent{textDelta("final summary")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 7, OutputTokens: 2},
	})
	a := newAgent(fp, tools.Default(), Options{ReasoningReplayDomain: "domain"})
	a.SetTranscript([]llm.Message{userText("old"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Kind: llm.BlockReasoning, ReasoningReplayDomain: "domain", ReasoningEncrypted: "opaque"},
		{Kind: llm.BlockText, Text: "answer"},
	}}})
	sink := &recordSink{}

	usage, wasted, _, completed := a.finalizeWithSummary(context.Background(), sink, nil, 4)
	if !completed {
		t.Fatal("finalization fallback did not complete")
	}
	if usage.InputTokens != 12 || usage.OutputTokens != 3 || wasted.InputTokens != 5 || wasted.OutputTokens != 1 {
		t.Fatalf("finalization usage = %+v wasted=%+v, want total 12/3 and wasted 5/1", usage, wasted)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want rejected request plus one fallback", len(fp.Requests))
	}
	if !requestHasReasoningEncrypted(fp.Requests[0], "opaque") || requestHasReasoningEncrypted(fp.Requests[1], "opaque") {
		t.Fatalf("finalization replay filtering = first %s second %s", dump(fp.Requests[0].Messages), dump(fp.Requests[1].Messages))
	}
	for i, request := range fp.Requests {
		if len(request.Tools) != 0 || len(request.ServerTools) != 0 {
			t.Fatalf("finalization request %d advertised tools", i+1)
		}
	}
	if want := []turnAttemptEvent{{turn: 4, attempt: 1}, {turn: 4, attempt: 2}}; !slices.Equal(sink.attemptStarts, want) {
		t.Fatalf("attempt starts = %+v, want %+v", sink.attemptStarts, want)
	}
	if want := []turnAttemptEvent{{turn: 4, attempt: 1}}; !slices.Equal(sink.abandoned, want) {
		t.Fatalf("abandoned = %+v, want %+v", sink.abandoned, want)
	}
	if len(sink.completedTurns) != 1 || sink.completedTurns[0].Attempts != 2 || sink.completedTurns[0].Wasted.InputTokens != 5 {
		t.Fatalf("completed final turn = %+v, want attempts=2 and rejected usage wasted", sink.completedTurns)
	}
	last := a.Transcript()[len(a.Transcript())-1]
	if last.Phase != llm.AssistantPhaseFinal || last.Content[0].Text != "final summary" {
		t.Fatalf("final transcript message = %+v", last)
	}
}

func requestHasReasoningEncrypted(req llm.Request, encrypted string) bool {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if providerOwnedReasoningBlock(block.Kind) && block.ReasoningEncrypted == encrypted {
				return true
			}
		}
	}
	return false
}

func TestSessionIDsHaveDistinctContinuationAndCacheLifetimes(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn}, llmtest.Step{Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})

	key := a.ProxySessionID()
	if !strings.HasPrefix(key, "harness-session-") {
		t.Fatalf("proxy session id = %q, want harness-session-*", key)
	}
	cacheKey := a.CacheAffinityID()
	if !strings.HasPrefix(cacheKey, "harness-cache-") {
		t.Fatalf("cache affinity id = %q, want harness-cache-*", cacheKey)
	}

	// Every turn in a session reuses both local keys. The model proxy uses one
	// for continuation and derives the provider-facing prompt cache key from the
	// other.
	for _, prompt := range []string{"one", "two"} {
		if err := a.RunPrompt(context.Background(), prompt, &recordSink{}); err != nil {
			t.Fatalf("RunPrompt %q: %v", prompt, err)
		}
	}
	if got := fp.Requests[0].ProxySessionID; got != key {
		t.Fatalf("request[0] proxy session id = %q, want %q", got, key)
	}
	if fp.Requests[0].PromptCacheKey != "" {
		t.Fatalf("agent request prompt cache key = %q, want proxy to derive it", fp.Requests[0].PromptCacheKey)
	}
	if fp.Requests[0].CacheAffinityID != cacheKey || fp.Requests[1].CacheAffinityID != cacheKey {
		t.Fatalf("cache affinity ids = %q then %q, want %q", fp.Requests[0].CacheAffinityID, fp.Requests[1].CacheAffinityID, cacheKey)
	}
	if fp.Requests[0].ProxySessionID != fp.Requests[1].ProxySessionID {
		t.Fatalf("proxy session id changed across turns: %q vs %q", fp.Requests[0].ProxySessionID, fp.Requests[1].ProxySessionID)
	}

	if other := newAgent(fp, tools.Default(), Options{}).ProxySessionID(); other == key {
		t.Fatalf("proxy session id reused across agent sessions: %q", key)
	}

	a.SetProxySessionID("restored-session")
	if a.ProxySessionID() != "restored-session" {
		t.Fatalf("restored proxy session id = %q", a.ProxySessionID())
	}
	a.ResetProxySessionID()
	if a.ProxySessionID() == "" || a.ProxySessionID() == "restored-session" {
		t.Fatalf("reset proxy session id = %q", a.ProxySessionID())
	}
	if a.CacheAffinityID() != cacheKey {
		t.Fatalf("cache affinity id changed on continuation reset: %q, want %q", a.CacheAffinityID(), cacheKey)
	}
	a.SetCacheAffinityID("restored-cache")
	a.ResetSessionIDs()
	if a.CacheAffinityID() == "" || a.CacheAffinityID() == "restored-cache" {
		t.Fatalf("reset cache affinity id = %q", a.CacheAffinityID())
	}
}

func TestCachePolicyTTLReflectsInteractive(t *testing.T) {
	interactive := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	a := newAgent(interactive, tools.Default(), Options{Interactive: true})
	if err := a.RunPrompt(context.Background(), "hi", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if interactive.Requests[0].CachePolicy.StaticTTL != llm.CacheTTLExtended {
		t.Fatal("interactive session must request the extended static cache TTL")
	}

	oneshot := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	b := newAgent(oneshot, tools.Default(), Options{Interactive: false})
	if err := b.RunPrompt(context.Background(), "hi", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if oneshot.Requests[0].CachePolicy.StaticTTL != llm.CacheTTLDefault {
		t.Fatal("one-shot session must request the default static cache TTL")
	}
}

func TestPrewarmRequestShape(t *testing.T) {
	fp := llmtest.New("fake")
	a := newAgent(fp, tools.Default(), Options{})

	req, ok := a.PrewarmRequest()
	if !ok {
		t.Fatal("PrewarmRequest ok=false, want a warm request (agent advertises tools)")
	}
	if req.MaxTokens != 1 {
		t.Errorf("MaxTokens = %d, want 1 (prefill only)", req.MaxTokens)
	}
	if req.Purpose != llm.RequestPurposePrewarm {
		t.Errorf("Purpose = %q, want %q", req.Purpose, llm.RequestPurposePrewarm)
	}
	if !req.Reasoning.Empty() {
		t.Errorf("Reasoning = %+v, want empty (pure prefix write)", req.Reasoning)
	}
	if len(req.Messages) == 0 {
		t.Fatal("want at least a placeholder message (Messages API requires one)")
	}
	if len(req.RequestContext) != 0 {
		t.Errorf("RequestContext = %v, want nil", req.RequestContext)
	}
}

func TestPrewarmFuncStreamsAndDiscards(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("x")},
		Stop:   llm.StopMaxTokens,
		Usage:  llm.Usage{InputTokens: 12, OutputTokens: 1},
	})
	a := newAgent(fp, tools.Default(), Options{})

	warm, ok := a.PrewarmFunc()
	if !ok {
		t.Fatal("PrewarmFunc ok=false")
	}
	result := warm(context.Background())

	if len(fp.Requests) != 1 {
		t.Fatalf("provider received %d requests, want 1", len(fp.Requests))
	}
	if result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 1 {
		t.Errorf("prewarm usage = %+v, want 12 in / 1 out", result.Usage)
	}
	if fp.Requests[0].MaxTokens != 1 {
		t.Errorf("warm request MaxTokens = %d, want 1", fp.Requests[0].MaxTokens)
	}
	// Pre-warming must not mutate the transcript.
	if n := len(a.Transcript()); n != 0 {
		t.Errorf("transcript mutated by prewarm: %d messages", n)
	}
}

func TestPrewarmFuncPreservesUsageReportedBeforeFailure(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 9, OutputTokens: 1}}},
		Err:    errors.New("stream failed after usage"),
	})
	a := newAgent(fp, tools.Default(), Options{})

	warm, ok := a.PrewarmFunc()
	if !ok {
		t.Fatal("PrewarmFunc ok=false")
	}
	result := warm(context.Background())
	if result.Usage.InputTokens != 9 || result.Usage.OutputTokens != 1 {
		t.Fatalf("prewarm failure usage = %+v, want reported 9 in / 1 out", result.Usage)
	}
}

func TestPrewarmFuncSkipsInvalidRichTranscript(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})
	a.SetTranscript([]llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageMediaType: "image/png",
			ImageData:      "not-base64",
		}},
	}})
	warm, ok := a.PrewarmFunc()
	if !ok {
		t.Fatal("PrewarmFunc ok=false")
	}
	if result := warm(context.Background()); result.Usage != (llm.Usage{}) {
		t.Fatalf("prewarm usage = %+v, want zero", result.Usage)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(fp.Requests))
	}
}

func TestMaxTokensStopEmitsNotice(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("partial final")},
		Stop:   llm.StopMaxTokens,
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if !slices.Contains(sink.notices, "[stopped: model reached max tokens]") {
		t.Fatalf("max-token stop notice missing: %v", sink.notices)
	}
}

func TestPreToolUseHookBlocksToolAndPreservesTranscript(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_1", "writer", `{"path":"x"}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	tool := &recordTool{name: "writer", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "should not run", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)
	runner := testHookRunner(t, `{"PreToolUse":[{"hooks":[{"type":"command","command":"printf '{\"decision\":\"block\",\"reason\":\"no writes\"}'"}]}]}`)
	a := newAgent(fp, reg, Options{Hooks: runner})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(tool.inputs) != 0 {
		t.Fatalf("tool ran despite hook block: %v", tool.inputs)
	}
	if len(sink.hookDiagnostics) != 1 || sink.hookDiagnostics[0].Event != hooks.PreToolUse || sink.hookDiagnostics[0].Outcome != hooks.OutcomeSuccess {
		t.Fatalf("hook diagnostics = %+v", sink.hookDiagnostics)
	}
	if len(sink.results) != 1 || !sink.results[0].IsError || !strings.Contains(sink.results[0].Text, "no writes") {
		t.Fatalf("hook-blocked result = %+v", sink.results)
	}
}

func TestPreToolUseHookBlockStampsErrorKind(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_1", "writer", `{"path":"x"}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "writer", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "should not run", nil
	}})
	runner := testHookRunner(t, `{"PreToolUse":[{"hooks":[{"type":"command","command":"printf '{\"decision\":\"block\",\"reason\":\"no writes\"}'"}]}]}`)
	a := newAgent(fp, reg, Options{Hooks: runner})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(sink.results) != 1 || !sink.results[0].IsError {
		t.Fatalf("hook-blocked result = %+v", sink.results)
	}
	if got := sink.results[0].ErrorKind; got != llm.ToolErrorHookBlocked {
		t.Fatalf("ErrorKind = %q, want %q", got, llm.ToolErrorHookBlocked)
	}
}

func TestPostToolUseHookReplacesToolResult(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_1", "reader", `{"path":"x"}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "reader", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "raw output", nil
	}})
	runner := testHookRunner(t, `{"PostToolUse":[{"hooks":[{"type":"command","command":"printf '{\"continue\":false,\"reason\":\"redacted\"}'"}]}]}`)
	a := newAgent(fp, reg, Options{Hooks: runner})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(sink.results) != 1 || !sink.results[0].IsError || !strings.Contains(sink.results[0].Text, "redacted") {
		t.Fatalf("post-hook result = %+v", sink.results)
	}
	var sawReplaced bool
	for _, msg := range a.Transcript() {
		for _, block := range msg.Content {
			if block.Kind == llm.BlockToolResult && strings.Contains(block.ResultText, "redacted") {
				sawReplaced = true
			}
		}
	}
	if !sawReplaced {
		t.Fatalf("transcript did not include replaced tool result:\n%s", dump(a.Transcript()))
	}
}

func TestRunPromptContentAddsImagesBeforeText(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}
	image := llm.ContentBlock{
		Kind:           llm.BlockImage,
		ImageMediaType: "image/png",
		ImageData:      agentOnePixelPNG,
		ImageDetail:    "high",
		ImageName:      "screen.png",
	}

	if err := a.RunPromptContent(context.Background(), "describe it", []llm.ContentBlock{image}, sink); err != nil {
		t.Fatalf("RunPromptContent: %v", err)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs[0].Content) != 2 {
		t.Fatalf("user content = %d, want image + text", len(msgs[0].Content))
	}
	if msgs[0].Content[0].Kind != llm.BlockImage || msgs[0].Content[0].ImageData != agentOnePixelPNG {
		t.Fatalf("first block = %+v, want image", msgs[0].Content[0])
	}
	if msgs[0].Content[1].Kind != llm.BlockText || msgs[0].Content[1].Text != "describe it" {
		t.Fatalf("second block = %+v, want text", msgs[0].Content[1])
	}
}

func TestDefaultParallelToolCallsPreserveObservableOrder(t *testing.T) {
	tool := &recordTool{name: "echo", run: func(_ context.Context, in json.RawMessage) (string, error) {
		return "ran " + string(in), nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				textDelta("calling tools"),
				toolDone(0, "call_a", "echo", `{"n":1}`),
				toolDone(1, "call_b", "echo", `{"n":2}`),
			},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 20, OutputTokens: 8},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 30, OutputTokens: 4},
		},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)

	// user, assistant(text+2 tool_use), user(2 results), assistant(text)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d:\n%s", len(msgs), dump(msgs))
	}

	// Assistant message preserves emission order: text then both tool_use blocks.
	asst := msgs[1]
	if asst.Role != llm.RoleAssistant || len(asst.Content) != 3 {
		t.Fatalf("assistant message shape wrong:\n%s", dump([]llm.Message{asst}))
	}
	if asst.Content[0].Kind != llm.BlockText || asst.Content[1].ToolUseID != "call_a" || asst.Content[2].ToolUseID != "call_b" {
		t.Errorf("assistant content order wrong:\n%s", dump([]llm.Message{asst}))
	}
	if asst.Phase != llm.AssistantPhaseCommentary {
		t.Errorf("tool-use assistant phase = %q, want commentary", asst.Phase)
	}

	// Results message: two tool_result blocks in call order.
	resMsg := msgs[2]
	if resMsg.Role != llm.RoleUser || len(resMsg.Content) != 2 {
		t.Fatalf("results message shape wrong:\n%s", dump([]llm.Message{resMsg}))
	}
	if resMsg.Content[0].ResultForID != "call_a" || resMsg.Content[1].ResultForID != "call_b" {
		t.Errorf("results out of order:\n%s", dump([]llm.Message{resMsg}))
	}
	if resMsg.Content[0].ToolName != "echo" || resMsg.Content[1].ToolName != "echo" {
		t.Errorf("results missing tool names:\n%s", dump([]llm.Message{resMsg}))
	}
	if len(resMsg.ParallelToolBatches) != 1 || !slices.Equal(resMsg.ParallelToolBatches[0].ToolUseIDs, []string{"call_a", "call_b"}) {
		t.Errorf("parallel batch metadata = %+v, want [call_a call_b]", resMsg.ParallelToolBatches)
	}

	tool.mu.Lock()
	inputCount := len(tool.inputs)
	tool.mu.Unlock()
	if inputCount != 2 {
		t.Errorf("tool executions = %d, want 2", inputCount)
	}

	// Loop re-called the provider after dispatching tools.
	if len(fp.Requests) != 2 {
		t.Errorf("provider called %d times, want 2", len(fp.Requests))
	}
	if msgs[3].Phase != llm.AssistantPhaseFinal {
		t.Errorf("final assistant phase = %q, want final_answer", msgs[3].Phase)
	}
	if len(sink.starts) != 2 || len(sink.results) != 2 {
		t.Errorf("sink saw %d starts and %d results, want 2 each", len(sink.starts), len(sink.results))
	}
}

func TestAssistantMessagePreservesExplicitPhase(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{assistantPhaseEvent(llm.AssistantPhaseCommentary), textDelta("working note")},
		Stop:   llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	if got := msgs[len(msgs)-1].Phase; got != llm.AssistantPhaseCommentary {
		t.Fatalf("assistant phase = %q, want commentary", got)
	}
}

func TestToolUsageIncludedInTurnUsage(t *testing.T) {
	tool := &meteredRecordTool{
		recordTool: &recordTool{name: "delegate", run: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "delegate report", nil
		}},
		usage: llm.Usage{InputTokens: 70, OutputTokens: 30},
	}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_d", "delegate", `{"task":"inspect"}`)},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 2},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 20, OutputTokens: 4},
		},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.promptUsage) != 1 {
		t.Fatalf("prompt usage events = %d, want 1", len(sink.promptUsage))
	}
	got := sink.promptUsage[0].Usage
	if got.InputTokens != 100 || got.OutputTokens != 36 {
		t.Fatalf("prompt usage = %+v, want provider 30/6 + delegate 70/30", got)
	}
}

func TestPendingPromptWorkForcesSynthesisAndCountsUsage(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("premature final")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 2},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("synthesized child report")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 20, OutputTokens: 4},
		},
	)
	a := newAgent(fp, &tools.Registry{}, Options{MaxTurns: 1})
	sink := &promptWorkSink{
		pending: true,
		usage:   llm.Usage{InputTokens: 70, OutputTokens: 30},
	}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if sink.waits != 1 || len(fp.Requests) != 2 {
		t.Fatalf("waits=%d requests=%d, want one join and synthesis request", sink.waits, len(fp.Requests))
	}
	if got := strings.Join(sink.progress, ","); got != "start,complete" {
		t.Fatalf("prompt-work progress = %q, want balanced start,complete", got)
	}
	if got := strings.Join(fp.Requests[1].RequestContext, "\n"); !strings.Contains(got, "child report") {
		t.Fatalf("synthesis request context = %q, want child report", got)
	}
	if len(sink.promptUsage) != 1 {
		t.Fatalf("prompt usage events = %d, want 1", len(sink.promptUsage))
	}
	got := sink.promptUsage[0].Usage
	if got.InputTokens != 100 || got.OutputTokens != 36 {
		t.Fatalf("prompt usage = %+v, want provider 30/6 + background child 70/30", got)
	}
}

func TestPromptWorkProgressCompletesWhenWaitIsCancelled(t *testing.T) {
	sink := &promptWorkSink{pending: true, waitErr: context.Canceled}

	if _, err := waitForPromptWork(context.Background(), sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForPromptWork error = %v, want context canceled", err)
	}
	if got := strings.Join(sink.progress, ","); got != "start,complete" {
		t.Fatalf("prompt-work progress = %q, want balanced start,complete", got)
	}
}

func TestToolCallStreamEventsForwardedBeforeDone(t *testing.T) {
	tool := &recordTool{name: "echo", run: func(_ context.Context, in json.RawMessage) (string, error) {
		return "ran " + string(in), nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolUseStart(0, "call_a", "echo"),
				toolUseDelta(0, `{"n":`),
				toolUseDelta(0, `1}`),
				toolDone(0, "call_a", "echo", `{"n":1}`),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(sink.toolUses) != 1 || sink.toolUses[0].ID != "call_a" || sink.toolUses[0].Name != "echo" {
		t.Fatalf("tool-use start events = %+v, want call_a/echo", sink.toolUses)
	}
	if got := strings.Join(sink.argDeltas, ""); got != `{"n":1}` {
		t.Errorf("tool-use arg deltas = %q, want raw fragments joined", got)
	}
	if len(sink.starts) != 1 || sink.starts[0].Input == nil || string(sink.starts[0].Input) != `{"n":1}` {
		t.Errorf("completed tool start should carry full input, got %+v", sink.starts)
	}

	asst := a.Transcript()[1]
	if len(asst.Content) != 1 || asst.Content[0].Kind != llm.BlockToolUse || string(asst.Content[0].ToolInput) != `{"n":1}` {
		t.Fatalf("transcript should contain only the completed tool input:\n%s", dump([]llm.Message{asst}))
	}
}

func TestToolCallOverridesContradictoryEndTurn(t *testing.T) {
	tool := &recordTool{name: "echo", run: func(_ context.Context, in json.RawMessage) (string, error) {
		return "ran " + string(in), nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_a", "echo", `{"n":1}`)},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(tool.inputs) != 1 || string(tool.inputs[0]) != `{"n":1}` {
		t.Fatalf("tool inputs = %q, want contradictory stop normalized and dispatched", tool.inputs)
	}
	mustValid(t, a.Transcript())
}

func TestToolUseStopWithoutCallFailsBeforeAppendingAssistant(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopToolUse})
	a := newAgent(fp, tools.Default(), Options{})

	err := a.RunPrompt(context.Background(), "go", &recordSink{})
	if err == nil || !strings.Contains(err.Error(), "emitted no usable tool call") {
		t.Fatalf("RunPrompt error = %v", err)
	}
	msgs := a.Transcript()
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("transcript should contain only the prompt: %s", dump(msgs))
	}
	mustValid(t, msgs)
}

func TestInteractionsStatePersistedAndReplayed(t *testing.T) {
	thought := llm.StreamEvent{
		Kind:            llm.EventReasoningSummary,
		ReasoningFormat: llm.ReasoningFormatGeminiInteractions,
		Text:            "search first",
		Signature:       "thought-sig",
	}
	search := llm.StreamEvent{
		Kind:            llm.EventInteractionStep,
		InteractionStep: json.RawMessage(`{"type":"google_search_call","id":"search-1","signature":"search-sig"}`),
	}
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{thought, search, textDelta("answer")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("again")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatal(err)
	}
	msgs := a.Transcript()
	asst := msgs[len(msgs)-1]
	if len(asst.Content) != 3 ||
		asst.Content[0].Kind != llm.BlockInteractionThought ||
		asst.Content[0].InteractionThoughtSignature != "thought-sig" ||
		asst.Content[1].Kind != llm.BlockInteractionStep {
		t.Fatalf("assistant Interactions state = %s", dump([]llm.Message{asst}))
	}
	if err := a.RunPrompt(context.Background(), "more", sink); err != nil {
		t.Fatal(err)
	}
	var replayedThought, replayedSearch bool
	for _, message := range fp.Requests[1].Messages {
		for _, block := range message.Content {
			replayedThought = replayedThought || block.Kind == llm.BlockInteractionThought
			replayedSearch = replayedSearch || block.Kind == llm.BlockInteractionStep
		}
	}
	if !replayedThought || !replayedSearch {
		t.Fatalf("Interactions state not replayed: %s", dump(fp.Requests[1].Messages))
	}
}

func TestTurnAttemptStartEmittedForRetries(t *testing.T) {
	fail := llmtest.Step{Err: &llm.APIError{StatusCode: 503, Message: "service unavailable", Retryable: true}}
	fp := llmtest.New("fake",
		fail,
		llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	want := []turnAttemptEvent{{turn: 1, attempt: 1}, {turn: 1, attempt: 2}}
	if !slices.Equal(sink.attemptStarts, want) {
		t.Errorf("turn attempt starts = %+v, want %+v", sink.attemptStarts, want)
	}
}

func TestFailingToolFedBackAsError(t *testing.T) {
	tool := &recordTool{name: "boom", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", errors.New("kaboom")
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_x", "boom", `{}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("ok")},
			Stop:   llm.StopEndTurn,
		},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	// The error result is appended as an is_error tool_result.
	resMsg := a.Transcript()[2]
	if len(resMsg.Content) != 1 || !resMsg.Content[0].ResultError {
		t.Fatalf("expected an is_error result:\n%s", dump([]llm.Message{resMsg}))
	}
	if resMsg.Content[0].ResultText != "kaboom" {
		t.Errorf("error text = %q, want unprefixed %q", resMsg.Content[0].ResultText, "kaboom")
	}

	// The next request carries the error result so the model can self-correct.
	if len(fp.Requests) != 2 {
		t.Fatalf("provider called %d times, want 2", len(fp.Requests))
	}
	second := fp.Requests[1]
	var carried bool
	for _, m := range second.Messages {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && strings.Contains(b.ResultText, "kaboom") {
				carried = true
			}
		}
	}
	if !carried {
		t.Errorf("second request did not carry the error result:\n%s", dump(second.Messages))
	}
	if len(sink.results) != 1 || !sink.results[0].IsError {
		t.Errorf("sink should have seen one is_error result, got %+v", sink.results)
	}
}

func TestTruncatedWriteArgsSuggestChunkedWrite(t *testing.T) {
	truncated := `invalid JSON at byte offset 152340: unexpected end of JSON input; input preview "{\"path\":\"big.go\",\"content\":\"package main..."`
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{invalidToolDone(0, "call_big", "write", truncated)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("will chunk")},
			Stop:   llm.StopEndTurn,
		},
	)
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "write big.go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.results) != 1 || !sink.results[0].IsError {
		t.Fatalf("results = %+v, want one error result", sink.results)
	}
	got := sink.results[0]
	if got.ErrorKind != llm.ToolErrorInvalidArgs {
		t.Errorf("kind = %q, want %q", got.ErrorKind, llm.ToolErrorInvalidArgs)
	}
	for _, want := range []string{"truncated", "smaller chunks with write", "edit to append"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("result text missing %q: %q", want, got.Text)
		}
	}
}

func TestUnexpectedEOFEditArgsSuggestChunkedWrite(t *testing.T) {
	got := invalidToolInputResult(llm.ToolCall{Name: "edit", InvalidInputError: "unexpected EOF"})
	for _, want := range []string{"truncated", "smaller chunks with write", "edit to append"} {
		if !strings.Contains(got, want) {
			t.Fatalf("result missing %q: %q", want, got)
		}
	}
}

func TestNonTruncatedWriteArgsGetNoChunkSuggestion(t *testing.T) {
	broken := `invalid JSON at byte offset 30: invalid character '}' looking for beginning of value; input preview "{\"path\":\"x.go\",}"`
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{invalidToolDone(0, "call_bad", "write", broken)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("fixed")},
			Stop:   llm.StopEndTurn,
		},
	)
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "write x.go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.results) != 1 {
		t.Fatalf("results = %+v, want one result", sink.results)
	}
	if strings.Contains(sink.results[0].Text, "smaller chunks with write") {
		t.Errorf("non-truncation error should not get the chunked-write sentence: %q", sink.results[0].Text)
	}
}

func TestInvalidToolInputFedBackAsError(t *testing.T) {
	var ran bool
	tool := &recordTool{name: "probe", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		ran = true
		return "should not run", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	invalid := `invalid JSON at byte offset 12: invalid character 'i' in numeric literal; input preview "{\"args\": [-i, vi, .]}"`
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolUseStart(0, "call_bad", "probe"),
				toolUseDelta(0, `{"args": [-i, vi, .]}`),
				invalidToolDone(0, "call_bad", "probe", invalid),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("corrected")},
			Stop:   llm.StopEndTurn,
		},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if ran {
		t.Fatal("invalid tool input should not dispatch the real tool")
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider called %d times, want 2", len(fp.Requests))
	}
	if len(sink.results) != 1 || !sink.results[0].IsError {
		t.Fatalf("sink results = %+v, want one error result", sink.results)
	}
	if strings.HasPrefix(sink.results[0].Text, "error: ") {
		t.Fatalf("internal error text must be unprefixed: %q", sink.results[0].Text)
	}
	for _, want := range []string{"invalid tool call arguments for probe", "valid JSON object"} {
		if !strings.Contains(sink.results[0].Text, want) {
			t.Fatalf("error result %q missing %q", sink.results[0].Text, want)
		}
	}
	if strings.Contains(sink.results[0].Text, `{"args"`) {
		t.Fatalf("generic invalid-input result contains removed-tool hint: %q", sink.results[0].Text)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 4 {
		t.Fatalf("want user, invalid tool_use, error result, final assistant; got %d:\n%s", len(msgs), dump(msgs))
	}
	asst := msgs[1]
	if len(asst.Content) != 1 || asst.Content[0].Kind != llm.BlockToolUse {
		t.Fatalf("assistant tool_use missing:\n%s", dump([]llm.Message{asst}))
	}
	if !strings.Contains(string(asst.Content[0].ToolInput), "_harness_invalid_tool_input") {
		t.Fatalf("invalid tool_use should carry diagnostic input, got %s", asst.Content[0].ToolInput)
	}
	resMsg := msgs[2]
	if len(resMsg.Content) != 1 || !resMsg.Content[0].ResultError || !strings.Contains(resMsg.Content[0].ResultText, "invalid tool call arguments") {
		t.Fatalf("error tool_result missing:\n%s", dump([]llm.Message{resMsg}))
	}

	second := fp.Requests[1]
	var carried bool
	for _, m := range second.Messages {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && strings.Contains(b.ResultText, "invalid tool call arguments for probe") {
				carried = true
			}
		}
	}
	if !carried {
		t.Errorf("second request did not carry invalid-input error:\n%s", dump(second.Messages))
	}
}

func TestMaxTurnsStop(t *testing.T) {
	// Vary the tool result each call so the maxTurns behavior is isolated from
	// the repetition guard (which keys on identical results).
	n := 0
	tool := &recordTool{name: "loop", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		n++
		return fmt.Sprintf("again %d", n), nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	// Every turn asks for a tool: the loop must stop at the limit. After
	// the cap, one tools-disabled summary request winds the turn down (r49).
	always := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "loop", `{}`)},
		Stop:   llm.StopToolUse,
	}
	summary := llmtest.Step{
		Events: []llm.StreamEvent{textDelta("wrapping up: ran loop, nothing left")},
		Stop:   llm.StopEndTurn,
	}
	fp := llmtest.New("fake", always, always, always, summary)
	a := newAgent(fp, reg, Options{MaxTurns: 3})
	sink := &peekCountingSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	// 3 capped turns + 1 tools-disabled wind-down request.
	if len(fp.Requests) != 4 {
		t.Errorf("provider called %d times, want 4 (3 turns + final summary)", len(fp.Requests))
	}
	if sink.requestCalls != len(fp.Requests) {
		t.Errorf("RequestContext calls = %d, want %d (including final summary)", sink.requestCalls, len(fp.Requests))
	}
	// The final summary request must advertise no tools so the model cannot keep
	// calling them.
	if final := fp.Requests[3]; len(final.Tools) != 0 {
		t.Errorf("final wind-down request advertised %d tools, want 0", len(final.Tools))
	}
	// The turn ends on an assistant summary, not a dangling tool_result.
	last := a.Transcript()[len(a.Transcript())-1]
	if last.Role != llm.RoleAssistant || last.Phase != llm.AssistantPhaseFinal {
		t.Errorf("turn should end on a final assistant message, got role=%s phase=%s", last.Role, last.Phase)
	}
	if len(last.Content) == 0 || !strings.Contains(last.Content[0].Text, "wrapping up") {
		t.Errorf("final assistant message = %+v, want the wind-down summary", last.Content)
	}
	usage := sink.promptUsage[0]
	if usage.ClosureTrigger != ClosureTriggerTurnBudget || usage.ClosureTurn != 2 {
		t.Errorf("closure = (%q, turn %d), want (turn_budget, turn 2)", usage.ClosureTrigger, usage.ClosureTurn)
	}
	if !usage.TurnBudgetExhausted {
		t.Error("turn budget should be reported exhausted")
	}
	// A one-shot closure nudge was injected with two allowed turns remaining.
	var sawWrapUp bool
	for _, m := range a.Transcript() {
		for _, b := range m.Content {
			if m.Role == llm.RoleUser && strings.Contains(b.Text, "turn budget") {
				sawWrapUp = true
			}
		}
	}
	if !sawWrapUp {
		t.Errorf("expected a closure steering message with two turns remaining:\n%s", dump(a.Transcript()))
	}
	// Early closure remains tool-permitting; only the post-budget summary disables
	// tools to preserve transcript validity.
	if got := len(fp.Requests[1].Tools); got == 0 {
		t.Error("first closure request advertised no tools; necessary closure work must remain possible")
	}

	var sawMaxTurns bool
	for _, n := range sink.notices {
		if strings.Contains(n, "max turns") {
			sawMaxTurns = true
			if !strings.Contains(n, "(3)") {
				t.Errorf("max-turns notice should name the limit: %q", n)
			}
			if strings.Contains(n, "continue") {
				t.Errorf("max-turns notice should only report stop: %q", n)
			}
		}
	}
	if !sawMaxTurns {
		t.Errorf("sink not told about max-turns stop, notices=%v", sink.notices)
	}
	assertPromptTermination(t, &sink.recordSink, TerminationTurnLimit)
}

func TestWorkflowStatusNormalizationIsBounded(t *testing.T) {
	negative := -1
	sink := &workflowRecordSink{status: WorkflowStatus{
		Available:             true,
		Outcome:               WorkflowOutcome("not-a-status"),
		RemainingRequirements: &negative,
	}}
	got := sampleWorkflowStatus(sink)
	if !got.Available || got.Outcome != WorkflowOutcomeUnknown || got.RemainingRequirements != nil {
		t.Fatalf("sampleWorkflowStatus = %+v, want available unknown with no negative count", got)
	}
}

func TestWorkflowStatusIsSampledAtClosureAndCompletion(t *testing.T) {
	remaining := 2
	sink := &workflowRecordSink{status: WorkflowStatus{
		Available:             true,
		Outcome:               WorkflowOutcomeInProgress,
		RemainingRequirements: &remaining,
	}}
	work := llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn}
	a := newAgent(llmtest.New("fake", work), &tools.Registry{}, Options{MaxTurns: 1})

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.closures) != 1 {
		t.Fatalf("ClosureStarted calls = %d, want 1", len(sink.closures))
	}
	closure := sink.closures[0]
	if closure.Trigger != ClosureTriggerTurnBudget || closure.Turn != 1 {
		t.Errorf("closure = %+v, want turn_budget on turn 1", closure)
	}
	usage := sink.promptUsage[0]
	if usage.WorkflowStatus.Outcome != WorkflowOutcomeInProgress || usage.WorkflowStatus.RemainingRequirements == nil || *usage.WorkflowStatus.RemainingRequirements != 2 {
		t.Errorf("prompt workflow status = %+v", usage.WorkflowStatus)
	}
	if usage.TerminationReason != TerminationModelCompleted {
		t.Errorf("termination reason = %q, want model_completed", usage.TerminationReason)
	}
	if !usage.TurnBudgetExhausted {
		t.Error("final allowed conversational turn should exhaust the turn budget independently of termination reason")
	}
}

func TestEarlyClosureCanFinishBeforeBudgetExhaustion(t *testing.T) {
	tool := &recordTool{name: "work", run: func(context.Context, json.RawMessage) (string, error) {
		return "step complete", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "id", "work", `{}`)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("closed early")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{MaxTurns: 3})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	usage := sink.promptUsage[0]
	if usage.TerminationReason != TerminationModelCompleted || usage.ClosureTrigger != ClosureTriggerTurnBudget || usage.ClosureTurn != 2 {
		t.Fatalf("prompt usage = %+v", usage)
	}
	if usage.TurnBudgetExhausted {
		t.Error("orderly completion on the first closure turn should not report budget exhaustion")
	}
	if len(fp.Requests[1].Tools) == 0 {
		t.Error("early closure request should continue to advertise necessary tools")
	}
}

func TestModelCompletionOnFinalBudgetTurnIsNotTurnLimit(t *testing.T) {
	n := 0
	tool := &recordTool{name: "work", run: func(context.Context, json.RawMessage) (string, error) {
		n++
		return fmt.Sprintf("step %d", n), nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)
	work := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "work", `{}`)},
		Stop:   llm.StopToolUse,
	}
	done := llmtest.Step{
		Events: []llm.StreamEvent{textDelta("finished within budget")},
		Stop:   llm.StopEndTurn,
	}
	fp := llmtest.New("fake", work, work, done)
	a := newAgent(fp, reg, Options{MaxTurns: 3})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if sink.promptUsage[0].Turns != 3 {
		t.Fatalf("turns used = %d, want 3", sink.promptUsage[0].Turns)
	}
	if sink.promptUsage[0].ClosureTrigger != ClosureTriggerTurnBudget || !sink.promptUsage[0].TurnBudgetExhausted {
		t.Errorf("final budget turn closure = %+v", sink.promptUsage[0])
	}
	assertPromptTermination(t, sink, TerminationModelCompleted)
}

func TestNonPositiveMaxTurnsIsUnlimited(t *testing.T) {
	const formerDefaultMaxTurns = 250

	n := 0
	tool := &recordTool{name: "loop", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		n++
		return fmt.Sprintf("again %d", n), nil // distinct output each call so the repeat guard never trips
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	toolUse := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "loop", `{}`)},
		Stop:   llm.StopToolUse,
	}
	turns := make([]llmtest.Step, formerDefaultMaxTurns+2)
	for i := 0; i < formerDefaultMaxTurns+1; i++ {
		turns[i] = toolUse
	}
	turns[len(turns)-1] = llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn}
	fp := llmtest.New("fake", turns...)
	a := newAgent(fp, reg, Options{MaxTurns: 0})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(fp.Requests) != formerDefaultMaxTurns+2 {
		t.Errorf("provider called %d times, want %d (past the former default cap)", len(fp.Requests), formerDefaultMaxTurns+2)
	}
	if sink.promptUsage[0].Turns != formerDefaultMaxTurns+2 {
		t.Errorf("PromptComplete turns = %d, want %d", sink.promptUsage[0].Turns, formerDefaultMaxTurns+2)
	}
	if sink.promptUsage[0].ClosureTrigger != "" || sink.promptUsage[0].TurnBudgetExhausted {
		t.Errorf("unlimited prompt reported turn-budget closure: %+v", sink.promptUsage[0])
	}
	for _, n := range sink.notices {
		if strings.Contains(n, "max turns") {
			t.Errorf("unlimited max turns should not emit stop notice, got %q", n)
		}
	}
	assertPromptTermination(t, sink, TerminationModelCompleted)
}

func TestCancellationMidStreamKeepsPartialText(t *testing.T) {
	tool := &recordTool{name: "noop", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	ctx, cancel := context.WithCancel(context.Background())
	// The step emits partial text, then a tool_use, but cancellation fires before
	// the terminal event. Un-executed tool_use must be stripped; partial text kept.
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("partial answer")},
		Stop:   llm.StopToolUse,
		Block:  func(_ context.Context) { cancel() },
	})
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	err := a.RunPrompt(ctx, "go", sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunPrompt err = %v, want context.Canceled", err)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 2 {
		t.Fatalf("want user + partial assistant, got %d:\n%s", len(msgs), dump(msgs))
	}
	asst := msgs[1]
	if asst.Role != llm.RoleAssistant {
		t.Fatalf("second message should be assistant, got %q", asst.Role)
	}
	for _, b := range asst.Content {
		if b.Kind == llm.BlockToolUse {
			t.Errorf("dangling tool_use not stripped:\n%s", dump([]llm.Message{asst}))
		}
	}
	if asst.Content[0].Text != "partial answer" {
		t.Errorf("partial text not kept, got %q", asst.Content[0].Text)
	}
	if asst.Phase != llm.AssistantPhaseCommentary {
		t.Errorf("partial assistant phase = %q, want commentary", asst.Phase)
	}
	if len(sink.completedTurns) != 1 || sink.promptUsage[0].Turns != 1 {
		t.Fatalf("retained partial response should complete one turn: turns=%+v prompt=%+v", sink.completedTurns, sink.promptUsage)
	}
	assertPromptTermination(t, sink, TerminationCancelled)
}

func TestCancellationWithNoTextDropsMessage(t *testing.T) {
	reg := &tools.Registry{}
	ctx, cancel := context.WithCancel(context.Background())
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{},
		Stop:   llm.StopEndTurn,
		Block:  func(_ context.Context) { cancel() },
	})
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	err := a.RunPrompt(ctx, "go", sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunPrompt err = %v, want context.Canceled", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	// Nothing streamed: the partial assistant message is dropped, leaving only the
	// user message.
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("want only the user message, got %d:\n%s", len(msgs), dump(msgs))
	}
	if len(sink.completedTurns) != 0 || sink.promptUsage[0].Turns != 0 {
		t.Fatalf("uncommitted failed attempt counted as a turn: turns=%+v prompt=%+v", sink.completedTurns, sink.promptUsage)
	}
	assertPromptTermination(t, sink, TerminationCancelled)
}

func TestUsageAccumulatedAcrossTurns(t *testing.T) {
	tool := &recordTool{name: "echo", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "x", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "a", "echo", `{}`)},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 100, OutputTokens: 10},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 200, OutputTokens: 20},
		},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.promptUsage) != 1 {
		t.Fatalf("want one PromptComplete, got %d", len(sink.promptUsage))
	}
	pu := sink.promptUsage[0]
	if pu.Usage.InputTokens != 300 || pu.Usage.OutputTokens != 30 {
		t.Errorf("prompt usage = %+v, want 300 in / 30 out", pu.Usage)
	}
	if pu.Turns != 2 {
		t.Errorf("prompt turns = %d, want 2", pu.Turns)
	}
	assertPromptTermination(t, sink, TerminationModelCompleted)
}

func TestTurnAttemptUsageEmittedForEachProviderReturn(t *testing.T) {
	tool := &recordTool{name: "echo", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "x", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "a", "echo", `{}`)},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 100, OutputTokens: 10},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 200, OutputTokens: 20},
		},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.attemptUsage) != 2 {
		t.Fatalf("turn attempt usage events = %d, want 2", len(sink.attemptUsage))
	}
	if got := sink.attemptUsage[0]; got.Turn != 1 || got.Attempt != 1 || got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 10 {
		t.Errorf("turn attempt usage[0] = %+v, want turn 1 attempt 1 with 100/10", got)
	}
	if got := sink.attemptUsage[1]; got.Turn != 2 || got.Attempt != 1 || got.Usage.InputTokens != 200 || got.Usage.OutputTokens != 20 {
		t.Errorf("turn attempt usage[1] = %+v, want turn 2 attempt 1 with 200/20", got)
	}
}

func TestRunPromptRejectsInvalidStableTranscriptBeforeRequest(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("should not run")}, Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})
	a.SetTranscript([]llm.Message{asstToolUse("dangling", "read", `{}`)})
	sink := &recordSink{}

	err := a.RunPrompt(context.Background(), "next", sink)
	if err == nil || !strings.Contains(err.Error(), "agent transcript invalid before model request") {
		t.Fatalf("RunPrompt err = %v, want invalid transcript before request", err)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(fp.Requests))
	}
	if len(sink.promptUsage) != 1 {
		t.Fatalf("prompt usage events = %d, want 1", len(sink.promptUsage))
	}
}

func TestRunPromptRollsBackInvalidNewImage(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})
	invalid := llm.ContentBlock{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "not-base64"}

	err := a.RunPromptContent(context.Background(), "invalid", []llm.ContentBlock{invalid}, &recordSink{})
	if err == nil || !strings.Contains(err.Error(), "invalid image base64") {
		t.Fatalf("invalid prompt error = %v", err)
	}
	if len(a.Transcript()) != 0 {
		t.Fatalf("invalid prompt remained in transcript: %s", dump(a.Transcript()))
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider requests after invalid prompt = %d", len(fp.Requests))
	}

	if err := a.RunPrompt(context.Background(), "valid", &recordSink{}); err != nil {
		t.Fatalf("valid prompt after rollback: %v", err)
	}
	if len(fp.Requests) != 1 || len(fp.Requests[0].Messages) != 1 || fp.Requests[0].Messages[0].Content[0].Text != "valid" {
		t.Fatalf("provider requests = %+v", fp.Requests)
	}
}

func TestStreamSummaryRejectsInvalidRichContentBeforeProvider(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("must not run", 1, 1))
	a := newAgent(fp, tools.Default(), Options{})
	messages := []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageMediaType: "image/png",
			ImageData:      "not-base64",
		}},
	}}
	if _, _, _, err := a.streamSummary(context.Background(), "summary", messages, 100, llm.RequestPurposeCompaction); err == nil {
		t.Fatal("streamSummary accepted invalid image")
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(fp.Requests))
	}
}

// SetTools swaps the registry that backs both the advertised specs and
// dispatch, so an agent switch immediately changes what the model sees and can
// call.
func TestSetToolsChangesAdvertisedAndDispatchableTools(t *testing.T) {
	full := &tools.Registry{}
	full.Register(&recordTool{name: "read", readOnly: true})
	full.Register(&recordTool{name: "extra", readOnly: true})
	restricted := &tools.Registry{}
	restricted.Register(&recordTool{name: "read", readOnly: true})

	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("a")}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("b")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, full, Options{})

	if err := a.RunPrompt(context.Background(), "one", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	a.SetTools(restricted)
	if err := a.RunPrompt(context.Background(), "two", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}

	if names := specNames(fp.Requests[0].Tools); !slices.Contains(names, "extra") {
		t.Errorf("first request should advertise extra, got %v", names)
	}
	if names := specNames(fp.Requests[1].Tools); slices.Contains(names, "extra") {
		t.Errorf("after SetTools, extra should no longer be advertised, got %v", names)
	}

	// A call to the now-removed tool must be undispatchable.
	res := a.tools.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "extra", Input: json.RawMessage(`{}`)})
	if !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Errorf("removed tool should be undispatchable, got %+v", res)
	}
}

func TestToolSpecsReturnsDeepCopy(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "read", readOnly: true})
	a := newAgent(llmtest.New("fake"), reg, Options{})

	specs := a.ToolSpecs()
	if len(specs) != 1 {
		t.Fatalf("ToolSpecs = %d, want 1", len(specs))
	}
	specs[0].Name = "mutated"
	specs[0].Parameters[0] = 'x'

	later := a.ToolSpecs()
	if later[0].Name != "read" {
		t.Fatalf("cached tool name mutated to %q", later[0].Name)
	}
	if later[0].Parameters[0] == 'x' {
		t.Fatalf("cached tool parameters were mutated: %s", later[0].Parameters)
	}

	req := a.ContextRequest()
	if req.Tools[0].Name != "read" || req.Tools[0].Parameters[0] == 'x' {
		t.Fatalf("ContextRequest used mutated specs: %+v", req.Tools[0])
	}
}

func TestResponsesStatefulSendsDeltaAfterResponseID(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&recordTool{
		name:     "echo",
		readOnly: true,
		run: func(context.Context, json.RawMessage) (string, error) {
			return "tool output", nil
		},
	})
	fp := llmtest.New("responses",
		llmtest.Step{
			Events:     []llm.StreamEvent{toolDone(0, "call_1", "echo", `{}`)},
			Stop:       llm.StopToolUse,
			ResponseID: "resp_1",
		},
		llmtest.Step{
			Events:     []llm.StreamEvent{textDelta("done")},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp_2",
		},
	)
	a := newAgent(fp, reg, Options{ResponsesStateful: true})

	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	if !fp.Requests[0].StoreResponse || fp.Requests[0].PreviousResponseID != "" {
		t.Fatalf("first request state = store %v prev %q", fp.Requests[0].StoreResponse, fp.Requests[0].PreviousResponseID)
	}
	if got := fp.Requests[0].Messages[0].Content[0].Text; got != "go" {
		t.Fatalf("first request message = %q, want user prompt", got)
	}
	if !fp.Requests[1].StoreResponse || fp.Requests[1].PreviousResponseID != "resp_1" {
		t.Fatalf("second request state = store %v prev %q", fp.Requests[1].StoreResponse, fp.Requests[1].PreviousResponseID)
	}
	if len(fp.Requests[1].Messages) != 1 || fp.Requests[1].Messages[0].Content[0].Kind != llm.BlockToolResult {
		t.Fatalf("second request messages = %+v, want only tool result delta", fp.Requests[1].Messages)
	}
	state := a.ResponseState()
	if state == nil ||
		state.PreviousResponseID != "resp_2" ||
		state.AnchorMessages != len(a.Transcript()) ||
		!llm.MatchesMessageFingerprint(a.Transcript(), state.AnchorDigest) {
		t.Fatalf("response state = %+v, transcript len %d", state, len(a.Transcript()))
	}
}

// probeProvider wraps a scripted provider with a controllable continuation
// probe so tests can exercise the pre-request response-continuation check.
type probeProvider struct {
	*llmtest.FakeProvider
	canContinue func(string) bool
}

func (p *probeProvider) CanContinueResponse(responseID string) bool {
	return p.canContinue(responseID)
}

func TestResponsesProbeResetsDeadAnchorBeforeRequest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		canContinue func(string) bool
		wantPrev    string
		wantNotices int
	}{{
		name:        "probe unavailable",
		canContinue: func(string) bool { return false },
		wantPrev:    "",
		wantNotices: 1,
	}, {
		name:        "probe available",
		canContinue: func(string) bool { return true },
		wantPrev:    "resp_anchor",
		wantNotices: 0,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			fp := llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_next"})
			a := newAgent(&probeProvider{FakeProvider: fp, canContinue: tc.canContinue}, tools.Default(), Options{ResponsesStateful: true})
			a.SetTranscript([]llm.Message{userText("old"), asstText("answer")})
			digest, err := llm.FingerprintMessages(a.Transcript())
			if err != nil {
				t.Fatal(err)
			}
			a.SetResponseState(&llm.ResponseState{PreviousResponseID: "resp_anchor", AnchorMessages: 2, AnchorDigest: digest})
			sink := &recordSink{}

			if err := a.RunPrompt(context.Background(), "next", sink); err != nil {
				t.Fatalf("RunPrompt: %v", err)
			}
			if len(fp.Requests) != 1 {
				t.Fatalf("provider requests = %d, want exactly one (no discovery round-trip)", len(fp.Requests))
			}
			if got := fp.Requests[0].PreviousResponseID; got != tc.wantPrev {
				t.Fatalf("request previous_response_id = %q, want %q", got, tc.wantPrev)
			}
			if tc.wantPrev == "" && len(fp.Requests[0].Messages) != 3 {
				t.Fatalf("stateless request messages = %d, want full context (3)", len(fp.Requests[0].Messages))
			}
			resetNotices := 0
			for _, n := range sink.notices {
				if strings.Contains(n, "continuation unavailable") {
					resetNotices++
				}
			}
			if resetNotices != tc.wantNotices {
				t.Fatalf("reset notices = %d, want %d: %v", resetNotices, tc.wantNotices, sink.notices)
			}
		})
	}
}

func TestResponsesProbeDisablesStateAfterRepeatedDeadAnchors(t *testing.T) {
	fp := llmtest.New("responses",
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_1"},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_2"},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_3"},
	)
	a := newAgent(&probeProvider{FakeProvider: fp, canContinue: func(string) bool { return false }}, tools.Default(), Options{ResponsesStateful: true})
	a.SetTranscript([]llm.Message{userText("old"), asstText("answer")})
	digest, err := llm.FingerprintMessages(a.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "resp_anchor", AnchorMessages: 2, AnchorDigest: digest})

	for prompt := 1; prompt <= 3; prompt++ {
		sink := &recordSink{}
		if err := a.RunPrompt(context.Background(), fmt.Sprintf("prompt %d", prompt), sink); err != nil {
			t.Fatalf("RunPrompt %d: %v", prompt, err)
		}
		disabled := false
		for _, notice := range sink.notices {
			if strings.Contains(notice, "continuation repeatedly unavailable") {
				disabled = true
			}
		}
		if disabled != (prompt == 3) {
			t.Fatalf("prompt %d disabled=%v, notices=%v", prompt, disabled, sink.notices)
		}
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want one per prompt", len(fp.Requests))
	}
	if fp.Requests[0].StoreResponse != true || fp.Requests[1].StoreResponse != true {
		t.Fatalf("stateful mode disabled before third probe failure: %+v", fp.Requests)
	}
	if fp.Requests[2].StoreResponse || fp.Requests[2].PreviousResponseID != "" {
		t.Fatalf("third request = store %v prev %q, want stateless", fp.Requests[2].StoreResponse, fp.Requests[2].PreviousResponseID)
	}
	if state := a.ResponseState(); state != nil {
		t.Fatalf("response state = %+v, want cleared after disable", state)
	}
}

func TestResponsesProbeAbsentProviderKeepsStatefulBehavior(t *testing.T) {
	// A provider without the probe (plain HTTP) must keep its anchor so HTTP
	// behavior is unchanged.
	fp := llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_next"})
	a := newAgent(fp, tools.Default(), Options{ResponsesStateful: true})
	a.SetTranscript([]llm.Message{userText("old"), asstText("answer")})
	digest, err := llm.FingerprintMessages(a.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "resp_anchor", AnchorMessages: 2, AnchorDigest: digest})

	if err := a.RunPrompt(context.Background(), "next", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 1 || fp.Requests[0].PreviousResponseID != "resp_anchor" {
		t.Fatalf("requests = %+v, want one request keeping the anchor", fp.Requests)
	}
}

func TestResponsesStateDisabledAfterRepeatedContinuationFailures(t *testing.T) {
	// Each prompt's first request is rejected, so the count accumulates across
	// prompts: the reactive stateless retry inside a prompt succeeds without
	// continuing the anchor and must not clear it.
	rejected := &llm.APIError{StatusCode: 400, Code: "previous_response_not_found", Message: "missing previous_response_id"}
	fp := llmtest.New("responses",
		llmtest.Step{Err: rejected},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_ok_1"},
		llmtest.Step{Err: rejected},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_ok_2"},
		llmtest.Step{Err: rejected},
		// The third failure disables stateful mode; its retry is stateless.
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp_ok_3"},
	)
	a := newAgent(fp, tools.Default(), Options{ResponsesStateful: true})
	a.SetTranscript([]llm.Message{userText("old"), asstText("answer")})
	digest, err := llm.FingerprintMessages(a.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "resp_anchor", AnchorMessages: 2, AnchorDigest: digest})

	for prompt := 1; prompt <= 3; prompt++ {
		sink := &recordSink{}
		if err := a.RunPrompt(context.Background(), fmt.Sprintf("prompt %d", prompt), sink); err != nil {
			t.Fatalf("RunPrompt %d: %v", prompt, err)
		}
		disabled := false
		for _, n := range sink.notices {
			if strings.Contains(n, "continuation repeatedly unavailable") {
				disabled = true
			}
		}
		if prompt < 3 && disabled {
			t.Fatalf("prompt %d disabled stateful mode early: %v", prompt, sink.notices)
		}
		if prompt == 3 && !disabled {
			t.Fatalf("prompt 3 missing disable notice: %v", sink.notices)
		}
	}
	// Six scripted steps for three rejected+retried prompts: every provider
	// call maps to a scripted step, so no extra probe round-trips happened.
	if len(fp.Requests) != 6 {
		t.Fatalf("provider requests = %d, want 6", len(fp.Requests))
	}
	// From the disable point on, requests are stateless.
	for i, req := range fp.Requests[5:] {
		if req.StoreResponse || req.PreviousResponseID != "" {
			t.Fatalf("request[%d] = store %v prev %q, want stateless after disable", i+5, req.StoreResponse, req.PreviousResponseID)
		}
	}
	if state := a.ResponseState(); state != nil {
		t.Fatalf("response state = %+v, want cleared after disable", state)
	}
}

func TestResponsesStatefulRetriesFullContextWhenPreviousResponseRejected(t *testing.T) {
	fp := llmtest.New("responses",
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 12}}}, Err: &llm.APIError{StatusCode: 400, Code: "previous_response_not_found", Message: "missing previous_response_id"}},
		llmtest.Step{
			Events:     []llm.StreamEvent{textDelta("recovered")},
			Stop:       llm.StopEndTurn,
			Usage:      llm.Usage{InputTokens: 8},
			ResponseID: "resp_new",
		},
	)
	fixedNow := func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	a := newAgent(fp, tools.Default(), Options{ResponsesStateful: true, Now: fixedNow})
	a.SetTranscript([]llm.Message{
		textMessageAt(fixedNow(), llm.RoleUser, "old"),
		textMessageAt(fixedNow(), llm.RoleAssistant, "answer"),
	})
	digest, err := llm.FingerprintMessages(a.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "missing", AnchorMessages: 2, AnchorDigest: digest})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "next", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	if fp.Requests[0].PreviousResponseID != "missing" || len(fp.Requests[0].Messages) != 1 {
		t.Fatalf("first request = prev %q messages %d", fp.Requests[0].PreviousResponseID, len(fp.Requests[0].Messages))
	}
	if fp.Requests[1].PreviousResponseID != "" || len(fp.Requests[1].Messages) != 3 {
		t.Fatalf("retry request = prev %q messages %d, want full context", fp.Requests[1].PreviousResponseID, len(fp.Requests[1].Messages))
	}
	if got := sink.completedTurns[0]; got.Attempts != 2 || got.Wasted.InputTokens != 12 || got.Usage.InputTokens != 20 {
		t.Fatalf("turn usage = %+v, want attempts=2 wasted=12 total=20", got)
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}}; !slices.Equal(sink.abandoned, want) {
		t.Fatalf("abandoned = %+v, want %+v", sink.abandoned, want)
	}
}

func TestResponsesStateFingerprintRejectsSameLengthTranscriptMutation(t *testing.T) {
	fp := llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp-next"})
	a := newAgent(fp, tools.Default(), Options{ResponsesStateful: true})
	a.SetTranscript([]llm.Message{
		userText("old"),
		asstText("answer"),
	})
	digest, err := llm.FingerprintMessages(a.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	a.SetResponseState(&llm.ResponseState{
		PreviousResponseID: "resp-old",
		AnchorMessages:     len(a.Transcript()),
		AnchorDigest:       digest,
	})
	a.transcript[0].Content[0].Text = "new" // same byte length, simulating an unannounced rewrite

	if err := a.RunPrompt(context.Background(), "next", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if len(fp.Requests) != 1 || fp.Requests[0].PreviousResponseID != "" || len(fp.Requests[0].Messages) != 3 {
		t.Fatalf("request after mutation = %+v", fp.Requests)
	}
}

func TestResponsesStateMissingOrMalformedDigestIsDiscarded(t *testing.T) {
	for _, digest := range []string{"", "not-a-digest"} {
		t.Run(digest, func(t *testing.T) {
			fp := llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn})
			a := newAgent(fp, tools.Default(), Options{ResponsesStateful: true})
			a.SetTranscript([]llm.Message{userText("old"), asstText("answer")})
			a.SetResponseState(&llm.ResponseState{
				PreviousResponseID: "resp-old",
				AnchorMessages:     2,
				AnchorDigest:       digest,
			})
			if err := a.RunPrompt(context.Background(), "next", &recordSink{}); err != nil {
				t.Fatal(err)
			}
			if fp.Requests[0].PreviousResponseID != "" || len(fp.Requests[0].Messages) != 3 {
				t.Fatalf("request = %+v", fp.Requests[0])
			}
		})
	}
}

func TestPrewarmResultInstallsOnlyCurrentExplicitAnchor(t *testing.T) {
	zero := 0
	fp := llmtest.New("responses", llmtest.Step{
		Events: []llm.StreamEvent{{
			Kind:             llm.EventDone,
			ResponseID:       "resp-prewarm",
			ResponseIDAnchor: &zero,
		}},
		Stop: llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{ResponsesStateful: true})
	warm, ok := a.PrewarmFunc()
	if !ok {
		t.Fatal("PrewarmFunc ok=false")
	}
	result := warm(context.Background())
	if result.ResponseState == nil ||
		result.ResponseState.AnchorMessages != 0 ||
		result.ResponseState.AnchorDigest == "" ||
		result.ResponseStateEpoch != a.ResponseStateEpoch() ||
		result.ProxySessionID != a.ProxySessionID() {
		t.Fatalf("prewarm result = %+v", result)
	}
	if !a.ApplyPrewarmResult(result) {
		t.Fatal("current prewarm result was not installed")
	}
	if state := a.ResponseState(); state == nil || state.PreviousResponseID != "resp-prewarm" {
		t.Fatalf("installed state = %+v", state)
	}

	// A normal turn that wins the race prevents the background result from
	// replacing its newer anchor.
	a.responseState = llm.ResponseState{
		PreviousResponseID: "resp-turn",
		AnchorMessages:     0,
		AnchorDigest:       result.ResponseState.AnchorDigest,
	}
	if a.ApplyPrewarmResult(result) {
		t.Fatal("prewarm replaced a real-turn anchor")
	}
}

func TestPrewarmResultRejectsStaleEpochIdentityAndTranscript(t *testing.T) {
	emptyDigest, err := llm.FingerprintMessages(nil)
	if err != nil {
		t.Fatal(err)
	}
	newResult := func(a *Agent) PrewarmResult {
		return PrewarmResult{
			ResponseState: &llm.ResponseState{
				PreviousResponseID: "resp-prewarm",
				AnchorMessages:     0,
				AnchorDigest:       emptyDigest,
			},
			ResponseStateEpoch: a.ResponseStateEpoch(),
			ProxySessionID:     a.ProxySessionID(),
			TranscriptMessages: len(a.Transcript()),
		}
	}
	t.Run("epoch", func(t *testing.T) {
		a := newAgent(llmtest.New("responses"), tools.Default(), Options{ResponsesStateful: true})
		result := newResult(a)
		a.SetModel("different", 0)
		if a.ApplyPrewarmResult(result) {
			t.Fatal("stale model epoch installed")
		}
	})
	t.Run("proxy session", func(t *testing.T) {
		a := newAgent(llmtest.New("responses"), tools.Default(), Options{ResponsesStateful: true})
		result := newResult(a)
		result.ProxySessionID = "foreign"
		if a.ApplyPrewarmResult(result) {
			t.Fatal("foreign proxy session installed")
		}
	})
	t.Run("transcript length", func(t *testing.T) {
		a := newAgent(llmtest.New("responses"), tools.Default(), Options{ResponsesStateful: true})
		result := newResult(a)
		result.TranscriptMessages++
		if a.ApplyPrewarmResult(result) {
			t.Fatal("stale transcript length installed")
		}
	})
}

func TestHTTPPrewarmWithoutAnchorDoesNotProduceResponseState(t *testing.T) {
	fp := llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp-http"})
	a := newAgent(fp, tools.Default(), Options{ResponsesStateful: true})
	warm, ok := a.PrewarmFunc()
	if !ok {
		t.Fatal("PrewarmFunc ok=false")
	}
	result := warm(context.Background())
	if result.ResponseState != nil || a.ApplyPrewarmResult(result) {
		t.Fatalf("HTTP-style prewarm installed state: result=%+v state=%+v", result, a.ResponseState())
	}
}

func TestResponsesStatefulDisablesAndRetriesWhenStoreRejected(t *testing.T) {
	fp := llmtest.New("responses",
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 14}}}, Err: &llm.APIError{StatusCode: 400, Message: "Store must be set to false"}},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("recovered")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 6},
		},
	)
	a := newAgent(fp, tools.Default(), Options{ResponsesStateful: true})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	if !fp.Requests[0].StoreResponse {
		t.Fatalf("first request StoreResponse = false, want true")
	}
	if fp.Requests[1].StoreResponse || fp.Requests[1].PreviousResponseID != "" {
		t.Fatalf("retry request state = store %v prev %q, want stateless", fp.Requests[1].StoreResponse, fp.Requests[1].PreviousResponseID)
	}
	if got := sink.text.String(); got != "recovered" {
		t.Fatalf("text = %q, want recovered", got)
	}
	if state := a.ResponseState(); state != nil {
		t.Fatalf("response state = %+v, want nil", state)
	}
	if !slices.Contains(sink.notices, "[responses state disabled: provider rejected stored responses; retrying stateless]") {
		t.Fatalf("notices = %+v, want responses-state disabled notice", sink.notices)
	}
	if got := sink.completedTurns[0]; got.Attempts != 2 || got.Wasted.InputTokens != 14 || got.Usage.InputTokens != 20 {
		t.Fatalf("turn usage = %+v, want attempts=2 wasted=14 total=20", got)
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}}; !slices.Equal(sink.abandoned, want) {
		t.Fatalf("abandoned = %+v, want %+v", sink.abandoned, want)
	}
}

func TestMidStreamRetrySucceedsOnSecondAttempt(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				textDelta("partial "),
				{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 40}},
			},
			Err: &llm.APIError{StatusCode: 503, Message: "service unavailable", Retryable: true},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("hello")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 5},
		},
	)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d:\n%s", len(msgs), dump(msgs))
	}
	if got := msgs[1].Content[0].Text; got != "hello" {
		t.Errorf("assistant text = %q, want %q (failed attempt must not be committed)", got, "hello")
	}
	if len(fp.Requests) != 2 {
		t.Errorf("provider called %d times, want 2", len(fp.Requests))
	}
	var retried, surfacedWaste bool
	for _, n := range sink.notices {
		if strings.Contains(n, "retrying turn") {
			retried = true
			if strings.Contains(n, "discarded ~40 tokens") {
				surfacedWaste = true
			}
		}
	}
	if !retried {
		t.Errorf("no retry notice, notices=%v", sink.notices)
	}
	if !surfacedWaste {
		t.Errorf("retry notice should surface the discarded tokens, notices=%v", sink.notices)
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}}; !slices.Equal(sink.abandoned, want) {
		t.Errorf("abandoned attempts = %+v, want %+v", sink.abandoned, want)
	}
	// Wasted usage from the failed attempt is paid for and counted.
	if got := sink.promptUsage[0].Usage.InputTokens; got != 50 {
		t.Errorf("prompt input tokens = %d, want 50 (40 wasted + 10)", got)
	}
	// And it is broken out so the UI can show the retry cost (r51+r52).
	if got := sink.promptUsage[0].Wasted.InputTokens; got != 40 {
		t.Errorf("wasted input tokens = %d, want 40", got)
	}
}

func TestMidStreamRetryHonorsRetryAfter(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Err: &llm.APIError{
				Code:       "rate_limit_exceeded",
				Message:    "rate limited",
				Retryable:  true,
				RetryAfter: 2 * time.Second,
			},
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	var slept []time.Duration
	a.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	sink := &modelRequestRecordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("slept = %v, want [2s]", slept)
	}
	var noticed bool
	for _, n := range sink.notices {
		if strings.Contains(n, "retrying turn in 2s") {
			noticed = true
		}
	}
	if !noticed {
		t.Fatalf("retry notice missing delay, notices=%v", sink.notices)
	}
	if len(sink.events) != 2 {
		t.Fatalf("model request events = %+v, want failed then retry scheduled", sink.events)
	}
	retryEvent := sink.events[1]
	if retryEvent.State != llm.ModelRequestRetryScheduled || retryEvent.Attempt != 2 || retryEvent.MaxAttempts != 3 || retryEvent.RetryDelayMS != 2000 {
		t.Fatalf("retry event = %+v", retryEvent)
	}
}

func TestMidStreamLongRetryAfterFailsWithoutSleeping(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Err: &llm.APIError{
				Code:       "rate_limit_exceeded",
				Message:    "quota exhausted; retry later",
				Retryable:  true,
				RetryAfter: 61 * time.Second,
			},
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("unexpected")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	var slept []time.Duration
	a.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	err := a.RunPrompt(context.Background(), "hi", &recordSink{})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != 61*time.Second {
		t.Fatalf("RunPrompt error = %v, want long Retry-After API error", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	if len(slept) != 0 {
		t.Fatalf("slept = %v, want no long provider-directed sleep", slept)
	}
}

func TestInvalidToolArgumentStreamIsRetried(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolUseStart(0, "call_bad", "git"),
				toolUseDelta(0, `{"args":["commit","-m",`),
			},
			Err: &llm.APIError{Message: `tool "git" produced invalid JSON arguments`, Retryable: true},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("retry recovered")},
			Stop:   llm.StopEndTurn,
		},
	)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "commit", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider called %d times, want 2", len(fp.Requests))
	}
	if len(sink.starts) != 0 || len(sink.results) != 0 {
		t.Fatalf("malformed tool call should not dispatch, starts=%v results=%v", sink.starts, sink.results)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 2 || msgs[1].Content[0].Text != "retry recovered" {
		t.Fatalf("failed attempt leaked into transcript:\n%s", dump(msgs))
	}
}

func TestMidStreamRetryBudgetExhausted(t *testing.T) {
	fail := llmtest.Step{Err: &llm.APIError{StatusCode: 503, Message: "service unavailable", Retryable: true}}
	fp := llmtest.New("fake", fail, fail, fail)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})
	sink := &recordSink{}

	err := a.RunPrompt(context.Background(), "hi", sink)
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("RunPrompt err = %v, want the APIError after budget exhaustion", err)
	}
	if len(fp.Requests) != 3 {
		t.Errorf("provider called %d times, want 3 (1 + 2 retries)", len(fp.Requests))
	}
	mustValid(t, a.Transcript())
}

func TestCompatibilityDiagnosticEmittedOnceAfterRetryExhaustion(t *testing.T) {
	diagnostic := &llm.APIErrorDiagnostic{
		Stage:          llm.APIErrorStageUpstreamHTTP,
		ProxyRequestID: 42,
		TargetID:       "openai:vision",
		TraceID:        "trace-123",
		Compatibility: &llm.CompatibilityDiagnostic{
			Category:    llm.CompatibilityCategoryMultimodalToolResultRejected,
			Reason:      "image_unsupported",
			Confidence:  llm.CompatibilityConfidenceLikely,
			Remediation: "Use an image-capable target.",
		},
	}
	fail := llmtest.Step{Err: &llm.APIError{StatusCode: 503, Code: "invalid_request", Message: "sanitized provider error", Retryable: true, Diagnostic: diagnostic}}
	fp := llmtest.New("fake", fail, fail, fail)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})
	sink := &diagnosticRecordSink{}

	err := a.RunPromptContentWithContext(context.Background(), "hi", nil, nil, 7, sink)
	if err == nil {
		t.Fatal("RunPrompt should fail")
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider called %d times, want 3", len(fp.Requests))
	}
	if len(sink.diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one", sink.diagnostics)
	}
	got := sink.diagnostics[0]
	if got.Prompt != 7 || got.Turn != 1 || got.Attempt != 3 || got.StatusCode != 503 || got.Diagnostic.ProxyRequestID != 42 {
		t.Fatalf("diagnostic = %+v", got)
	}
	compatibilityNotices := 0
	for _, notice := range sink.notices {
		if strings.Contains(notice, "model compatibility:") {
			compatibilityNotices++
			for _, want := range []string{"openai:vision", llm.CompatibilityCategoryMultimodalToolResultRejected, "Use an image-capable target.", "proxy request 42", "trace trace-123"} {
				if !strings.Contains(notice, want) {
					t.Fatalf("notice %q missing %q", notice, want)
				}
			}
			if strings.Contains(notice, "sanitized provider error") {
				t.Fatalf("notice repeated provider message: %q", notice)
			}
		}
	}
	if compatibilityNotices != 1 {
		t.Fatalf("compatibility notices = %d in %v, want 1", compatibilityNotices, sink.notices)
	}
}

func TestUnclassifiedAPIErrorDoesNotEmitCompatibilityDiagnostic(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Err: &llm.APIError{StatusCode: 400, Message: "old proxy error"}})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &diagnosticRecordSink{}
	if err := a.RunPrompt(context.Background(), "hi", sink); err == nil {
		t.Fatal("RunPrompt should fail")
	}
	if len(sink.diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v, want none", sink.diagnostics)
	}
	for _, notice := range sink.notices {
		if strings.Contains(notice, "model compatibility:") {
			t.Fatalf("unexpected compatibility notice: %q", notice)
		}
	}
}

func TestMidStreamRetryBudgetExhaustedDropsPartialText(t *testing.T) {
	fail := llmtest.Step{
		Events: []llm.StreamEvent{textDelta("I'll inspect the repo.")},
		Err:    &llm.APIError{Message: `tool "probe" produced invalid arguments: invalid JSON`, Retryable: true},
	}
	fp := llmtest.New("fake", fail, fail, fail)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})

	err := a.RunPrompt(context.Background(), "debug this", &recordSink{})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("RunPrompt err = %v, want the APIError after budget exhaustion", err)
	}
	if len(fp.Requests) != 3 {
		t.Errorf("provider called %d times, want 3 (1 + 2 retries)", len(fp.Requests))
	}

	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("failed stream should leave only the user message, got %d:\n%s", len(msgs), dump(msgs))
	}
}

func TestProviderErrorRunsTerminalStopHook(t *testing.T) {
	payloadPath := filepath.Join(t.TempDir(), "stop-payload.json")
	command := fmt.Sprintf("cat >> %q; printf '{}'", payloadPath)
	runner := testHookRunner(t, fmt.Sprintf(`{"Stop":[{"hooks":[{"type":"command","command":%q}]}]}`, command))
	providerErr := &llm.APIError{StatusCode: 429, Message: "slow down", Retryable: true}
	fp := llmtest.New("fake", llmtest.Step{Err: providerErr})
	a := newAgent(fp, tools.Default(), Options{Hooks: runner})

	err := a.RunPrompt(context.Background(), "hi", &recordSink{})
	if !errors.Is(err, providerErr) {
		t.Fatalf("RunPrompt err = %v, want provider error", err)
	}
	payloadBytes, readErr := os.ReadFile(payloadPath)
	if readErr != nil {
		t.Fatalf("read Stop payload: %v", readErr)
	}
	payloadLines := strings.Split(strings.TrimSpace(string(payloadBytes)), "\n")
	if len(payloadLines) != 1 {
		t.Fatalf("Stop hook calls = %d, want exactly 1", len(payloadLines))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadLines[0]), &payload); err != nil {
		t.Fatalf("decode Stop payload: %v", err)
	}
	if payload["hook_event_name"] != string(hooks.Stop) {
		t.Fatalf("hook_event_name = %v, want %q", payload["hook_event_name"], hooks.Stop)
	}
	if canBlock, ok := payload["can_block"].(bool); !ok || canBlock {
		t.Fatalf("can_block = %v, want false", payload["can_block"])
	}
}

func TestCanceledPromptRunsTerminalStopHook(t *testing.T) {
	payloadPath := filepath.Join(t.TempDir(), "stop-payload.json")
	command := fmt.Sprintf("cat >> %q; printf '{}'", payloadPath)
	runner := testHookRunner(t, fmt.Sprintf(`{"Stop":[{"hooks":[{"type":"command","command":%q}]}]}`, command))
	ctx, cancel := context.WithCancel(context.Background())
	fp := llmtest.New("fake", llmtest.Step{Block: func(context.Context) { cancel() }})
	a := newAgent(fp, tools.Default(), Options{Hooks: runner})

	err := a.RunPrompt(ctx, "hi", &recordSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunPrompt err = %v, want context canceled", err)
	}
	payloadBytes, readErr := os.ReadFile(payloadPath)
	if readErr != nil {
		t.Fatalf("read Stop payload: %v", readErr)
	}
	payloadLines := strings.Split(strings.TrimSpace(string(payloadBytes)), "\n")
	if len(payloadLines) != 1 {
		t.Fatalf("Stop hook calls = %d, want exactly 1", len(payloadLines))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadLines[0]), &payload); err != nil {
		t.Fatalf("decode Stop payload: %v", err)
	}
	if canBlock, ok := payload["can_block"].(bool); !ok || canBlock {
		t.Fatalf("can_block = %v, want false", payload["can_block"])
	}
}

func TestHardBudgetMakesNormalStopHookNonBlockable(t *testing.T) {
	payloadPath := filepath.Join(t.TempDir(), "stop-payload.json")
	command := fmt.Sprintf("cat > %q; printf '{\"decision\":\"block\",\"reason\":\"continue\"}'", payloadPath)
	runner := testHookRunner(t, fmt.Sprintf(`{"Stop":[{"hooks":[{"type":"command","command":%q}]}]}`, command))
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("done")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 10},
	})
	a := newAgent(fp, tools.Default(), Options{Hooks: runner, MaxPromptTokens: 1})

	if err := a.RunPrompt(context.Background(), "hi", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	payloadBytes, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read Stop payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("decode Stop payload: %v", err)
	}
	if canBlock, ok := payload["can_block"].(bool); !ok || canBlock {
		t.Fatalf("can_block = %v, want false", payload["can_block"])
	}
}

func TestRateLimitedStreamNotRetried(t *testing.T) {
	// A connect-exhausted rate-limit error (HTTP 429/529, status code set) must not
	// be re-run by the agent: the provider's connect loop already spent its full
	// attempt budget on it, so re-running would only multiply attempts (up to
	// 3×5=15) and hammer a busy API (r46). Transient 500/502/503 still retry.
	for _, code := range []int{429, 529} {
		fail := llmtest.Step{Err: &llm.APIError{StatusCode: code, Message: "slow down", Retryable: true}}
		fp := llmtest.New("fake", fail, fail, fail)
		a := newAgent(fp, tools.Default(), Options{})
		a.SetSleep(func(time.Duration) {})

		err := a.RunPrompt(context.Background(), "hi", &recordSink{})
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != code {
			t.Fatalf("status %d: RunPrompt err = %v, want the %d APIError", code, err, code)
		}
		if len(fp.Requests) != 1 {
			t.Errorf("status %d: provider called %d times, want 1 (rate limit not re-multiplied)", code, len(fp.Requests))
		}
	}
}

func TestMidStreamNonRetryableNotRetried(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Err: &llm.APIError{StatusCode: 400, Message: "bad request", Retryable: false}},
	)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})
	sink := &recordSink{}

	err := a.RunPrompt(context.Background(), "hi", sink)
	if err == nil {
		t.Fatal("RunPrompt should fail")
	}
	if len(fp.Requests) != 1 {
		t.Errorf("provider called %d times, want 1 (no retry)", len(fp.Requests))
	}
	assertPromptTermination(t, sink, TerminationError)
}

func TestTruncatedStreamRetried(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Err: fmt.Errorf("stream ended early: %w", sse.ErrTruncatedStream)},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})

	if err := a.RunPrompt(context.Background(), "hi", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Errorf("provider called %d times, want 2", len(fp.Requests))
	}
}

func TestCancellationDuringRetryBackoff(t *testing.T) {
	// A retryable failure schedules a retry; cancellation arrives during the
	// backoff sleep, before the next attempt. The loop must honor it: return
	// context.Canceled, attempt no further request, and leave a valid transcript.
	fail := llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 13, OutputTokens: 2}}},
		Err:    &llm.APIError{StatusCode: 503, Message: "service unavailable", Retryable: true},
	}
	fp := llmtest.New("fake", fail, fail, fail)
	a := newAgent(fp, tools.Default(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	sink := &recordSink{}
	err := a.RunPrompt(ctx, "hi", sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunPrompt err = %v, want context.Canceled", err)
	}
	// One real attempt, then cancellation during the backoff stops the loop before
	// any retry re-requests the step. Its absolute attempt metadata and billed
	// usage remain observable even though its output was abandoned.
	if len(fp.Requests) != 1 {
		t.Errorf("provider called %d times, want 1 (no retry after cancel)", len(fp.Requests))
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}}; !slices.Equal(sink.attemptStarts, want) {
		t.Fatalf("attempt starts = %+v, want %+v", sink.attemptStarts, want)
	}
	if len(sink.attemptUsage) != 1 || sink.attemptUsage[0].Attempt != 1 || sink.attemptUsage[0].Usage.InputTokens != 13 {
		t.Fatalf("attempt completions = %+v, want physical attempt 1 usage", sink.attemptUsage)
	}
	if want := []turnAttemptEvent{{turn: 1, attempt: 1}}; !slices.Equal(sink.abandoned, want) {
		t.Fatalf("abandoned = %+v, want %+v", sink.abandoned, want)
	}
	if got := sink.promptUsage[0]; got.Usage.InputTokens != 13 || got.Usage.OutputTokens != 2 || got.Wasted.InputTokens != 13 || got.Wasted.OutputTokens != 2 {
		t.Fatalf("prompt usage = %+v, want canceled attempt billed and wasted exactly once", got)
	}
	mustValid(t, a.Transcript())
}

func TestZeroedFinalUsageFrameDoesNotEraseEarlier(t *testing.T) {
	// The Done event carries zero usage (FakeProvider appends Done with
	// step.Usage, here the zero value); the mid-stream snapshot must survive.
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 7}},
			textDelta("hi"),
		},
		Stop: llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	u := sink.promptUsage[0].Usage
	if u.InputTokens != 100 || u.OutputTokens != 10 || u.CacheReadTokens != 7 {
		t.Errorf("usage = %+v, want the mid-stream snapshot preserved", u)
	}
}

func specNames(specs []llm.ToolSchema) []string {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return names
}

func idsFromCalls(calls []llm.ToolCall) []string {
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = c.ID
	}
	return ids
}

func idsFromResults(results []llm.ToolResult) []string {
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.ForID
	}
	return ids
}

func TestKimiWebSearchToolCallPassesThroughArguments(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "call_web", "$web_search", `{"query":"current docs","_stage":"provider-owned"}`),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
		},
	)
	a := newAgent(fp, tools.Catalog(), Options{
		ServerTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch}},
	})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.results) != 1 {
		t.Fatalf("tool results = %+v, want one", sink.results)
	}
	if sink.results[0].IsError || sink.results[0].Text != `{"query":"current docs","_stage":"provider-owned"}` {
		t.Fatalf("kimi web_search result = %+v, want passthrough arguments", sink.results[0])
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	msgs := fp.Requests[1].Messages
	if len(msgs) == 0 || len(msgs[len(msgs)-1].Content) != 1 || msgs[len(msgs)-1].Content[0].ResultText != `{"query":"current docs","_stage":"provider-owned"}` {
		t.Fatalf("second request messages = %s", dump(msgs))
	}
}

func TestIsKimiWebSearchCall(t *testing.T) {
	cases := []struct {
		name        string
		callName    string
		serverTools []llm.ServerTool
		want        bool
	}{
		{name: "kimi kind echoes", callName: "$web_search", serverTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindKimiWebSearch}}, want: true},
		{name: "untagged web_search falls back", callName: "$web_search", serverTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch}}, want: true},
		{name: "non-kimi kind is not echoed", callName: "$web_search", serverTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindOpenAIWebSearch}}, want: false},
		{name: "wrong call name", callName: "web_search", serverTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindKimiWebSearch}}, want: false},
		{name: "no server tools", callName: "$web_search", serverTools: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAgent(llmtest.New("fake"), tools.Catalog(), Options{ServerTools: tc.serverTools})
			if got := a.isKimiWebSearchCall(llm.ToolCall{ID: "c", Name: tc.callName}); got != tc.want {
				t.Fatalf("isKimiWebSearchCall(%q) = %v, want %v", tc.callName, got, tc.want)
			}
		})
	}
}

func TestRequestCarriesResolvedModel(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(fp.Requests) != 1 {
		t.Fatalf("provider called %d times, want 1", len(fp.Requests))
	}
	if got := fp.Requests[0].Model; got != "claude-opus-4-8" {
		t.Errorf("Request.Model = %q, want %q", got, "claude-opus-4-8")
	}
}

// barrierRun returns a Run that only completes once n calls have entered it —
// it deadlocks (then errors via timeout) under sequential dispatch.
func barrierRun(n int) func(context.Context, json.RawMessage) (string, error) {
	var wg sync.WaitGroup
	wg.Add(n)
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		wg.Done()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
			return "ok", nil
		case <-time.After(2 * time.Second):
			return "", errors.New("barrier timeout: calls were not concurrent")
		}
	}
}

func TestPlanToolStagesResolvesInheritanceAndPreservesUnannotatedInput(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "a", Name: "read", Input: json.RawMessage(` {"path":"a"} `)},
		{ID: "b", Name: "read", Input: json.RawMessage(`{"_stage":3,"path":"b"}`)},
		{ID: "c", Name: "read", Input: json.RawMessage(`{"path":"c"}`)},
		{ID: "d", Name: "read", Input: json.RawMessage(`{"_stage":7,"path":"d"}`)},
	}
	execution, stages, err := planToolStages(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(execution[0].Input); got != ` {"path":"a"} ` {
		t.Fatalf("unannotated input re-encoded: %q", got)
	}
	if got := string(execution[1].Input); got != `{"path":"b"}` {
		t.Fatalf("explicit stage not stripped: %q", got)
	}
	if got := string(execution[2].Input); got != `{"path":"c"}` {
		t.Fatalf("inherited-stage input changed: %q", got)
	}
	if got := string(execution[3].Input); got != `{"path":"d"}` {
		t.Fatalf("later stage not stripped: %q", got)
	}
	wantStages := []callStage{{start: 0, end: 1}, {start: 1, end: 3}, {start: 3, end: 4}}
	if !slices.Equal(stages, wantStages) {
		t.Fatalf("stages = %+v, want %+v", stages, wantStages)
	}
	if string(calls[1].Input) != `{"_stage":3,"path":"b"}` {
		t.Fatalf("raw call input mutated: %s", calls[1].Input)
	}
}

type stageRecordSink struct {
	recordSink
	resultCount atomic.Int32
}

func (s *stageRecordSink) ToolResult(result llm.ToolResult) {
	s.recordSink.ToolResult(result)
	s.resultCount.Add(1)
}

func TestPlanToolStagesKeepsProviderInvalidInputInCurrentStage(t *testing.T) {
	invalid := llm.ToolCall{
		ID:                "bad",
		Name:              "read",
		Input:             llm.InvalidToolInputObject(errors.New("unexpected EOF")),
		InvalidInputError: "unexpected EOF",
	}
	execution, stages, err := planToolStages([]llm.ToolCall{
		{ID: "a", Name: "read", Input: json.RawMessage(`{"_stage":4}`)},
		invalid,
		{ID: "c", Name: "read", Input: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatalf("provider invalid input replaced by stage error: %v", err)
	}
	if len(stages) != 1 || stages[0] != (callStage{start: 0, end: 3}) {
		t.Fatalf("stages = %+v, want one inherited stage", stages)
	}
	if execution[1].InvalidInputError != invalid.InvalidInputError || string(execution[1].Input) != string(invalid.Input) {
		t.Fatalf("provider invalid input changed: %+v", execution[1])
	}
}

func TestDispatchCallsRunsStagesSeriallyAndCallsWithinStagesConcurrently(t *testing.T) {
	stage1Started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	stage2Started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	releaseStage1 := make(chan struct{})
	releaseStage2 := make(chan struct{})
	stage2Barrier := barrierRun(2)
	reg := &tools.Registry{}
	for i, name := range []string{"a", "b"} {
		started := stage1Started[i]
		reg.Register(&meteredRecordTool{
			recordTool: &recordTool{name: name, run: func(context.Context, json.RawMessage) (string, error) {
				close(started)
				<-releaseStage1
				return "stage 1", nil
			}},
			usage: llm.Usage{InputTokens: i + 1, OutputTokens: 1},
		})
	}
	for i, name := range []string{"c", "d"} {
		started := stage2Started[i]
		reg.Register(&meteredRecordTool{
			recordTool: &recordTool{name: name, run: func(ctx context.Context, input json.RawMessage) (string, error) {
				close(started)
				result, err := stage2Barrier(ctx, input)
				<-releaseStage2
				return result, err
			}},
			usage: llm.Usage{InputTokens: i + 3, OutputTokens: 1},
		})
	}
	a := newAgent(llmtest.New("fake"), reg, Options{})
	sink := &stageRecordSink{}
	type dispatchOutcome struct {
		blocks  []llm.ContentBlock
		batches []llm.ParallelToolBatch
		usage   llm.Usage
	}
	done := make(chan dispatchOutcome, 1)
	calls := []llm.ToolCall{
		{ID: "a", Name: "a", Input: json.RawMessage(`{"_stage":1}`)},
		{ID: "b", Name: "b", Input: json.RawMessage(`{"_stage":1}`)},
		{ID: "c", Name: "c", Input: json.RawMessage(`{"_stage":2}`)},
		{ID: "d", Name: "d", Input: json.RawMessage(`{}`)},
	}
	go func() {
		blocks, batches, usage := a.dispatchCalls(context.Background(), calls, 1, 1, sink)
		done <- dispatchOutcome{blocks: blocks, batches: batches, usage: usage}
	}()

	awaitSignal(t, stage1Started[0], "stage-1 call a start")
	awaitSignal(t, stage1Started[1], "stage-1 call b start")
	assertNotSignaled(t, stage2Started[0], "stage 2 started before stage 1 settled")
	assertNotSignaled(t, stage2Started[1], "stage 2 started before stage 1 settled")
	close(releaseStage1)
	awaitSignal(t, stage2Started[0], "stage-2 call c start")
	awaitSignal(t, stage2Started[1], "stage-2 call d start")
	if got := sink.resultCount.Load(); got != 2 {
		t.Fatalf("stage 2 started after %d stage-1 results, want 2", got)
	}
	close(releaseStage2)
	outcome := <-done
	if got := idsFromCalls(sink.starts); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("ToolStart order = %v", got)
	}
	if got := idsFromResults(sink.results); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("ToolResult order = %v", got)
	}
	for i, block := range outcome.blocks {
		if block.ResultError || block.ResultForID != calls[i].ID {
			t.Fatalf("block %d = %+v", i, block)
		}
	}
	if len(outcome.batches) != 2 ||
		!slices.Equal(outcome.batches[0].ToolUseIDs, []string{"a", "b"}) ||
		!slices.Equal(outcome.batches[1].ToolUseIDs, []string{"c", "d"}) {
		t.Fatalf("parallel batches = %+v", outcome.batches)
	}
	if outcome.usage.InputTokens != 10 || outcome.usage.OutputTokens != 4 {
		t.Fatalf("global usage = %+v, want input=10 output=4", outcome.usage)
	}
	for _, start := range sink.starts {
		if strings.Contains(string(start.Input), "_stage") {
			t.Fatalf("ToolStart saw scheduling metadata: %+v", start)
		}
	}
}

func TestRichResultBudgetRemainsGlobalAcrossStages(t *testing.T) {
	tool := &richRecordTool{
		recordTool: &recordTool{name: "image", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
			return "legacy", nil
		}},
		modality: "image",
		result: tools.RichResult{
			Text: "attached",
			Content: []llm.ContentBlock{{
				Kind:           llm.BlockImage,
				ImageMediaType: "image/png",
				ImageData:      agentOnePixelPNG,
			}},
		},
	}
	reg := &tools.Registry{}
	reg.Register(tool)
	models := llm.NewRegistry(map[string]llm.ModelInfo{
		"vision-model": {InputModalities: []string{"text", "image"}},
	})
	a := newAgent(llmtest.New("fake"), reg, Options{Model: "vision-model", Registry: models})
	calls := []llm.ToolCall{
		{ID: "first", Name: "image", Input: json.RawMessage(`{}`)},
		{ID: "second", Name: "image", Input: json.RawMessage(`{}`)},
	}
	blocks := make([]llm.ContentBlock, len(calls))
	richEncodedBytes := inputimage.MaxTotalEncodedBytes - len(agentOnePixelPNG)
	crossStageDependencies := make([][]int, len(calls))
	actualCompletions := make([]<-chan struct{}, len(calls))
	for _, stage := range []callStage{{start: 0, end: 1}, {start: 1, end: 2}} {
		a.dispatchCallStage(context.Background(), calls, stage, 1, 1, &recordSink{}, blocks, &richEncodedBytes, crossStageDependencies, actualCompletions)
	}
	if blocks[0].ResultError || len(blocks[0].ResultContent) != 1 {
		t.Fatalf("first-stage rich result = %+v", blocks[0])
	}
	if !blocks[1].ResultError || len(blocks[1].ResultContent) != 0 || !strings.Contains(blocks[1].ResultText, "encoded total") {
		t.Fatalf("second-stage rich result did not share budget: %+v", blocks[1])
	}
}

func TestFailureGuardFoldingRemainsGlobalAcrossStages(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "check", run: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("check failed")
	}})
	reg.Register(&mutationRecordTool{
		recordTool: &recordTool{name: "mutation", run: func(context.Context, json.RawMessage) (string, error) {
			return "changed", nil
		}},
		paths: func(json.RawMessage) ([]string, error) { return []string{"changed.txt"}, nil },
	})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	a.failGuard = newFailureGuard()
	a.dispatchCalls(context.Background(), []llm.ToolCall{
		{ID: "failure", Name: "check", Input: json.RawMessage(`{"_stage":1}`)},
		{ID: "mutation", Name: "mutation", Input: json.RawMessage(`{"_stage":2}`)},
	}, 1, 1, &recordSink{})
	a.failGuard.mu.Lock()
	defer a.failGuard.mu.Unlock()
	if len(a.failGuard.records) != 0 {
		t.Fatalf("failure guard retained earlier-stage failure after mutation reset: %+v", a.failGuard.records)
	}
}

func TestIndependentWriteEditPathsStartConcurrently(t *testing.T) {
	run := barrierRun(2)
	pathReporter := func(input json.RawMessage) ([]string, error) {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, err
		}
		return []string{args.Path}, nil
	}
	reg := &tools.Registry{}
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "write", run: run}, paths: pathReporter})
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "edit", run: run}, paths: pathReporter})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	blocks, batches, _ := a.dispatchCalls(context.Background(), []llm.ToolCall{
		{ID: "write-a", Name: "write", Input: json.RawMessage(`{"path":"a.txt"}`)},
		{ID: "edit-b", Name: "edit", Input: json.RawMessage(`{"path":"b.txt"}`)},
	}, 1, 1, &recordSink{})
	if len(blocks) != 2 || blocks[0].ResultError || blocks[1].ResultError {
		t.Fatalf("independent mutation results = %+v", blocks)
	}
	if len(batches) != 1 || !slices.Equal(batches[0].ToolUseIDs, []string{"write-a", "edit-b"}) {
		t.Fatalf("parallel batches = %+v, want one ordered write/edit island", batches)
	}
}

func TestSamePathMutationWaitsWhileUnrelatedCallRuns(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	successorStarted := make(chan struct{})
	unrelatedStarted := make(chan struct{})

	paths := func(input json.RawMessage) ([]string, error) {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, err
		}
		return []string{args.Path}, nil
	}
	reg := &tools.Registry{}
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "write", run: func(context.Context, json.RawMessage) (string, error) {
		close(firstStarted)
		<-releaseFirst
		return "", errors.New("first mutation failed")
	}}, paths: paths})
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "edit", run: func(context.Context, json.RawMessage) (string, error) {
		close(successorStarted)
		return "edited", nil
	}}, paths: paths})
	reg.Register(&recordTool{name: "shell", run: func(context.Context, json.RawMessage) (string, error) {
		close(unrelatedStarted)
		return "ran", nil
	}})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	done := make(chan []llm.ParallelToolBatch, 1)
	go func() {
		_, batches, _ := a.dispatchCalls(context.Background(), []llm.ToolCall{
			{ID: "first", Name: "write", Input: json.RawMessage(`{"path":"same.txt"}`)},
			{ID: "successor", Name: "edit", Input: json.RawMessage(`{"path":"same.txt"}`)},
			{ID: "other", Name: "shell", Input: json.RawMessage(`{}`)},
		}, 1, 1, &recordSink{})
		done <- batches
	}()
	awaitSignal(t, firstStarted, "first mutation start")
	awaitSignal(t, unrelatedStarted, "unrelated call start")
	assertNotSignaled(t, successorStarted, "same-path successor started early")
	close(releaseFirst)
	awaitSignal(t, successorStarted, "same-path successor start")
	batches := <-done
	if len(batches) != 1 || !slices.Equal(batches[0].ToolUseIDs, []string{"first", "successor", "other"}) {
		t.Fatalf("parallel batches = %+v, want mixed scheduling island", batches)
	}
}

func TestTimedOutMutationDoesNotReleaseSuccessorBeforeActualCompletion(t *testing.T) {
	firstStarted := make(chan struct{})
	deadlineObserved := make(chan struct{})
	releaseFirst := make(chan struct{})
	successorStarted := make(chan struct{})
	unrelatedStarted := make(chan struct{})
	paths := func(json.RawMessage) ([]string, error) { return []string{"same.txt"}, nil }
	reg := &tools.Registry{}
	reg.SetDispatchTimeout(20 * time.Millisecond)
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "first", run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(firstStarted)
		go func() {
			<-ctx.Done()
			close(deadlineObserved)
		}()
		<-releaseFirst // deliberately ignore ctx until the test releases actual execution
		return "late mutation", nil
	}}, paths: paths})
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "successor", run: func(context.Context, json.RawMessage) (string, error) {
		close(successorStarted)
		return "successor mutation", nil
	}}, paths: paths})
	reg.Register(&recordTool{name: "unrelated", run: func(context.Context, json.RawMessage) (string, error) {
		close(unrelatedStarted)
		return "unrelated", nil
	}})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	type dispatchOutcome struct {
		blocks  []llm.ContentBlock
		batches []llm.ParallelToolBatch
	}
	done := make(chan dispatchOutcome, 1)
	go func() {
		blocks, batches, _ := a.dispatchCalls(context.Background(), []llm.ToolCall{
			{ID: "first", Name: "first", Input: json.RawMessage(`{"_stage":1}`)},
			{ID: "successor", Name: "successor", Input: json.RawMessage(`{"_stage":2}`)},
			{ID: "unrelated", Name: "unrelated", Input: json.RawMessage(`{}`)},
		}, 1, 1, &recordSink{})
		done <- dispatchOutcome{blocks: blocks, batches: batches}
	}()
	awaitSignal(t, firstStarted, "first mutation start")
	awaitSignal(t, deadlineObserved, "first mutation dispatch timeout")
	awaitSignal(t, unrelatedStarted, "unrelated stage-2 call after timeout result")
	assertNotSignaled(t, successorStarted, "successor overtook timed-out mutation")
	close(releaseFirst)
	awaitSignal(t, successorStarted, "successor start after actual completion")
	outcome := <-done
	if len(outcome.blocks) != 3 || !outcome.blocks[0].ResultError || !strings.Contains(outcome.blocks[0].ResultText, "timed out after 20ms") || outcome.blocks[1].ResultError || outcome.blocks[2].ResultError {
		t.Fatalf("timeout/successor results = %+v", outcome.blocks)
	}
	if len(outcome.batches) != 1 || !slices.Equal(outcome.batches[0].ToolUseIDs, []string{"successor", "unrelated"}) {
		t.Fatalf("parallel metadata = %+v, want concurrent stage-2 island", outcome.batches)
	}
}

func TestTimedOutNonconflictingStageReleasesLaterStageOnResult(t *testing.T) {
	firstStarted := make(chan struct{})
	deadlineObserved := make(chan struct{})
	releaseFirst := make(chan struct{})
	laterStarted := make(chan struct{})
	reg := &tools.Registry{}
	reg.SetDispatchTimeout(20 * time.Millisecond)
	reg.Register(&recordTool{name: "first", run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(firstStarted)
		go func() {
			<-ctx.Done()
			close(deadlineObserved)
		}()
		<-releaseFirst
		return "late", nil
	}})
	reg.Register(&recordTool{name: "later", run: func(context.Context, json.RawMessage) (string, error) {
		close(laterStarted)
		return "later", nil
	}})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	done := make(chan []llm.ContentBlock, 1)
	go func() {
		blocks, _, _ := a.dispatchCalls(context.Background(), []llm.ToolCall{
			{ID: "first", Name: "first", Input: json.RawMessage(`{"_stage":1}`)},
			{ID: "later", Name: "later", Input: json.RawMessage(`{"_stage":2}`)},
		}, 1, 1, &recordSink{})
		done <- blocks
	}()
	awaitSignal(t, firstStarted, "stage-1 call start")
	awaitSignal(t, deadlineObserved, "stage-1 timeout")
	awaitSignal(t, laterStarted, "nonconflicting later stage start after timeout result")
	close(releaseFirst)
	blocks := <-done
	if len(blocks) != 2 || !blocks[0].ResultError || !strings.Contains(blocks[0].ResultText, "timed out after 20ms") || blocks[1].ResultError {
		t.Fatalf("staged timeout results = %+v", blocks)
	}
}

func TestMutationPathAliasesConflict(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	rel := filepath.Join("nested", "..", "file.txt")
	abs, err := filepath.Abs("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	dependencies, concurrent := newAgent(llmtest.New("fake"), mutationRegistryForTest(), Options{}).mutationDependencies([]llm.ToolCall{
		{ID: "relative", Name: "mutation", Input: json.RawMessage(fmt.Sprintf(`{"path":%q}`, rel))},
		{ID: "absolute", Name: "mutation", Input: json.RawMessage(fmt.Sprintf(`{"path":%q}`, abs))},
	})
	if concurrent || len(dependencies[1]) != 1 || dependencies[1][0] != 0 {
		t.Fatalf("alias dependencies = %v, concurrent=%v; want second depend on first", dependencies, concurrent)
	}
}

func TestMultiFileMutationWaitsForEveryLatestPredecessor(t *testing.T) {
	startedA := make(chan struct{})
	startedB := make(chan struct{})
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	multiStarted := make(chan struct{})
	paths := func(input json.RawMessage) ([]string, error) {
		var args struct {
			Paths []string `json:"paths"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, err
		}
		return args.Paths, nil
	}
	reg := &tools.Registry{}
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "first-a", run: func(context.Context, json.RawMessage) (string, error) {
		close(startedA)
		<-releaseA
		return "a", nil
	}}, paths: paths})
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "first-b", run: func(context.Context, json.RawMessage) (string, error) {
		close(startedB)
		<-releaseB
		return "b", nil
	}}, paths: paths})
	reg.Register(&mutationRecordTool{recordTool: &recordTool{name: "multi", run: func(context.Context, json.RawMessage) (string, error) {
		close(multiStarted)
		return "multi", nil
	}}, paths: paths})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	done := make(chan struct{})
	go func() {
		a.dispatchCalls(context.Background(), []llm.ToolCall{
			{ID: "a", Name: "first-a", Input: json.RawMessage(`{"paths":["a.txt"]}`)},
			{ID: "b", Name: "first-b", Input: json.RawMessage(`{"paths":["b.txt"]}`)},
			{ID: "multi", Name: "multi", Input: json.RawMessage(`{"paths":["a.txt","b.txt","a.txt"]}`)},
		}, 1, 1, &recordSink{})
		close(done)
	}()
	awaitSignal(t, startedA, "first a start")
	awaitSignal(t, startedB, "first b start")
	assertNotSignaled(t, multiStarted, "multi-file mutation started before predecessors")
	close(releaseA)
	assertNotSignaled(t, multiStarted, "multi-file mutation ignored b predecessor")
	close(releaseB)
	awaitSignal(t, multiStarted, "multi-file mutation start")
	awaitSignal(t, done, "dispatch completion")
}

func mutationRegistryForTest() *tools.Registry {
	reg := &tools.Registry{}
	reg.Register(&mutationRecordTool{
		recordTool: &recordTool{name: "mutation", run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }},
		paths: func(input json.RawMessage) ([]string, error) {
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, err
			}
			return []string{args.Path}, nil
		},
	})
	return reg
}

func awaitSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func assertNotSignaled(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(label)
	default:
	}
}

func TestInvalidToolStagePlanRejectsWholeBatchBeforeToolsOrHooks(t *testing.T) {
	tests := []struct {
		name   string
		inputs []string
	}{
		{name: "invalid value", inputs: []string{`{"_stage":"later","value":1}`, `{"value":2}`}},
		{name: "decreasing stages", inputs: []string{`{"_stage":2,"value":1}`, `{"_stage":1,"value":2}`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var runs atomic.Int32
			tool := &recordTool{name: "capture", run: func(context.Context, json.RawMessage) (string, error) {
				runs.Add(1)
				return "ran", nil
			}}
			reg := &tools.Registry{}
			reg.Register(tool)
			hookMarker := filepath.Join(t.TempDir(), "hook-ran")
			runner := testHookRunner(t, fmt.Sprintf(`{"PreToolUse":[{"hooks":[{"type":"command","command":%q}]}]}`, "printf ran > \""+hookMarker+"\"; printf '{}'"))
			fp := llmtest.New("fake",
				llmtest.Step{Events: []llm.StreamEvent{
					toolDone(0, "a", "capture", tc.inputs[0]),
					toolDone(1, "b", "capture", tc.inputs[1]),
				}, Stop: llm.StopToolUse},
				llmtest.Step{Events: []llm.StreamEvent{textDelta("recovered")}, Stop: llm.StopEndTurn},
			)
			a := newAgent(fp, reg, Options{Hooks: runner})
			sink := &recordSink{}
			if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
				t.Fatal(err)
			}
			mustValid(t, a.Transcript())
			if runs.Load() != 0 || len(tool.inputs) != 0 {
				t.Fatalf("tool ran for invalid stage plan: runs=%d inputs=%v", runs.Load(), tool.inputs)
			}
			if _, err := os.Stat(hookMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("hook ran for invalid stage plan: stat err=%v", err)
			}
			if len(sink.starts) != 2 || len(sink.results) != 2 {
				t.Fatalf("unbalanced sink events: starts=%d results=%d", len(sink.starts), len(sink.results))
			}
			for i, result := range sink.results {
				if !result.IsError || result.ErrorKind != llm.ToolErrorInvalidArgs || !strings.Contains(result.Text, "non-decreasing") || result.ForID != []string{"a", "b"}[i] {
					t.Fatalf("result %d = %+v", i, result)
				}
				if strings.Contains(string(sink.starts[i].Input), "_stage") {
					t.Fatalf("ToolStart %d saw stage metadata: %s", i, sink.starts[i].Input)
				}
			}
			resultMessage := a.Transcript()[2]
			if len(resultMessage.Content) != 2 || len(resultMessage.ParallelToolBatches) != 0 {
				t.Fatalf("invalid-plan result message = %+v", resultMessage)
			}
		})
	}
}

func TestFullTurnPreservesRawStageAndStripsExecutionAndHookInput(t *testing.T) {
	tool := &recordTool{name: "capture", run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	reg := &tools.Registry{}
	reg.Register(tool)
	hookInputPath := filepath.Join(t.TempDir(), "hook-input.json")
	hookCommand := fmt.Sprintf("tee %q >/dev/null; printf '{}'", hookInputPath)
	runner := testHookRunner(t, fmt.Sprintf(`{"PreToolUse":[{"hooks":[{"type":"command","command":%q}]}]}`, hookCommand))
	const rawInput = `{"_stage":2,"value":"kept"}`
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "call", "capture", rawInput)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{Hooks: runner})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatal(err)
	}
	messages := a.Transcript()
	mustValid(t, messages)
	if got := string(messages[1].Content[0].ToolInput); got != rawInput {
		t.Fatalf("assistant transcript input = %s, want raw %s", got, rawInput)
	}
	if len(fp.Requests) != 2 || len(fp.Requests[1].Messages) < 2 || string(fp.Requests[1].Messages[1].Content[0].ToolInput) != rawInput {
		t.Fatalf("provider replay did not preserve raw stage input: %+v", fp.Requests)
	}
	if !slices.Equal(tool.inputs, []string{`{"value":"kept"}`}) {
		t.Fatalf("tool inputs = %v", tool.inputs)
	}
	if len(sink.starts) != 1 || string(sink.starts[0].Input) != `{"value":"kept"}` {
		t.Fatalf("ToolStart inputs = %+v", sink.starts)
	}
	payloadBytes, err := os.ReadFile(hookInputPath)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("hook payload: %v\n%s", err, payloadBytes)
	}
	hookInput, ok := payload["tool_input"].(map[string]any)
	if !ok || hookInput["value"] != "kept" {
		t.Fatalf("hook tool_input = %#v", payload["tool_input"])
	}
	if _, exists := hookInput["_stage"]; exists {
		t.Fatalf("hook saw reserved stage metadata: %#v", hookInput)
	}
	resultMessages := 0
	for _, message := range messages {
		if message.Role == llm.RoleUser && len(message.Content) > 0 && message.Content[0].Kind == llm.BlockToolResult {
			resultMessages++
		}
	}
	if resultMessages != 1 {
		t.Fatalf("tool result user messages = %d, want one\n%s", resultMessages, dump(messages))
	}
}

func TestCancellationInEarlierStageClosesAllCallsInOneValidResultMessage(t *testing.T) {
	firstStarted := make(chan struct{})
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "first", run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(firstStarted)
		<-ctx.Done()
		return "", ctx.Err()
	}})
	reg.Register(&recordTool{name: "later", run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	calls := []llm.ToolCall{
		{ID: "first", Name: "first", Input: json.RawMessage(`{"_stage":1}`)},
		{ID: "later", Name: "later", Input: json.RawMessage(`{"_stage":2}`)},
	}
	a.transcript = []llm.Message{
		a.textMessage(llm.RoleUser, "go"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Kind: llm.BlockToolUse, ToolUseID: "first", ToolName: "first", ToolInput: calls[0].Input},
			{Kind: llm.BlockToolUse, ToolUseID: "later", ToolName: "later", ToolInput: calls[1].Input},
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := &recordSink{}
	done := make(chan []llm.ContentBlock, 1)
	go func() {
		blocks, _, _ := a.dispatchCalls(ctx, calls, 1, 1, sink)
		done <- blocks
	}()
	awaitSignal(t, firstStarted, "stage-1 call start")
	cancel()
	blocks := <-done
	a.transcript = append(a.transcript, llm.Message{Role: llm.RoleUser, Content: blocks})
	mustValid(t, a.Transcript())
	if len(blocks) != 2 || blocks[0].ResultForID != "first" || blocks[1].ResultForID != "later" || !blocks[0].ResultError || !blocks[1].ResultError {
		t.Fatalf("cancellation blocks = %+v", blocks)
	}
	if len(sink.results) != 2 || sink.results[0].ErrorKind != llm.ToolErrorCancelled || sink.results[1].ErrorKind != llm.ToolErrorCancelled {
		t.Fatalf("cancellation results = %+v", sink.results)
	}
	if len(a.Transcript()) != 3 || len(a.Transcript()[2].Content) != 2 {
		t.Fatalf("transcript did not close in one result message: %s", dump(a.Transcript()))
	}
}

func TestOrdinaryMutatingToolsDispatchConcurrentlyByDefault(t *testing.T) {
	run := barrierRun(2)
	t1 := &recordTool{name: "r1", run: run}
	t2 := &recordTool{name: "r2", run: run}
	reg := &tools.Registry{}
	reg.Register(t1)
	reg.Register(t2)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "a", "r1", `{}`),
				toolDone(1, "b", "r2", `{}`),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	resMsg := a.Transcript()[2]
	if len(resMsg.Content) != 2 || resMsg.Content[0].ResultForID != "a" || resMsg.Content[1].ResultForID != "b" {
		t.Fatalf("results not in emission order:\n%s", dump([]llm.Message{resMsg}))
	}
	if resMsg.Content[0].ToolName != "r1" || resMsg.Content[1].ToolName != "r2" {
		t.Fatalf("results missing tool names:\n%s", dump([]llm.Message{resMsg}))
	}
	for _, b := range resMsg.Content {
		if b.ResultError {
			t.Errorf("ordinary mutating calls were not concurrent: %s", b.ResultText)
		}
	}
	if len(resMsg.ParallelToolBatches) != 1 || !slices.Equal(resMsg.ParallelToolBatches[0].ToolUseIDs, []string{"a", "b"}) {
		t.Fatalf("parallel batches = %+v, want one [a b] batch", resMsg.ParallelToolBatches)
	}
	// Sink saw both starts (emission order) before both results.
	if len(sink.starts) != 2 || sink.starts[0].ID != "a" || sink.starts[1].ID != "b" {
		t.Errorf("ToolStart order wrong: %+v", sink.starts)
	}
	if len(sink.results) != 2 || sink.results[0].ForID != "a" || sink.results[1].ForID != "b" {
		t.Errorf("ToolResult order wrong: %+v", sink.results)
	}
}

func TestParallelResultsUsageAndIDsRemainInEmissionOrder(t *testing.T) {
	firstStarted := make(chan struct{})
	secondFinished := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := &meteredRecordTool{
		recordTool: &recordTool{name: "first", run: func(context.Context, json.RawMessage) (string, error) {
			close(firstStarted)
			<-releaseFirst
			return "first result", nil
		}},
		usage: llm.Usage{InputTokens: 2, OutputTokens: 3},
	}
	second := &meteredRecordTool{
		recordTool: &recordTool{name: "second", run: func(context.Context, json.RawMessage) (string, error) {
			close(secondFinished)
			return "second result", nil
		}},
		usage: llm.Usage{InputTokens: 5, OutputTokens: 7},
	}
	reg := &tools.Registry{}
	reg.Register(first)
	reg.Register(second)
	a := newAgent(llmtest.New("fake"), reg, Options{})
	sink := &recordSink{}
	type dispatchOutcome struct {
		blocks []llm.ContentBlock
		usage  llm.Usage
	}
	done := make(chan dispatchOutcome, 1)
	go func() {
		blocks, _, usage := a.dispatchCalls(context.Background(), []llm.ToolCall{
			{ID: "a", Name: "first", Input: json.RawMessage(`{}`)},
			{ID: "b", Name: "second", Input: json.RawMessage(`{}`)},
		}, 1, 1, sink)
		done <- dispatchOutcome{blocks: blocks, usage: usage}
	}()
	awaitSignal(t, firstStarted, "first call start")
	awaitSignal(t, secondFinished, "second call completion")
	close(releaseFirst)
	outcome := <-done
	if got := idsFromCalls(sink.starts); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("ToolStart IDs = %v, want [a b]", got)
	}
	if got := idsFromResults(sink.results); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("ToolResult IDs = %v, want [a b]", got)
	}
	if len(outcome.blocks) != 2 || outcome.blocks[0].ResultForID != "a" || outcome.blocks[1].ResultForID != "b" {
		t.Fatalf("result blocks out of order: %+v", outcome.blocks)
	}
	if outcome.usage.InputTokens != 7 || outcome.usage.OutputTokens != 10 {
		t.Fatalf("usage = %+v, want ordered aggregate 7/10", outcome.usage)
	}
}

func TestParallelFailureGuardFoldsResultsInEmissionOrder(t *testing.T) {
	mutationDone := make(chan struct{})
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "check", run: func(context.Context, json.RawMessage) (string, error) {
		<-mutationDone // force reverse execution completion
		return "", errors.New("check failed")
	}})
	reg.Register(&mutationRecordTool{
		recordTool: &recordTool{name: "mutation", run: func(context.Context, json.RawMessage) (string, error) {
			close(mutationDone)
			return "changed", nil
		}},
		paths: func(json.RawMessage) ([]string, error) { return []string{"changed.txt"}, nil },
	})
	a := newAgent(llmtest.New("fake"), reg, Options{})
	a.failGuard = newFailureGuard()
	a.dispatchCalls(context.Background(), []llm.ToolCall{
		{ID: "failure", Name: "check", Input: json.RawMessage(`{}`)},
		{ID: "mutation", Name: "mutation", Input: json.RawMessage(`{}`)},
	}, 1, 1, &recordSink{})
	a.failGuard.mu.Lock()
	defer a.failGuard.mu.Unlock()
	if len(a.failGuard.records) != 0 {
		t.Fatalf("failure guard retained pre-mutation failure after ordered reset: %+v", a.failGuard.records)
	}
}

func TestNonToolHooksDoNotDisableReadOnlyParallelDispatch(t *testing.T) {
	run := barrierRun(2)
	t1 := &recordTool{name: "r1", readOnly: true, run: run}
	t2 := &recordTool{name: "r2", readOnly: true, run: run}
	reg := &tools.Registry{}
	reg.Register(t1)
	reg.Register(t2)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "a", "r1", `{}`),
				toolDone(1, "b", "r2", `{}`),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	runner := testHookRunner(t, `{"Stop":[{"hooks":[{"type":"command","command":"printf '{}'"}]}]}`)
	a := newAgent(fp, reg, Options{Hooks: runner})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	for _, result := range sink.results {
		if result.IsError {
			t.Fatalf("read-only calls were serialized despite only non-tool hooks: %+v", sink.results)
		}
	}
}

func TestToolHooksOmitParallelBatchMetadata(t *testing.T) {
	reg := &tools.Registry{}
	for _, name := range []string{"r1", "r2"} {
		reg.Register(&recordTool{name: name, readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok", nil
		}})
	}

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "a", "r1", `{}`),
				toolDone(1, "b", "r2", `{}`),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	runner := testHookRunner(t, `{"PreToolUse":[{"hooks":[{"type":"command","command":"printf '{}'"}]}]}`)
	a := newAgent(fp, reg, Options{Hooks: runner})

	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if got := a.Transcript()[2].ParallelToolBatches; len(got) != 0 {
		t.Fatalf("tool-hook-serialized calls recorded as parallel: %+v", got)
	}
}

func TestSequentialToolSplitsDefaultParallelIslands(t *testing.T) {
	firstRun := barrierRun(2)
	secondRun := barrierRun(2)
	r1 := &recordTool{name: "r1", readOnly: true, run: firstRun}
	r2 := &recordTool{name: "r2", readOnly: true, run: firstRun}
	mut := &recordTool{name: "mut", sequential: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "mutated", nil
	}}
	r3 := &recordTool{name: "r3", readOnly: true, run: secondRun}
	r4 := &recordTool{name: "r4", readOnly: true, run: secondRun}
	reg := &tools.Registry{}
	for _, tool := range []*recordTool{r1, r2, mut, r3, r4} {
		reg.Register(tool)
	}

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "a", "r1", `{"_stage":1}`),
				toolDone(1, "b", "r2", `{"_stage":1}`),
				toolDone(2, "c", "mut", `{"_stage":1}`),
				toolDone(3, "d", "r3", `{"_stage":1}`),
				toolDone(4, "e", "r4", `{"_stage":1}`),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	resMsg := a.Transcript()[2]
	if len(resMsg.Content) != 5 {
		t.Fatalf("result blocks = %d, want 5:\n%s", len(resMsg.Content), dump([]llm.Message{resMsg}))
	}
	for i, wantID := range []string{"a", "b", "c", "d", "e"} {
		if got := resMsg.Content[i].ResultForID; got != wantID {
			t.Fatalf("result %d id = %q, want %q:\n%s", i, got, wantID, dump([]llm.Message{resMsg}))
		}
		if resMsg.Content[i].ResultError {
			t.Fatalf("result %s errored; default-parallel islands did not overlap: %s", wantID, resMsg.Content[i].ResultText)
		}
	}
	if got := resMsg.ParallelToolBatches; len(got) != 2 ||
		!slices.Equal(got[0].ToolUseIDs, []string{"a", "b"}) ||
		!slices.Equal(got[1].ToolUseIDs, []string{"d", "e"}) {
		t.Fatalf("parallel batches = %+v, want [a b] and [d e]", got)
	}
	if got := idsFromCalls(sink.starts); !slices.Equal(got, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("ToolStart order = %v, want [a b c d e]", got)
	}
	if got := idsFromResults(sink.results); !slices.Equal(got, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("ToolResult order = %v, want [a b c d e]", got)
	}
	for _, tool := range []*recordTool{r1, r2, mut, r3, r4} {
		if !slices.Equal(tool.inputs, []string{"{}"}) {
			t.Fatalf("tool %s inputs = %v, want stage-free object", tool.name, tool.inputs)
		}
	}
}

func TestBuiltInWriteThenEditSamePathRunsInEmissionOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordered.txt")
	writeInput := fmt.Sprintf(`{"path":%q,"content":"one\n"}`, path)
	editInput := fmt.Sprintf(`{"files":[{"path":%q,"edits":[{"oldText":"one","newText":"two"}]}]}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "write", "write", writeInput),
				toolDone(1, "edit", "edit", editInput),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	if err := a.RunPrompt(context.Background(), "write then edit", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "two\n" {
		t.Fatalf("final content = %q, want emission-ordered write then edit", body)
	}
	if batches := a.Transcript()[2].ParallelToolBatches; len(batches) != 0 {
		t.Fatalf("fully serialized write/edit chain recorded as parallel: %+v", batches)
	}
}

func TestShowDiffsEmitsPerToolDiffWithoutChangingToolResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	input := fmt.Sprintf(`{"files":[{"path":%q,"edits":[{"oldText":"bar","newText":"baz"}]}]}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "edit1", "edit", input)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{ShowDiffs: true})
	sink := &diffRecordSink{}

	if err := a.RunPrompt(context.Background(), "edit", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(sink.diffs) != 1 {
		t.Fatalf("diff events = %d, want 1", len(sink.diffs))
	}
	if sink.paths[0] != path {
		t.Fatalf("diff path = %q, want %q", sink.paths[0], path)
	}
	if !strings.Contains(sink.diffs[0], "-bar\n+baz\n") {
		t.Fatalf("diff missing edit:\n%s", sink.diffs[0])
	}
	result := a.Transcript()[2].Content[0].ResultText
	if strings.Contains(result, "-bar") || strings.Contains(result, "+baz") {
		t.Fatalf("tool result should not include diff text: %q", result)
	}
}

func TestShowDiffsDisabledEmitsNoDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	input := fmt.Sprintf(`{"files":[{"path":%q,"edits":[{"oldText":"bar","newText":"baz"}]}]}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "edit1", "edit", input)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	sink := &diffRecordSink{}

	if err := a.RunPrompt(context.Background(), "edit", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.diffs) != 0 {
		t.Fatalf("diff events = %d, want 0: %v", len(sink.diffs), sink.diffs)
	}
}

func TestShowDiffsIncrementalSameFileToolCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	first := fmt.Sprintf(`{"_stage":1,"files":[{"path":%q,"edits":[{"oldText":"bar","newText":"foo"}]}]}`, path)
	second := fmt.Sprintf(`{"_stage":2,"files":[{"path":%q,"edits":[{"oldText":"foo\nfoo","newText":"foo\nbar"}]}]}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "edit1", "edit", first),
				toolDone(1, "edit2", "edit", second),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{ShowDiffs: true})
	sink := &diffRecordSink{}

	if err := a.RunPrompt(context.Background(), "edit", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(sink.diffs) != 2 {
		t.Fatalf("diff events = %d, want 2: %v", len(sink.diffs), sink.diffs)
	}
	if !strings.Contains(sink.diffs[0], "-bar\n+foo\n") {
		t.Fatalf("first diff should show bar -> foo:\n%s", sink.diffs[0])
	}
	if !strings.Contains(sink.diffs[1], "-foo\n+bar\n") {
		t.Fatalf("second diff should show foo -> bar:\n%s", sink.diffs[1])
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if string(got) != "foo\nbar\n" {
		t.Fatalf("final content = %q, want original content restored", got)
	}
	if batches := a.Transcript()[2].ParallelToolBatches; len(batches) != 0 {
		t.Fatalf("fully chained same-file edits recorded parallel metadata: %+v", batches)
	}
}

func TestTruncatedToolResultIncludesArchivePathInNextRequest(t *testing.T) {
	reg := &tools.Registry{}
	reg.SetResultLimits(80, 1000)
	reg.Register(&recordTool{name: "big", readOnly: true, run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return strings.Repeat("x", 500), nil
	}})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_big", "big", `{}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	sink := &archiveSink{
		archive: ToolResultArchive{
			DisplayPath: "artifacts/tool-results/0001-call_big.txt",
			ModelPath:   "/tmp/harness-session/artifacts/tool-results/0001-call_big.txt",
		},
	}
	a := newAgent(fp, reg, Options{})

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(sink.archived) != 1 {
		t.Fatalf("archived results = %d, want 1", len(sink.archived))
	}
	if !strings.Contains(sink.archived[0].OriginalText, strings.Repeat("x", 100)) {
		t.Fatalf("archived result did not receive original text")
	}
	if len(fp.Requests) < 2 {
		t.Fatalf("model requests = %d, want at least 2", len(fp.Requests))
	}

	resultText := ""
	for _, msg := range fp.Requests[1].Messages {
		for _, block := range msg.Content {
			if block.Kind == llm.BlockToolResult && block.ResultForID == "call_big" {
				resultText = block.ResultText
			}
		}
	}
	if !strings.Contains(resultText, "/tmp/harness-session/artifacts/tool-results/0001-call_big.txt") {
		t.Fatalf("next request lacks archive path:\n%s", dump(fp.Requests[1].Messages))
	}
	if !strings.Contains(resultText, `use read {"path":`) {
		t.Fatalf("next request lacks read guidance:\n%s", dump(fp.Requests[1].Messages))
	}
	if len(sink.notices) == 0 || !strings.Contains(sink.notices[len(sink.notices)-1], "full output: artifacts/tool-results/0001-call_big.txt") {
		t.Fatalf("truncation notice missing display path: %+v", sink.notices)
	}
}

func TestResultToolOriginalUsesArchivePathInNextRequest(t *testing.T) {
	reg := &tools.Registry{}
	reg.Register(&resultRecordTool{
		recordTool: recordTool{name: "receipts", readOnly: false},
		result: tools.RunResult{
			Text:         "PASS focused tests",
			OriginalText: "verbose complete test transcript",
		},
	})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_receipts", "receipts", `{}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	sink := &archiveSink{
		archive: ToolResultArchive{
			DisplayPath: "artifacts/tool-results/0001-call_receipts.txt",
			ModelPath:   "/tmp/harness-session/artifacts/tool-results/0001-call_receipts.txt",
		},
	}
	a := newAgent(fp, reg, Options{})

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(sink.archived) != 1 || sink.archived[0].OriginalText != "verbose complete test transcript" {
		t.Fatalf("archived results = %+v", sink.archived)
	}
	var resultText string
	for _, msg := range fp.Requests[1].Messages {
		for _, block := range msg.Content {
			if block.Kind == llm.BlockToolResult && block.ResultForID == "call_receipts" {
				resultText = block.ResultText
			}
		}
	}
	if !strings.Contains(resultText, "PASS focused tests") ||
		!strings.Contains(resultText, "/tmp/harness-session/artifacts/tool-results/0001-call_receipts.txt") {
		t.Fatalf("next request result = %q", resultText)
	}
}

func TestMixedStepStaysSequential(t *testing.T) {
	var mu sync.Mutex
	var trace []string
	mk := func(name string, ro bool) *recordTool {
		return &recordTool{name: name, readOnly: ro, sequential: name == "writer", run: func(_ context.Context, _ json.RawMessage) (string, error) {
			mu.Lock()
			trace = append(trace, "start:"+name)
			mu.Unlock()
			mu.Lock()
			trace = append(trace, "end:"+name)
			mu.Unlock()
			return "ok", nil
		}}
	}
	reg := &tools.Registry{}
	reg.Register(mk("reader", true))
	reg.Register(mk("writer", false))

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolDone(0, "a", "reader", `{}`),
				toolDone(1, "b", "writer", `{}`),
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{})

	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	want := []string{"start:reader", "end:reader", "start:writer", "end:writer"}
	if !slices.Equal(trace, want) {
		t.Errorf("mixed step interleaving = %v, want strictly sequential %v", trace, want)
	}
}

// TestSteerInjectsBeforeNextTurn drives a tool-calling turn where the tool
// blocks until a steer is queued. The steered text must land as a RoleUser
// message between the tool_result and the second assistant message, and the
// second model request must have seen it (design §8.1).
func TestSteerInjectsBeforeNextTurn(t *testing.T) {
	toolRan := make(chan struct{})
	releaseTool := make(chan struct{})
	tool := &recordTool{name: "probe", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		close(toolRan)
		<-releaseTool
		return "probed", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_1", "probe", `{}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{Steer: true})
	sink := &recordSink{}

	errCh := make(chan error, 1)
	go func() { errCh <- a.RunPrompt(context.Background(), "go", sink) }()

	// Wait for the tool to run (so the loop is between tool dispatch and the
	// next model request), then steer.
	<-toolRan
	a.Steer("now do Y instead")
	close(releaseTool)

	if err := <-errCh; err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)
	// user(prompt), assistant(tool_use), user(tool_result), user(steer text), assistant(final)
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d:\n%s", len(msgs), dump(msgs))
	}
	if msgs[3].Role != llm.RoleUser || len(msgs[3].Content) != 1 || msgs[3].Content[0].Kind != llm.BlockText {
		t.Fatalf("message 3 should be the steer text, got:\n%s", dump([]llm.Message{msgs[3]}))
	}
	if msgs[3].Content[0].Text != "now do Y instead" {
		t.Errorf("steer text = %q, want %q", msgs[3].Content[0].Text, "now do Y instead")
	}
	if got := fp.RequestCount(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	// The second request's messages must include the steered text so the model
	// actually saw it on this round.
	second := fp.Requests[1]
	var sawSteer bool
	for _, m := range second.Messages {
		for _, b := range m.Content {
			if b.Kind == llm.BlockText && b.Text == "now do Y instead" {
				sawSteer = true
			}
		}
	}
	if !sawSteer {
		t.Errorf("second model request did not include the steered text")
	}
}

func TestSteerContentInjectsImagesAndRequestContext(t *testing.T) {
	toolRan := make(chan struct{})
	releaseTool := make(chan struct{})
	tool := &recordTool{name: "probe", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		close(toolRan)
		<-releaseTool
		return "probed", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_1", "probe", `{}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{Steer: true})
	sink := &recordSink{}

	errCh := make(chan error, 1)
	go func() { errCh <- a.RunPrompt(context.Background(), "go", sink) }()
	<-toolRan
	a.SteerContent(SteerInput{
		Text: "inspect this",
		Images: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageMediaType: "image/png",
			ImageData:      agentOnePixelPNG,
			ImageDetail:    "high",
			ImageName:      "screen.png",
		}},
		RequestContext: []string{"steer context"},
	})
	close(releaseTool)

	if err := <-errCh; err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	second := fp.Requests[1]
	if len(second.RequestContext) != 1 || second.RequestContext[0] != "steer context" {
		t.Fatalf("second request context = %v, want steer context", second.RequestContext)
	}
	var sawImage, sawText bool
	for _, m := range second.Messages {
		for _, b := range m.Content {
			if b.Kind == llm.BlockImage && b.ImageName == "screen.png" && b.ImageData == agentOnePixelPNG {
				sawImage = true
			}
			if b.Kind == llm.BlockText && b.Text == "inspect this" {
				sawText = true
			}
		}
	}
	if !sawImage || !sawText {
		t.Fatalf("second request saw image=%v text=%v; messages:\n%s", sawImage, sawText, dump(second.Messages))
	}
}

// steerDeliverySink records SteerDelivered notifications on top of recordSink.
type steerDeliverySink struct {
	*recordSink
	mu        sync.Mutex
	delivered []string
}

func (s *steerDeliverySink) SteerDelivered(input SteerInput) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, input.Text)
}

func (s *steerDeliverySink) deliveredTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.delivered...)
}

// TestSteerDeliveredNotifiesSinkOnInjection queues two steers during a tool
// round; the drain combines them into one Origin=MessageOriginSteer message
// and the sink must be notified exactly once, with the combined text.
func TestSteerDeliveredNotifiesSinkOnInjection(t *testing.T) {
	toolRan := make(chan struct{})
	releaseTool := make(chan struct{})
	tool := &recordTool{name: "probe", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		close(toolRan)
		<-releaseTool
		return "probed", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "call_1", "probe", `{}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{Steer: true})
	sink := &steerDeliverySink{recordSink: &recordSink{}}

	errCh := make(chan error, 1)
	go func() { errCh <- a.RunPrompt(context.Background(), "go", sink) }()
	<-toolRan
	a.SteerContent(SteerInput{Text: "first steer"})
	a.SteerContent(SteerInput{Text: "second steer"})
	close(releaseTool)

	if err := <-errCh; err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	got := sink.deliveredTexts()
	if len(got) != 1 || got[0] != "first steer\n\nsecond steer" {
		t.Fatalf("SteerDelivered calls = %q, want one call with the combined steer text", got)
	}
}

// TestSteerDeliveredNotCalledWhenSteeringDisabled confirms the notification
// never fires when Options.Steer is false: no channel, no drain, no delivery.
func TestSteerDeliveredNotCalledWhenSteeringDisabled(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Catalog(), Options{})
	sink := &steerDeliverySink{recordSink: &recordSink{}}
	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if got := sink.deliveredTexts(); len(got) != 0 {
		t.Fatalf("SteerDelivered calls = %q, want none when steering is disabled", got)
	}
}

// TestSteerResetsLoopGuard queues a steer during a repeating-tool turn. The
// steer must reset the repeat streak so the loop-guard nudge that would
// otherwise fire at repeatSteerThreshold (3 identical rounds) does not.
func TestSteerResetsLoopGuard(t *testing.T) {
	// Gate the first tool round so the test can queue a steer before round 2.
	firstDone := make(chan struct{})
	releaseAll := make(chan struct{})
	round := int32(0)
	tool := &recordTool{name: "loop", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		if atomic.AddInt32(&round, 1) == 1 {
			// Round 1: block until the test has steered, then release this and
			// every later round together.
			close(firstDone)
			<-releaseAll
		}
		return "looped", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	always := llmtest.Step{
		Events: []llm.StreamEvent{toolDone(0, "id", "loop", `{}`)},
		Stop:   llm.StopToolUse,
	}
	// Three identical tool rounds would trip repeatSteerThreshold (3) at round
	// 3 and inject a loop-guard nudge — unless the steer at round 1 resets the
	// streak to leave only 2 identical rounds after it.
	fp := llmtest.New("fake", always, always, always,
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn})
	a := newAgent(fp, reg, Options{Steer: true})
	sink := &recordSink{}

	errCh := make(chan error, 1)
	go func() { errCh <- a.RunPrompt(context.Background(), "go", sink) }()
	<-firstDone
	a.Steer("change approach")
	close(releaseAll)

	if err := <-errCh; err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)

	var injected []string
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Kind == llm.BlockText {
				injected = append(injected, b.Text)
			}
		}
	}
	for _, txt := range injected {
		if strings.Contains(txt, "[loop guard]") {
			t.Errorf("loop-guard nudge fired despite steer:\n%s", txt)
		}
	}
	var sawSteer bool
	for _, txt := range injected {
		if txt == "change approach" {
			sawSteer = true
		}
	}
	if !sawSteer {
		t.Errorf("steer text not present in transcript; injected=%v", injected)
	}
}

// TestSteerDisabledNoChannel confirms Steer() is a no-op when Options.Steer is
// false: the channel is nil and the loop never drains.
func TestSteerDisabledNoChannel(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Catalog(), Options{})
	if a.Steer("ignored") { // must not panic on a nil channel
		t.Fatal("Steer accepted input while disabled")
	}
	if a.steer != nil {
		t.Fatalf("steer channel should be nil when disabled, got %v", a.steer)
	}
	if got := a.DrainSteer(); got != "" {
		t.Fatalf("DrainSteer on disabled agent = %q, want empty", got)
	}
}

func TestSteerContentReportsFullBufferAndPreservesDrainMetadata(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Catalog(), Options{Steer: true})
	for i := 0; i < cap(a.steer); i++ {
		input := SteerInput{Text: fmt.Sprintf("steer %d", i), CorrelationID: fmt.Sprintf("p%d", i)}
		if !a.SteerContent(input) {
			t.Fatalf("SteerContent rejected input %d before buffer was full", i)
		}
	}
	if a.SteerContent(SteerInput{Text: "overflow", CorrelationID: "overflow"}) {
		t.Fatal("SteerContent accepted input after buffer was full")
	}

	got := a.DrainSteerContents()
	if len(got) != cap(a.steer) {
		t.Fatalf("drained inputs = %d, want %d", len(got), cap(a.steer))
	}
	for i, input := range got {
		if input.Text != fmt.Sprintf("steer %d", i) || input.CorrelationID != fmt.Sprintf("p%d", i) {
			t.Fatalf("drained input %d = %+v", i, input)
		}
	}
}

// TestDrainSteerJoinsMultiple confirms multiple queued steers are joined into one
// RoleUser message on the next tool round.
func TestDrainSteerJoinsMultiple(t *testing.T) {
	toolRan := make(chan struct{})
	release := make(chan struct{})
	tool := &recordTool{name: "probe", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		close(toolRan)
		<-release
		return "probed", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "call_1", "probe", `{}`)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{Steer: true})
	sink := &peekCountingSink{}

	errCh := make(chan error, 1)
	go func() { errCh <- a.RunPrompt(context.Background(), "go", sink) }()
	<-toolRan
	a.Steer("first")
	a.Steer("second")
	close(release)

	if err := <-errCh; err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if sink.requestCalls != len(fp.Requests) {
		t.Fatalf("RequestContext calls = %d, provider requests = %d", sink.requestCalls, len(fp.Requests))
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	var steerText string
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Kind == llm.BlockText && (strings.Contains(b.Text, "first") || b.Text == "second") && b.Text != "probed" {
				steerText = b.Text
			}
		}
	}
	if steerText != "first\n\nsecond" {
		t.Errorf("joined steer text = %q, want %q", steerText, "first\n\nsecond")
	}
}

// TestDrainSteerRecoversUnconsumed confirms a steer queued during a tool-less
// (StopEndTurn) turn is never injected (no tool round) and remains recoverable
// via DrainSteer after the turn ends, so the REPL can run it as the next turn.
func TestDrainSteerRecoversUnconsumed(t *testing.T) {
	steered := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("reply")},
			Stop:   llm.StopEndTurn,
			Block:  func(context.Context) { close(steered) },
		},
	)
	a := newAgent(fp, tools.Catalog(), Options{Steer: true})
	sink := &recordSink{}

	errCh := make(chan error, 1)
	go func() { errCh <- a.RunPrompt(context.Background(), "go", sink) }()
	<-steered
	a.Steer("missed me")
	if err := <-errCh; err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	// The turn ended on an assistant message with no tool round, so the steer was
	// never appended to the transcript...
	for _, m := range a.Transcript() {
		for _, b := range m.Content {
			if b.Kind == llm.BlockText && b.Text == "missed me" {
				t.Fatalf("unconsumed steer should not be in transcript")
			}
		}
	}
	// …but it remains recoverable for the REPL to queue as the next turn.
	if got := a.DrainSteer(); got != "missed me" {
		t.Fatalf("DrainSteer = %q, want %q", got, "missed me")
	}
	if got := a.DrainSteer(); got != "" {
		t.Fatalf("second DrainSteer = %q, want empty", got)
	}
}

// peekCountingSink counts consuming vs peek context gathering. Its
// RequestContext models a sink that consumes generic one-shot signals such as
// completed background context.
type peekCountingSink struct {
	recordSink
	requestCalls int
	peekCalls    int
}

func (s *peekCountingSink) RequestContext() []string {
	s.requestCalls++
	return []string{"[one-shot context]"}
}

func (s *peekCountingSink) PeekRequestContext() []string {
	s.peekCalls++
	return []string{"[one-shot context]"}
}

type changingRequestContextSink struct {
	recordSink
	requestCalls int
	rewriteCalls int
}

func (s *changingRequestContextSink) RequestContext() []string {
	s.requestCalls++
	return []string{fmt.Sprintf("[request context %d]", s.requestCalls)}
}

func (s *changingRequestContextSink) TranscriptRewritten() {
	s.rewriteCalls++
}

func TestPostPromptEstimateDoesNotConsumeRequestContext(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &peekCountingSink{}

	if err := a.RunPrompt(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	// RequestContext consumes one-shot signals, so it must run exactly once per
	// conversational model round; the post-prompt sizing estimate has to use the
	// non-consuming peek instead.
	if sink.requestCalls != len(fp.Requests) {
		t.Fatalf("RequestContext calls = %d, want %d (one per request sent)", sink.requestCalls, len(fp.Requests))
	}
	if sink.peekCalls == 0 {
		t.Fatal("post-prompt estimate did not use PeekRequestContext")
	}
}

// TestReportedContextAnchorsToMeasuredUsage pins WI-2: after a turn reports
// real usage, the next turn_attempt_start context total anchors to that
// measurement plus a local delta estimate, even when the provider count-token
// endpoint returns a much lower figure (Kimi's count route excludes billed
// thinking-signature replay).
func TestReportedContextAnchorsToMeasuredUsage(t *testing.T) {
	fp := &countingProvider{
		FakeProvider: llmtest.New("fake",
			llmtest.Step{
				Events: []llm.StreamEvent{textDelta("one"), toolDone(0, "call_1", "probe", `{}`)},
				Stop:   llm.StopToolUse,
				Usage:  llm.Usage{InputTokens: 800, CacheReadTokens: 200},
			},
			llmtest.Step{Events: []llm.StreamEvent{textDelta("two")}, Stop: llm.StopEndTurn},
		),
		count: 10, // count API claims the request is tiny
	}
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "probe", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}})
	a := newAgent(fp, reg, Options{})
	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "work", sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.contexts) != 2 {
		t.Fatalf("turn_attempt_start contexts = %d, want 2", len(sink.contexts))
	}
	if sink.contexts[0].Total >= 1000 {
		t.Fatalf("turn 1 context = %d, want the low count-adjusted estimate before any measurement", sink.contexts[0].Total)
	}
	if sink.contexts[1].Total < 1000 {
		t.Fatalf("turn 2 context = %d, want >= the measured 1000 (800 input + 200 cache read) despite the low count", sink.contexts[1].Total)
	}
	// Per-section estimates still come from the estimator, not the anchor.
	if sink.contexts[1].System != sink.contexts[0].System || sink.contexts[1].Tools != sink.contexts[0].Tools {
		t.Fatalf("section estimates changed under anchoring: %+v vs %+v", sink.contexts[0], sink.contexts[1])
	}
}

// TestMeasuredAnchorClearsAfterCompaction pins the fallback half of WI-2: a
// compaction that rewrites the transcript clears the measured anchor, so the
// reported context returns to the local estimate until the next measured turn
// (the anchor must not make the estimate non-monotonic across a shrink).
func TestMeasuredAnchorClearsAfterCompaction(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 5000}},
		summaryStep("CANNED SUMMARY", 200, 40),
	)
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("system prompt")
	a.SetTranscript(makeTurns(10))

	if err := a.RunPrompt(context.Background(), "keep going", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if got := a.ContextRequest().EstimatedInputTokens; got < 5000 {
		t.Fatalf("/context estimate after measured turn = %d, want anchored to >= 5000", got)
	}

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	got := a.ContextRequest().EstimatedInputTokens
	if got >= 5000 {
		t.Fatalf("/context estimate after compaction = %d, want the unanchored local estimate", got)
	}
	want := a.EstimateContext().Total
	if got != want {
		t.Fatalf("/context estimate after compaction = %d, want the estimator's %d", got, want)
	}
}

func TestSkillReadResultPassesThroughWithoutRequestContextPinning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	const body = "PASS THROUGH SKILL BODY"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"path":%q}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "skill-read", "read", input)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Catalog(), Options{})

	if err := a.RunPrompt(context.Background(), "use the skill", &recordSink{}); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(fp.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(fp.Requests))
	}
	for i, request := range fp.Requests {
		if contextText := strings.Join(request.RequestContext, "\n"); strings.Contains(contextText, body) {
			t.Fatalf("request %d pinned SKILL.md body in request context: %q", i+1, contextText)
		}
	}
	var result string
	for _, message := range fp.Requests[1].Messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockToolResult && block.ResultForID == "skill-read" {
				result = block.ResultText
			}
		}
	}
	if want := "1\t" + body; result != want {
		t.Fatalf("SKILL.md result = %q, want unchanged line-numbered content %q", result, want)
	}
	if strings.Contains(result, "receipt") {
		t.Fatalf("SKILL.md result was replaced with a receipt: %q", result)
	}
}
