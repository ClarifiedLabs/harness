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
	"testing"
	"time"

	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/sse"
	"harness/internal/tools"
)

// recordSink captures every sink callback so tests can assert what the UI would
// have been told.
type recordSink struct {
	text            strings.Builder
	models          []modelTurnEvent
	contexts        []ContextEstimate
	modelUsage      []ModelTurnUsage
	reasoning       []string
	phases          []string
	toolUses        []llm.ToolCall
	argDeltas       []string
	starts          []llm.ToolCall
	results         []llm.ToolResult
	abandoned       []modelTurnEvent
	notices         []string
	turnUsage       []TurnUsage
	modelTurnCounts []int
}

type modelTurnEvent struct {
	modelTurn int
	attempt   int
}

func (s *recordSink) TextDelta(t string) { s.text.WriteString(t) }
func (s *recordSink) ReasoningSummary(t string) {
	s.reasoning = append(s.reasoning, t)
}
func (s *recordSink) AssistantPhase(phase string) {
	s.phases = append(s.phases, phase)
}
func (s *recordSink) ModelTurnStart(modelTurn, attempt int, ctx ContextEstimate) {
	s.models = append(s.models, modelTurnEvent{modelTurn: modelTurn, attempt: attempt})
	s.contexts = append(s.contexts, ctx)
}
func (s *recordSink) ModelTurnComplete(u ModelTurnUsage) {
	s.modelUsage = append(s.modelUsage, u)
}
func (s *recordSink) ModelTurnAbandoned(modelTurn, attempt int) {
	s.abandoned = append(s.abandoned, modelTurnEvent{modelTurn: modelTurn, attempt: attempt})
}
func (s *recordSink) ToolUseStart(c llm.ToolCall) { s.toolUses = append(s.toolUses, c) }
func (s *recordSink) ToolUseDelta(_ int, delta string) {
	s.argDeltas = append(s.argDeltas, delta)
}
func (s *recordSink) ToolStart(c llm.ToolCall)    { s.starts = append(s.starts, c) }
func (s *recordSink) ToolResult(r llm.ToolResult) { s.results = append(s.results, r) }
func (s *recordSink) Notice(msg string)           { s.notices = append(s.notices, msg) }
func (s *recordSink) TurnComplete(u TurnUsage) {
	s.turnUsage = append(s.turnUsage, u)
	s.modelTurnCounts = append(s.modelTurnCounts, u.ModelTurns)
}

type diffRecordSink struct {
	recordSink
	diffs []string
}

func (s *diffRecordSink) ToolDiff(_ llm.ToolCall, text string) {
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

// recordTool is a fake tool whose Run is scriptable; it records the inputs it
// received in call order. The mutex guards inputs because read-only model turns now
// dispatch Run concurrently.
type recordTool struct {
	name     string
	readOnly bool
	run      func(ctx context.Context, input json.RawMessage) (string, error)
	mu       sync.Mutex
	inputs   []string
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

func TestTextOnlyTurn(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hello "), textDelta("world")},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 10, OutputTokens: 5},
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
	}
}

func TestModelRequestStampsContextBudgetHints(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 100_000})
	a.SetSystem("system prompt")

	if err := a.RunTurn(context.Background(), "hello", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

func TestRunTurnUsesProviderInputTokenCount(t *testing.T) {
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

	if err := a.RunTurn(context.Background(), "hello", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "hello", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
	a := newAgent(fp, reg, Options{ContextWindow: 10_000})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
	if len(full) != 20_000 {
		t.Fatalf("failed request tool result length = %d, want 20000", len(full))
	}
	if len(trimmed) >= len(full) || !strings.Contains(trimmed, retentionTrimMarker) {
		t.Fatalf("retry tool result was not trimmed: len=%d text=%q", len(trimmed), trimmed)
	}
	if !slices.Contains(sink.notices, "[context overflow: compacting and retrying request]") {
		t.Fatalf("notices = %+v, want context overflow retry notice", sink.notices)
	}
}

func TestReasoningSummaryUsesDedicatedSinkOnly(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{reasoningSummary("Checked the repo."), textDelta("done")},
		Stop:   llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "more", sink); err != nil {
		t.Fatalf("RunTurn 2: %v", err)
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

func TestPromptCacheKeyStablePerSessionAndPrefixSensitive(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn}, llmtest.Step{Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})

	key := a.promptCacheKey()
	if key == "" {
		t.Fatal("prompt cache key is empty")
	}
	prefix := a.promptCachePrefix()
	if !strings.HasPrefix(key, prefix+"-") {
		t.Fatalf("cache key = %q, want prefix %q plus session suffix", key, prefix)
	}

	// Every turn in a session reuses the same key, so requests with the same
	// large system+tools prefix keep landing on the same cache backend without
	// sharing proxy continuation state with other sessions.
	for _, prompt := range []string{"one", "two"} {
		if err := a.RunTurn(context.Background(), prompt, &recordSink{}); err != nil {
			t.Fatalf("RunTurn %q: %v", prompt, err)
		}
	}
	if got := fp.Requests[0].PromptCacheKey; got == "" || got != key {
		t.Fatalf("request[0] cache key = %q, want %q", got, key)
	}
	if fp.Requests[0].PromptCacheKey != fp.Requests[1].PromptCacheKey {
		t.Fatalf("cache key changed across turns: %q vs %q", fp.Requests[0].PromptCacheKey, fp.Requests[1].PromptCacheKey)
	}

	if other := newAgent(fp, tools.Default(), Options{}).promptCacheKey(); other == key {
		t.Fatalf("cache key reused across agent sessions: %q", key)
	}

	// A different advertised tool set is a different prefix, so the key must change.
	subset, err := tools.Default().Subset([]string{"read_file"})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	if other := newAgent(fp, subset, Options{}).promptCachePrefix(); other == prefix {
		t.Fatalf("cache prefix did not change for a different tool set: %q", other)
	}
}

func TestLongCacheTTLReflectsInteractive(t *testing.T) {
	interactive := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	a := newAgent(interactive, tools.Default(), Options{Interactive: true})
	if err := a.RunTurn(context.Background(), "hi", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !interactive.Requests[0].LongCacheTTL {
		t.Fatal("interactive session must set LongCacheTTL on requests")
	}

	oneshot := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	b := newAgent(oneshot, tools.Default(), Options{Interactive: false})
	if err := b.RunTurn(context.Background(), "hi", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if oneshot.Requests[0].LongCacheTTL {
		t.Fatal("one-shot session must not set LongCacheTTL")
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
	})
	a := newAgent(fp, tools.Default(), Options{})

	warm, ok := a.PrewarmFunc()
	if !ok {
		t.Fatal("PrewarmFunc ok=false")
	}
	warm(context.Background())

	if len(fp.Requests) != 1 {
		t.Fatalf("provider received %d requests, want 1", len(fp.Requests))
	}
	if fp.Requests[0].MaxTokens != 1 {
		t.Errorf("warm request MaxTokens = %d, want 1", fp.Requests[0].MaxTokens)
	}
	// Pre-warming must not mutate the transcript.
	if n := len(a.Transcript()); n != 0 {
		t.Errorf("transcript mutated by prewarm: %d messages", n)
	}
}

func TestMaxTokensStopEmitsNotice(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("partial final")},
		Stop:   llm.StopMaxTokens,
	})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

func TestRunTurnContentAddsImagesBeforeText(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})
	sink := &recordSink{}
	image := llm.ContentBlock{
		Kind:           llm.BlockImage,
		ImageMediaType: "image/png",
		ImageData:      "abc123",
		ImageDetail:    "high",
		ImageName:      "screen.png",
	}

	if err := a.RunTurnContent(context.Background(), "describe it", []llm.ContentBlock{image}, sink); err != nil {
		t.Fatalf("RunTurnContent: %v", err)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs[0].Content) != 2 {
		t.Fatalf("user content = %d, want image + text", len(msgs[0].Content))
	}
	if msgs[0].Content[0].Kind != llm.BlockImage || msgs[0].Content[0].ImageData != "abc123" {
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(sink.turnUsage) != 1 {
		t.Fatalf("turn usage events = %d, want 1", len(sink.turnUsage))
	}
	got := sink.turnUsage[0].Usage
	if got.InputTokens != 100 || got.OutputTokens != 36 {
		t.Fatalf("turn usage = %+v, want provider 30/6 + delegate 70/30", got)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

func TestModelTurnStartEmittedForRetries(t *testing.T) {
	fail := llmtest.Step{Err: &llm.APIError{StatusCode: 503, Message: "service unavailable", Retryable: true}}
	fp := llmtest.New("fake",
		fail,
		llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	want := []modelTurnEvent{{modelTurn: 1, attempt: 1}, {modelTurn: 1, attempt: 2}}
	if !slices.Equal(sink.models, want) {
		t.Errorf("model turn events = %+v, want %+v", sink.models, want)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	// The error result is appended as an is_error tool_result.
	resMsg := a.Transcript()[2]
	if len(resMsg.Content) != 1 || !resMsg.Content[0].ResultError {
		t.Fatalf("expected an is_error result:\n%s", dump([]llm.Message{resMsg}))
	}
	if !strings.Contains(resMsg.Content[0].ResultText, "kaboom") {
		t.Errorf("error text = %q, want it to mention kaboom", resMsg.Content[0].ResultText)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	// Every model turn asks for a tool: the loop must stop at the limit. After
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
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	// 3 capped model turns + 1 tools-disabled wind-down request.
	if len(fp.Requests) != 4 {
		t.Errorf("provider called %d times, want 4 (3 turns + final summary)", len(fp.Requests))
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
	modelTurns := make([]llmtest.Step, defaultConfigMaxTurns+2)
	for i := 0; i < defaultConfigMaxTurns+1; i++ {
		modelTurns[i] = toolUse
	}
	modelTurns[len(modelTurns)-1] = llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn}
	fp := llmtest.New("fake", modelTurns...)
	a := newAgent(fp, reg, Options{MaxTurns: 0})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(fp.Requests) != defaultConfigMaxTurns+2 {
		t.Errorf("provider called %d times, want %d (past default cap)", len(fp.Requests), defaultConfigMaxTurns+2)
	}
	if sink.turnUsage[0].ModelTurns != defaultConfigMaxTurns+2 {
		t.Errorf("TurnComplete model turns = %d, want %d", sink.turnUsage[0].ModelTurns, defaultConfigMaxTurns+2)
	}
	for _, n := range sink.notices {
		if strings.Contains(n, "max turns") {
			t.Errorf("unlimited max turns should not emit stop notice, got %q", n)
		}
	}
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

	err := a.RunTurn(ctx, "go", sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
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

	err := a.RunTurn(ctx, "go", &recordSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	// Nothing streamed: the partial assistant message is dropped, leaving only the
	// user message.
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("want only the user message, got %d:\n%s", len(msgs), dump(msgs))
	}
}

func TestUsageAccumulatedAcrossModelTurns(t *testing.T) {
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(sink.turnUsage) != 1 {
		t.Fatalf("want one TurnComplete, got %d", len(sink.turnUsage))
	}
	tu := sink.turnUsage[0]
	if tu.Usage.InputTokens != 300 || tu.Usage.OutputTokens != 30 {
		t.Errorf("turn usage = %+v, want 300 in / 30 out", tu.Usage)
	}
	if tu.ModelTurns != 2 {
		t.Errorf("turn model turns = %d, want 2", tu.ModelTurns)
	}
}

func TestModelTurnUsageEmittedForEachProviderReturn(t *testing.T) {
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(sink.modelUsage) != 2 {
		t.Fatalf("model usage events = %d, want 2", len(sink.modelUsage))
	}
	if got := sink.modelUsage[0]; got.ModelTurn != 1 || got.Attempt != 1 || got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 10 {
		t.Errorf("model usage[0] = %+v, want turn 1 attempt 1 with 100/10", got)
	}
	if got := sink.modelUsage[1]; got.ModelTurn != 2 || got.Attempt != 1 || got.Usage.InputTokens != 200 || got.Usage.OutputTokens != 20 {
		t.Errorf("model usage[1] = %+v, want turn 2 attempt 1 with 200/20", got)
	}
}

func TestRunTurnRejectsInvalidStableTranscriptBeforeRequest(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("should not run")}, Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Default(), Options{})
	a.SetTranscript([]llm.Message{asstToolUse("dangling", "read_file", `{}`)})
	sink := &recordSink{}

	err := a.RunTurn(context.Background(), "next", sink)
	if err == nil || !strings.Contains(err.Error(), "agent transcript invalid before model request") {
		t.Fatalf("RunTurn err = %v, want invalid transcript before request", err)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(fp.Requests))
	}
	if len(sink.turnUsage) != 1 {
		t.Fatalf("turn usage events = %d, want 1", len(sink.turnUsage))
	}
}

// SetTools swaps the registry that backs both the advertised specs and
// dispatch, so an agent switch immediately changes what the model sees and can
// call.
func TestSetToolsChangesAdvertisedAndDispatchableTools(t *testing.T) {
	catalog, _ := tools.CatalogWithOptions(tools.Options{SearchTools: tools.SearchToolsGrep})
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

	if err := a.RunTurn(context.Background(), "one", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	a.SetTools(restricted)
	if err := a.RunTurn(context.Background(), "two", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
	if state == nil || state.PreviousResponseID != "resp_2" || state.AnchorMessages != len(a.Transcript()) {
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
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "missing", AnchorMessages: 2})

	if err := a.RunTurn(context.Background(), "next", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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
		if strings.Contains(n, "retrying model turn") {
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
	if want := []modelTurnEvent{{modelTurn: 1, attempt: 1}}; !slices.Equal(sink.abandoned, want) {
		t.Errorf("abandoned attempts = %+v, want %+v", sink.abandoned, want)
	}
	// Wasted usage from the failed attempt is paid for and counted.
	if got := sink.turnUsage[0].Usage.InputTokens; got != 50 {
		t.Errorf("turn input tokens = %d, want 50 (40 wasted + 10)", got)
	}
	// And it is broken out so the UI can show the retry cost (r51+r52).
	if got := sink.turnUsage[0].Wasted.InputTokens; got != 40 {
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
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("slept = %v, want [2s]", slept)
	}
	var noticed bool
	for _, n := range sink.notices {
		if strings.Contains(n, "retrying model turn in 2s") {
			noticed = true
		}
	}
	if !noticed {
		t.Fatalf("retry notice missing delay, notices=%v", sink.notices)
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

	if err := a.RunTurn(context.Background(), "commit", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	err := a.RunTurn(context.Background(), "hi", sink)
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("RunTurn err = %v, want the APIError after budget exhaustion", err)
	}
	if len(fp.Requests) != 3 {
		t.Errorf("provider called %d times, want 3 (1 + 2 retries)", len(fp.Requests))
	}
	mustValid(t, a.Transcript())
}

func TestMidStreamRetryBudgetExhaustedDropsPartialText(t *testing.T) {
	fail := llmtest.Step{
		Events: []llm.StreamEvent{textDelta("I'll inspect the repo.")},
		Err:    &llm.APIError{Message: `tool "rg" produced invalid arguments: invalid JSON`, Retryable: true},
	}
	fp := llmtest.New("fake", fail, fail, fail)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})

	err := a.RunTurn(context.Background(), "debug this", &recordSink{})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("RunTurn err = %v, want the APIError after budget exhaustion", err)
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

		err := a.RunTurn(context.Background(), "hi", &recordSink{})
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != code {
			t.Fatalf("status %d: RunTurn err = %v, want the %d APIError", code, err, code)
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

	err := a.RunTurn(context.Background(), "hi", &recordSink{})
	if err == nil {
		t.Fatal("RunTurn should fail")
	}
	if len(fp.Requests) != 1 {
		t.Errorf("provider called %d times, want 1 (no retry)", len(fp.Requests))
	}
}

func TestTruncatedStreamRetried(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Err: fmt.Errorf("stream ended early: %w", sse.ErrTruncatedStream)},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("ok")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Default(), Options{})
	a.SetSleep(func(time.Duration) {})

	if err := a.RunTurn(context.Background(), "hi", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	err := a.RunTurn(ctx, "hi", &recordSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
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

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	u := sink.turnUsage[0].Usage
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

func TestRequestCarriesResolvedModel(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{textDelta("hi")},
		Stop:   llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	sink := &recordSink{}

	if err := a.RunTurn(context.Background(), "hi", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())

	resMsg := a.Transcript()[2]
	if len(resMsg.Content) != 2 || resMsg.Content[0].ResultForID != "a" || resMsg.Content[1].ResultForID != "b" {
		t.Fatalf("results not in emission order:\n%s", dump([]llm.Message{resMsg}))
	}
	for _, b := range resMsg.Content {
		if b.ResultError {
			t.Errorf("read-only calls were not concurrent: %s", b.ResultText)
		}
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())
	for _, result := range sink.results {
		if result.IsError {
			t.Fatalf("read-only calls were serialized despite only non-tool hooks: %+v", sink.results)
		}
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "edit", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(sink.diffs) != 1 {
		t.Fatalf("diff events = %d, want 1", len(sink.diffs))
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

	if err := a.RunTurn(context.Background(), "edit", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "edit", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunTurn: %v", err)
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

	if err := a.RunTurn(context.Background(), "go", &recordSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	want := []string{"start:reader", "end:reader", "start:writer", "end:writer"}
	if !slices.Equal(trace, want) {
		t.Errorf("mixed step interleaving = %v, want strictly sequential %v", trace, want)
	}
}
