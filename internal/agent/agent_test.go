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
	text           strings.Builder
	attemptStarts  []turnAttemptEvent
	contexts       []ContextEstimate
	attemptUsage   []TurnAttemptUsage
	reasoning      []string
	phases         []string
	toolUses       []llm.ToolCall
	argDeltas      []string
	starts         []llm.ToolCall
	results        []llm.ToolResult
	abandoned      []turnAttemptEvent
	notices        []string
	promptUsage    []PromptUsage
	completedTurns []TurnUsage
	turnCounts     []int
	maintenance    []MaintenanceUsage
	retention      []RetentionEvent
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
	archive     ToolResultArchive
	archiveErr  error
	archived    []llm.ToolResult
	activations []SkillActivationEvent
}

type countingProvider struct {
	*llmtest.FakeProvider
	count int
	err   error
}

func (p *countingProvider) CountInputTokens(context.Context, llm.Request) (llm.InputTokenCount, error) {
	if p.err != nil {
		return llm.InputTokenCount{}, p.err
	}
	return llm.InputTokenCount{InputTokens: p.count, Source: "test"}, nil
}

func (s *archiveSink) ArchiveToolResult(r llm.ToolResult) (ToolResultArchive, error) {
	s.archived = append(s.archived, r)
	return s.archive, s.archiveErr
}

func (s *archiveSink) SkillActivated(event SkillActivationEvent) {
	s.activations = append(s.activations, event)
}

// recordTool is a fake tool whose Run is scriptable; it records the inputs it
// received in call order. The mutex guards inputs because read-only turns now
// dispatch Run concurrently.
type recordTool struct {
	name     string
	readOnly bool
	run      func(ctx context.Context, input json.RawMessage) (string, error)
	mu       sync.Mutex
	inputs   []string
}

type resultRecordTool struct {
	recordTool
	result tools.RunResult
}

func (t *resultRecordTool) RunResult(context.Context, json.RawMessage) (tools.RunResult, error) {
	return t.result, nil
}

func (t *recordTool) Name() string                  { return t.name }
func (t *recordTool) Description() string           { return "fake tool" }
func (t *recordTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (t *recordTool) ReadOnly(json.RawMessage) bool { return t.readOnly }
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
	if len(sink.contexts) == 0 || sink.contexts[0].Total != 12_345 {
		t.Fatalf("model start context = %+v, want total 12345", sink.contexts)
	}
}

func TestContextOverflowLearnsWindowAndRetries(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Err: &llm.APIError{
			StatusCode: 400,
			Message:    "This endpoint's maximum context length is 262144 tokens. However, you requested about 266580 tokens.",
		}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn},
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
	if len(sink.results) != 1 || !sink.results[0].IsError || !strings.Contains(sink.results[0].Text, "no writes") {
		t.Fatalf("hook-blocked result = %+v", sink.results)
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

func TestParallelToolCallsSequentialInOrder(t *testing.T) {
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
	if len(resMsg.ParallelToolBatches) != 0 {
		t.Errorf("sequential calls recorded as parallel: %+v", resMsg.ParallelToolBatches)
	}

	// Tools executed sequentially in emission order.
	if len(tool.inputs) != 2 || tool.inputs[0] != `{"n":1}` || tool.inputs[1] != `{"n":2}` {
		t.Errorf("tool execution order wrong: %v", tool.inputs)
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

func TestInvalidToolInputFedBackAsError(t *testing.T) {
	var ran bool
	tool := &recordTool{name: "rg", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		ran = true
		return "should not run", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	invalid := `invalid JSON at byte offset 12: invalid character 'i' in numeric literal; input preview "{\"args\": [-i, vi, .]}"`
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{
				toolUseStart(0, "call_bad", "rg"),
				toolUseDelta(0, `{"args": [-i, vi, .]}`),
				invalidToolDone(0, "call_bad", "rg", invalid),
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
	for _, want := range []string{"invalid tool call arguments for rg", "valid JSON object", `{"args":["-n","PATTERN","."]}`} {
		if !strings.Contains(sink.results[0].Text, want) {
			t.Fatalf("error result %q missing %q", sink.results[0].Text, want)
		}
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
			if b.Kind == llm.BlockToolResult && strings.Contains(b.ResultText, "invalid tool call arguments for rg") {
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
	// A one-shot wrap-up nudge was injected before the final allowed turn.
	var sawWrapUp bool
	for _, m := range a.Transcript() {
		for _, b := range m.Content {
			if m.Role == llm.RoleUser && strings.Contains(b.Text, "turn budget") {
				sawWrapUp = true
			}
		}
	}
	if !sawWrapUp {
		t.Errorf("expected a wrap-up steering message before the final turn:\n%s", dump(a.Transcript()))
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
	assertPromptTermination(t, sink, TerminationModelCompleted)
}

func TestNonPositiveMaxTurnsIsUnlimited(t *testing.T) {
	const defaultConfigMaxTurns = 250

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
	turns := make([]llmtest.Step, defaultConfigMaxTurns+2)
	for i := 0; i < defaultConfigMaxTurns+1; i++ {
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

	if len(fp.Requests) != defaultConfigMaxTurns+2 {
		t.Errorf("provider called %d times, want %d (past default cap)", len(fp.Requests), defaultConfigMaxTurns+2)
	}
	if sink.promptUsage[0].Turns != defaultConfigMaxTurns+2 {
		t.Errorf("PromptComplete turns = %d, want %d", sink.promptUsage[0].Turns, defaultConfigMaxTurns+2)
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
	a.SetTranscript([]llm.Message{asstToolUse("dangling", "read_file", `{}`)})
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
	catalog, _ := tools.CatalogWithOptions(tools.Options{})
	full, err := catalog.Subset([]string{"read_file", "grep"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}
	restricted, err := catalog.Subset([]string{"read_file"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}

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

	if names := specNames(fp.Requests[0].Tools); !slices.Contains(names, "grep") {
		t.Errorf("first request should advertise grep, got %v", names)
	}
	if names := specNames(fp.Requests[1].Tools); slices.Contains(names, "grep") {
		t.Errorf("after SetTools, grep should no longer be advertised, got %v", names)
	}

	// A call to the now-removed tool must be undispatchable.
	res := a.tools.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "grep", Input: json.RawMessage(`{}`)})
	if !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Errorf("removed tool should be undispatchable, got %+v", res)
	}
}

func TestToolSpecsReturnsDeepCopy(t *testing.T) {
	reg, err := tools.Catalog().Subset([]string{"read_file"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}
	a := newAgent(llmtest.New("fake"), reg, Options{})

	specs := a.ToolSpecs()
	if len(specs) != 1 {
		t.Fatalf("ToolSpecs = %d, want 1", len(specs))
	}
	specs[0].Name = "mutated"
	specs[0].Parameters[0] = 'x'

	later := a.ToolSpecs()
	if later[0].Name != "read_file" {
		t.Fatalf("cached tool name mutated to %q", later[0].Name)
	}
	if later[0].Parameters[0] == 'x' {
		t.Fatalf("cached tool parameters were mutated: %s", later[0].Parameters)
	}

	req := a.ContextRequest()
	if req.Tools[0].Name != "read_file" || req.Tools[0].Parameters[0] == 'x' {
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

func TestResponsesStatefulRetriesFullContextWhenPreviousResponseRejected(t *testing.T) {
	fp := llmtest.New("responses",
		llmtest.Step{Err: &llm.APIError{StatusCode: 400, Code: "previous_response_not_found", Message: "missing previous_response_id"}},
		llmtest.Step{
			Events:     []llm.StreamEvent{textDelta("recovered")},
			Stop:       llm.StopEndTurn,
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

	if err := a.RunPrompt(context.Background(), "next", &recordSink{}); err != nil {
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
		llmtest.Step{Err: &llm.APIError{StatusCode: 400, Message: "Store must be set to false"}},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("recovered")},
			Stop:   llm.StopEndTurn,
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
		Err:    &llm.APIError{Message: `tool "rg" produced invalid arguments: invalid JSON`, Retryable: true},
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
	fail := llmtest.Step{Err: &llm.APIError{StatusCode: 503, Message: "service unavailable", Retryable: true}}
	fp := llmtest.New("fake", fail, fail, fail)
	a := newAgent(fp, tools.Default(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	err := a.RunPrompt(ctx, "hi", &recordSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunPrompt err = %v, want context.Canceled", err)
	}
	// One real attempt, then cancellation during the backoff stops the loop before
	// any retry re-requests the step.
	if len(fp.Requests) != 1 {
		t.Errorf("provider called %d times, want 1 (no retry after cancel)", len(fp.Requests))
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
				toolDone(0, "call_web", "$web_search", `{"query":"current docs"}`),
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
	if sink.results[0].IsError || sink.results[0].Text != `{"query":"current docs"}` {
		t.Fatalf("kimi web_search result = %+v, want passthrough arguments", sink.results[0])
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	msgs := fp.Requests[1].Messages
	if len(msgs) == 0 || len(msgs[len(msgs)-1].Content) != 1 || msgs[len(msgs)-1].Content[0].ResultText != `{"query":"current docs"}` {
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

func TestAllReadOnlyStepDispatchesConcurrently(t *testing.T) {
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
			t.Errorf("read-only calls were not concurrent: %s", b.ResultText)
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

func TestMixedStepDispatchesReadOnlyIslandsConcurrently(t *testing.T) {
	firstRun := barrierRun(2)
	secondRun := barrierRun(2)
	r1 := &recordTool{name: "r1", readOnly: true, run: firstRun}
	r2 := &recordTool{name: "r2", readOnly: true, run: firstRun}
	mut := &recordTool{name: "mut", run: func(_ context.Context, _ json.RawMessage) (string, error) {
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
				toolDone(0, "a", "r1", `{}`),
				toolDone(1, "b", "r2", `{}`),
				toolDone(2, "c", "mut", `{}`),
				toolDone(3, "d", "r3", `{}`),
				toolDone(4, "e", "r4", `{}`),
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
			t.Fatalf("result %s errored; read-only islands were not concurrent: %s", wantID, resMsg.Content[i].ResultText)
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

	first := fmt.Sprintf(`{"files":[{"path":%q,"edits":[{"oldText":"bar","newText":"foo"}]}]}`, path)
	second := fmt.Sprintf(`{"files":[{"path":%q,"edits":[{"oldText":"foo\nfoo","newText":"foo\nbar"}]}]}`, path)
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
	if !strings.Contains(resultText, `use read_file {"path":`) {
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
		return &recordTool{name: name, readOnly: ro, run: func(_ context.Context, _ json.RawMessage) (string, error) {
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
	a.Steer("ignored") // must not panic on a nil channel
	if a.steer != nil {
		t.Fatalf("steer channel should be nil when disabled, got %v", a.steer)
	}
	if got := a.DrainSteer(); got != "" {
		t.Fatalf("DrainSteer on disabled agent = %q, want empty", got)
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
