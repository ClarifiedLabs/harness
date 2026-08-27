package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
	"harness/prompts"
)

// userText is a genuine user input message (text, not tool results).
func userText(s string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: s}}}
}

// asstText is an end-turn assistant message with no tool calls.
func asstText(s string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: s}}}
}

// asstToolUse is an assistant message that issues one tool call.
func asstToolUse(id, name, input string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Kind: llm.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: []byte(input)},
	}}
}

// toolResult is the user message answering one tool call.
func toolResult(id, text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Kind: llm.BlockToolResult, ResultForID: id, ResultText: text},
	}}
}

// summaryStep scripts a canned summary reply from the FakeProvider.
func summaryStep(summary string, in, out int) llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{textDelta(summary)},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: in, OutputTokens: out},
	}
}

func summaryErrorSteps(err error) []llmtest.Step {
	steps := make([]llmtest.Step, streamRetries+1)
	for i := range steps {
		steps[i] = llmtest.Step{Err: err}
	}
	return steps
}

// makeTurns builds n whole text turns (user + assistant), labelled by index.
func makeTurns(n int) []llm.Message {
	msgs := make([]llm.Message, 0, 2*n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, userText(turnLabel(i)+" question"), asstText(turnLabel(i)+" answer"))
	}
	return msgs
}

func turnLabel(i int) string {
	return string(rune('A' + i))
}

type nativeCompactionProvider struct {
	*llmtest.FakeProvider
	result   llm.CompactedContext
	err      error
	requests []llm.Request
}

func (p *nativeCompactionProvider) CompactContext(_ context.Context, req llm.Request) (llm.CompactedContext, error) {
	p.requests = append(p.requests, req)
	return p.result, p.err
}

func nativeCompactedItems() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"retained state"}]}`),
		json.RawMessage(`{"id":"cmp_1","type":"compaction","encrypted_content":"opaque-window"}`),
	}
}

func TestNativeCompactionKeepsSemanticTranscriptAndReplaysCanonicalWindow(t *testing.T) {
	original := makeTurns(10)
	provider := &nativeCompactionProvider{
		FakeProvider: llmtest.New("responses"),
		result: llm.CompactedContext{
			Items: nativeCompactedItems(),
			Usage: llm.Usage{InputTokens: 200, OutputTokens: 20},
		},
	}
	a := newAgent(provider, tools.Default(), Options{
		Model: "gpt-5.5", ReasoningReplayDomain: "openai:gpt-5", NativeCompaction: true,
		Reasoning:   llm.ReasoningConfig{Effort: "high", Summary: "concise"},
		ServerTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch}},
	})
	a.SetTranscript(cloneMessages(original))

	usage, err := a.Compact(context.Background(), &recordSink{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if usage != provider.result.Usage || len(provider.requests) != 1 {
		t.Fatalf("native compact usage/requests = %+v/%d", usage, len(provider.requests))
	}
	if !reflect.DeepEqual(provider.requests[0].Messages, original) {
		t.Fatalf("native compact input changed semantic history")
	}
	if !reflect.DeepEqual(provider.requests[0].Tools, a.toolSpecs) ||
		!reflect.DeepEqual(provider.requests[0].ServerTools, a.serverTools) ||
		provider.requests[0].Reasoning != a.reasoning {
		t.Fatalf("native compact request omitted model controls: %+v", provider.requests[0])
	}
	transcript := a.Transcript()
	if len(transcript) != len(original)+1 || !reflect.DeepEqual(transcript[:len(original)], original) {
		t.Fatalf("native compaction rewrote semantic transcript: before=%d after=%d", len(original), len(transcript))
	}
	checkpoint := transcript[len(transcript)-1]
	if checkpoint.Origin != llm.MessageOriginProviderCompaction || len(checkpoint.Content) != 1 ||
		checkpoint.Content[0].Kind != llm.BlockProviderCompaction {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
	if got, want := a.triggerTokens(0, 0), estimateTokens(a.providerVisibleMessages(transcript)); got != want {
		t.Fatalf("post-compaction trigger estimate = %d, want provider-visible %d", got, want)
	} else if got >= estimateTokens(transcript) {
		t.Fatalf("post-compaction trigger still counts preserved semantic history: visible=%d full=%d", got, estimateTokens(transcript))
	}

	request := a.DebugRequest(false, "", nil, nil).Request
	if len(request.Messages) != 1 || request.Messages[0].Content[0].Kind != llm.BlockProviderCompaction {
		t.Fatalf("same-domain request messages = %+v, want canonical checkpoint only", request.Messages)
	}
	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("repeat Compact: %v", err)
	}
	if got := len(a.Transcript()); got != len(original)+1 {
		t.Fatalf("repeat native compaction accumulated checkpoints: messages=%d", got)
	}
	if len(provider.requests) != 2 || !hasProviderCompaction(provider.requests[1].Messages) {
		t.Fatalf("repeat native compaction requests = %+v", provider.requests)
	}

	a.SetReasoningReplayDomain("other-provider:model")
	request = a.DebugRequest(false, "", nil, nil).Request
	if !reflect.DeepEqual(request.Messages, original) {
		t.Fatalf("cross-domain request did not recover semantic history: got %d messages", len(request.Messages))
	}
}

func TestNativeCompactionUnsupportedFallsBackToTextualCheckpoint(t *testing.T) {
	provider := &nativeCompactionProvider{
		FakeProvider: llmtest.New("responses", summaryStep("TEXTUAL SUMMARY", 100, 10)),
		err:          llm.ErrContextCompactionUnsupported,
	}
	a := newAgent(provider, tools.Default(), Options{
		Model: "gpt-5.5", ReasoningReplayDomain: "compatible-domain", NativeCompaction: true,
	})
	transcript := append(makeTurns(10), llm.Message{
		Role:   llm.RoleUser,
		Origin: llm.MessageOriginProviderCompaction,
		Content: []llm.ContentBlock{{
			Kind:                  llm.BlockProviderCompaction,
			ReasoningReplayDomain: "compatible-domain",
			ProviderCompaction:    nativeCompactedItems(),
		}},
	})
	a.SetTranscript(transcript)

	usage, err := a.Compact(context.Background(), &recordSink{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if usage != (llm.Usage{InputTokens: 100, OutputTokens: 10}) {
		t.Fatalf("fallback usage = %+v", usage)
	}
	if len(provider.requests) != 1 || provider.RequestCount() != 1 {
		t.Fatalf("native/textual requests = %d/%d, want 1/1", len(provider.requests), provider.RequestCount())
	}
	if hasProviderCompaction(provider.FakeProvider.Requests[0].Messages) {
		t.Fatal("textual fallback replayed a stale native checkpoint")
	}
	if got := a.Transcript()[0]; got.Origin != llm.MessageOriginCompactionCheckpoint || got.Compaction == nil {
		t.Fatalf("fallback checkpoint = %+v", got)
	}
	if hasProviderCompaction(a.Transcript()) {
		t.Fatal("textual fallback retained a stale native checkpoint")
	}
	if a.nativeCompactionAvailable() {
		t.Fatal("unsupported native compaction remained enabled for the replay domain")
	}
}

func TestRejectedNativeCheckpointRetriesSemanticTranscriptAndIsDiscarded(t *testing.T) {
	original := makeTurns(2)
	provider := &nativeCompactionProvider{
		FakeProvider: llmtest.New("responses",
			llmtest.Step{Err: &llm.APIError{StatusCode: 400, Code: "invalid_encrypted_content", Message: "bad checkpoint"}},
			summaryStep("recovered", 40, 4),
		),
		result: llm.CompactedContext{Items: nativeCompactedItems()},
	}
	a := newAgent(provider, tools.Default(), Options{
		Model: "gpt-5.5", ReasoningReplayDomain: "openai:gpt-5", NativeCompaction: true,
	})
	a.SetTranscript(cloneMessages(original))
	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	sink := &recordSink{}
	if err := a.RunPrompt(context.Background(), "next", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if provider.RequestCount() != 2 {
		t.Fatalf("stream requests = %d, want rejected checkpoint plus semantic retry", provider.RequestCount())
	}
	if !hasProviderCompaction(provider.FakeProvider.Requests[0].Messages) {
		t.Fatal("first stream request did not use the native checkpoint")
	}
	if hasProviderCompaction(provider.FakeProvider.Requests[1].Messages) || len(provider.FakeProvider.Requests[1].Messages) != len(original)+1 {
		t.Fatalf("semantic retry messages = %+v", provider.FakeProvider.Requests[1].Messages)
	}
	if hasProviderCompaction(a.Transcript()) {
		t.Fatal("rejected provider checkpoint remained in durable transcript")
	}
	if countNoticesContaining(sink.notices, "provider rejected the checkpoint") != 1 {
		t.Fatalf("checkpoint rejection notices = %v", sink.notices)
	}
}

func TestNativeCheckpointStatefulSuffixUsesVisibleCacheBoundary(t *testing.T) {
	provider := &nativeCompactionProvider{
		FakeProvider: llmtest.New("responses"),
		result:       llm.CompactedContext{Items: nativeCompactedItems()},
	}
	a := newAgent(provider, tools.Default(), Options{
		Model: "gpt-5.5", ReasoningReplayDomain: "openai:gpt-5", NativeCompaction: true, ResponsesStateful: true,
	})
	a.SetTranscript(makeTurns(4))
	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	anchor := len(a.transcript)
	digest, err := llm.FingerprintMessages(a.transcript)
	if err != nil {
		t.Fatal(err)
	}
	a.responseState = llm.ResponseState{PreviousResponseID: "resp_1", AnchorMessages: anchor, AnchorDigest: digest}
	a.transcript = append(a.transcript, userText("new suffix"))

	req := a.modelRequest(nil)
	if !req.usedPrevious || len(req.request.Messages) != 1 || req.request.Messages[0].Content[0].Text != "new suffix" {
		t.Fatalf("stateful native suffix request = %+v", req)
	}
	if req.request.CachePolicy.StableMessagePrefix < 0 || req.request.CachePolicy.StableMessagePrefix > len(req.request.Messages) {
		t.Fatalf("visible cache boundary = %d for %d payload messages", req.request.CachePolicy.StableMessagePrefix, len(req.request.Messages))
	}
}

func TestMaintenanceRequestIdentitiesAreDeterministicAndPurposeScoped(t *testing.T) {
	fp := llmtest.New("fake",
		summaryStep("compaction", 1, 1),
		summaryStep("branch", 1, 1),
	)
	a := newAgent(fp, tools.Default(), Options{Model: "model"})
	a.SetTranscript(makeTurns(10))
	a.SetProxySessionID("harness-session-main")
	a.SetCacheAffinityID("harness-cache-main")
	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.GenerateBranchSummary(context.Background(), a.Transcript(), ""); err != nil {
		t.Fatal(err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(fp.Requests))
	}
	first, branch := fp.Requests[0], fp.Requests[1]
	if first.ProxySessionID == "" ||
		first.CacheAffinityID == "" ||
		first.ProxySessionID == "harness-session-main" ||
		first.CacheAffinityID == "harness-cache-main" {
		t.Fatalf("compaction identities = proxy %q cache %q", first.ProxySessionID, first.CacheAffinityID)
	}
	if branch.ProxySessionID == first.ProxySessionID || branch.CacheAffinityID == first.CacheAffinityID {
		t.Fatalf("branch identities were not purpose-separated: branch=%q/%q compaction=%q/%q",
			branch.ProxySessionID, branch.CacheAffinityID, first.ProxySessionID, first.CacheAffinityID)
	}
	for i, request := range fp.Requests {
		if request.CachePolicy.StaticTTL != llm.CacheTTLDefault {
			t.Fatalf("request %d static TTL = %q, want default", i, request.CachePolicy.StaticTTL)
		}
	}
}

func TestCompactKeepsLastEightTurns(t *testing.T) {
	// Ten whole turns; compaction keeps the latest eight assistant turns and
	// checkpoints the earlier progress and active instructions.
	transcript := makeTurns(10)

	fp := llmtest.New("fake", summaryStep("CANNED SUMMARY", 200, 40))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("system prompt")
	a.SetTranscript(transcript)

	sink := &recordSink{}
	if _, err := a.Compact(context.Background(), sink); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs := a.Transcript()
	mustValid(t, msgs)

	if len(msgs) != 16 {
		t.Fatalf("want checkpoint + 8 kept turns, got %d messages:\n%s", len(msgs), dump(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Errorf("first message should be a user checkpoint, got role %q origin %q", msgs[0].Role, msgs[0].Origin)
	}
	got := msgs[0].Content[0].Text
	if !strings.HasPrefix(got, checkpointHeader) {
		t.Errorf("checkpoint missing header, got %q", got)
	}
	if !strings.Contains(got, "CANNED SUMMARY") {
		t.Errorf("summary message should carry the model's summary, got %q", got)
	}

	// The kept turns are C..J; C's input survives verbatim in the checkpoint.
	if msgs[1].Content[0].Text != "C answer" || !strings.Contains(got, "C question") {
		t.Errorf("first retained turn/checkpoint mismatch: %q / %q", msgs[1].Content[0].Text, got)
	}
	if msgs[15].Content[0].Text != "J answer" {
		t.Errorf("last kept message = %q, want %q", msgs[15].Content[0].Text, "J answer")
	}

	// The summary request received the older turns but never the system prompt
	// as a message (it lives on Request.System).
	if len(fp.Requests) != 1 {
		t.Fatalf("summary call count = %d, want 1", len(fp.Requests))
	}
	if fp.Requests[0].System != prompts.CompactionSummary() {
		t.Fatalf("summary call system prompt = %q, want embedded compaction prompt", fp.Requests[0].System)
	}
	if fp.Requests[0].Purpose != llm.RequestPurposeCompaction {
		t.Fatalf("summary call purpose = %q, want %q", fp.Requests[0].Purpose, llm.RequestPurposeCompaction)
	}
}

func TestPreferredCompactionBoundaryUsesTokenFloorAndRoundCap(t *testing.T) {
	large := make([]llm.Message, 0, 20)
	for i := 0; i < 10; i++ {
		large = append(large, userText("q"), asstText(strings.Repeat(string(rune('a'+i)), 4_000)))
	}
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{CompactKeepTokens: 2_500, CompactKeepTurns: 8})
	a.SetTranscript(large)
	turns := completedTurnSpans(large)
	boundary := a.preferredCompactionBoundary(turns)
	if got := countCompletedTurns(large[boundary:]); got != 3 {
		t.Fatalf("token-floor suffix kept %d rounds, want 3", got)
	}

	a.SetTranscript(makeTurns(10))
	turns = completedTurnSpans(a.Transcript())
	boundary = a.preferredCompactionBoundary(turns)
	if got := countCompletedTurns(a.Transcript()[boundary:]); got != 8 {
		t.Fatalf("small-round suffix kept %d rounds, want cap 8", got)
	}
}

func TestCompactForContinuationCheckpointsAndArchivesCompleteTranscript(t *testing.T) {
	transcript := makeTurns(3)
	transcript[0].Origin = llm.MessageOriginPrompt
	fp := llmtest.New("fake", summaryStep("CONTINUATION STATE", 120, 18))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("system prompt")
	a.SetTranscript(transcript)
	a.SetProxySessionID("proxy-source")
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "response-source"})

	var archived CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archived = archive
		return "compactions/0001.input.json", nil
	})
	usage, changed, err := a.CompactForContinuation(context.Background(), &recordSink{})
	if err != nil {
		t.Fatalf("CompactForContinuation: %v", err)
	}
	if !changed || usage.InputTokens != 120 || usage.OutputTokens != 18 {
		t.Fatalf("continuation compaction = changed %t usage %+v", changed, usage)
	}
	if len(fp.Requests) != 1 || len(fp.Requests[0].Messages) != len(transcript)+1 ||
		!strings.Contains(dump(fp.Requests[0].Messages), "C answer") {
		t.Fatalf("summary request omitted complete transcript: %s", dump(fp.Requests[0].Messages))
	}
	if !reflect.DeepEqual(archived.Messages, transcript) || archived.Summary != "CONTINUATION STATE" {
		t.Fatalf("archive = %+v, want complete source transcript and summary", archived)
	}

	got := a.Transcript()
	mustValid(t, got)
	if len(got) != 1 || got[0].Origin != llm.MessageOriginCompactionCheckpoint || got[0].Compaction == nil {
		t.Fatalf("continuation transcript = %+v, want one typed checkpoint", got)
	}
	if got[0].Compaction.Summary != "CONTINUATION STATE" ||
		!strings.Contains(got[0].Content[0].Text, "A question") ||
		!strings.Contains(got[0].Content[0].Text, "compactions/0001.input.json") {
		t.Fatalf("continuation checkpoint = %+v", got[0])
	}
	if a.ProxySessionID() == "proxy-source" || a.ProxySessionID() == "" || a.ResponseState() != nil {
		t.Fatalf("continuation compaction retained remote anchor: proxy=%q state=%+v", a.ProxySessionID(), a.ResponseState())
	}
}

// Regression: low-water pressure used to drop retained rounds after the summary
// and archive boundary had already been fixed. Every moved round must be included
// in a regenerated summary and in the final raw archive.
func TestLowWaterBoundaryMoveRegeneratesSummaryAndArchive(t *testing.T) {
	transcript := make([]llm.Message, 0, 20)
	for i := 0; i < 10; i++ {
		transcript = append(transcript, userText(turnLabel(i)+" question"), asstText(turnLabel(i)+" "+strings.Repeat("x", 3_500)))
	}
	fp := llmtest.New("fake", summaryStep("first", 10, 1), summaryStep("second", 20, 2), summaryStep("third", 100, 23), summaryStep("fourth", 200, 50))
	a := newAgent(fp, &tools.Registry{}, Options{Model: "local", ContextWindow: 10_000, CompactKeepTokens: 2_000})
	a.SetTranscript(transcript)
	var archived CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archived = archive
		return "archive", nil
	})
	usage, err := a.Compact(context.Background(), &recordSink{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(fp.Requests) != 4 || usage.InputTokens != 330 || usage.OutputTokens != 76 {
		t.Fatalf("summary retries = %d usage=%+v, want 4 and cumulative usage", len(fp.Requests), usage)
	}
	if got := countCompletedTurns(archived.Messages); got != 7 {
		t.Fatalf("archived rounds = %d, want final moved boundary with 7", got)
	}
	// The boundary move must regenerate the summary: the final reduce request
	// folds the chunk covering the newly moved rounds instead of reusing the
	// summary produced for the earlier boundary.
	if !strings.Contains(dump(fp.Requests[3].Messages), "Chunk 3 summary") {
		t.Fatalf("final reduce omitted the regenerated chunk summaries: %s", dump(fp.Requests[3].Messages))
	}
	if !strings.Contains(a.Transcript()[0].Content[0].Text, "fourth") {
		t.Fatalf("checkpoint kept the stale pre-move summary: %s", a.Transcript()[0].Content[0].Text)
	}
}

func TestAutomaticCompactionCanBeDisabledWithoutDisablingManual(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("manual summary", 10, 2))
	a := newAgent(fp, tools.Default(), Options{DisableAutoCompaction: true})
	a.SetTranscript(makeTurns(10))

	if usage, changed, err := a.MaybeCompact(context.Background(), a.window(), &recordSink{}); err != nil || changed || usage != (llm.Usage{}) {
		t.Fatalf("disabled automatic compaction = usage %+v changed=%t err=%v", usage, changed, err)
	}
	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("manual compaction: %v", err)
	}
	if fp.RequestCount() != 1 || a.Transcript()[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("manual compaction did not run with auto disabled: requests=%d", fp.RequestCount())
	}
}

func TestNondefaultCompactionPercentagesMoveTriggerAndBudget(t *testing.T) {
	a := newAgent(llmtest.New("fake"), &tools.Registry{}, Options{
		Model:                 "local",
		ContextWindow:         10_000,
		CompactTriggerPercent: 90,
		CompactTargetPercent:  50,
	})
	if a.overThreshold(8_999) || !a.overThreshold(9_000) {
		t.Fatalf("90%% trigger did not move threshold")
	}
	if got := a.compactBudget(); got != 5_000 {
		t.Fatalf("50%% target budget = %d, want 5000", got)
	}
}

func TestCompactionFileActivityIsCumulativeAndModifiedWins(t *testing.T) {
	reg := tools.Catalog()
	a := newAgent(llmtest.New("fake"), reg, Options{})
	failed := toolResult("r2", "missing")
	failed.Content[0].ResultError = true
	messages := []llm.Message{
		asstToolUse("r1", "read", `{"path":"z.go"}`), toolResult("r1", "ok"),
		asstToolUse("r3", "read", `{"path":"./a.go"}`), toolResult("r3", "ok"),
		asstToolUse("r2", "read", `{"path":"failed.go"}`), failed,
		asstToolUse("w1", "write", `{"path":"a.go","content":"x"}`), toolResult("w1", "ok"),
		asstToolUse("e1", "edit", `{"files":[{"path":"edit.go","edits":[{"oldText":"a","newText":"b"}]}]}`), toolResult("e1", "ok"),
		asstToolUse("u1", "shell", `{"args":["touch","ignored.go"]}`), toolResult("u1", "ok"),
	}
	prior := &llm.CompactionMetadata{ReadFiles: []string{"prior.go"}, ModifiedFiles: []string{"already.go"}}
	reads, modified, omitted := a.compactionFileActivity(messages, prior)
	if omitted != 0 {
		t.Fatalf("read files omitted = %d, want 0", omitted)
	}
	if got, want := strings.Join(reads, ","), "prior.go,z.go"; got != want {
		t.Fatalf("read files = %q, want %q", got, want)
	}
	if got, want := strings.Join(modified, ","), "a.go,already.go,edit.go"; got != want {
		t.Fatalf("modified files = %q, want %q", got, want)
	}
}

func TestIdleCompactionAppliesMatchingSnapshot(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("idle summary", 100, 12))
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 10_000})
	a.SetTranscript(makeTurns(10))
	var archives []CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archives = append(archives, archive)
		return "compactions/idle.input.json", nil
	})

	work, ok, err := a.PrepareIdleCompaction(1)
	if err != nil || !ok {
		t.Fatalf("PrepareIdleCompaction = ok %t err %v, want work", ok, err)
	}
	result, err := work(context.Background())
	if err != nil {
		t.Fatalf("idle work: %v", err)
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 12 {
		t.Fatalf("idle usage = %+v, want 100/12", result.Usage)
	}
	if len(archives) != 0 {
		t.Fatal("background preparation ran the live archive callback")
	}

	sink := &recordSink{}
	applied, err := a.ApplyIdleCompaction(context.Background(), sink, result)
	if err != nil || !applied {
		t.Fatalf("ApplyIdleCompaction = applied %t err %v", applied, err)
	}
	if len(archives) != 1 || archives[0].Summary != "idle summary" {
		t.Fatalf("applied archives = %+v, want one idle summary", archives)
	}
	got := a.Transcript()
	if len(got) != 16 || got[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("idle transcript = %d messages, want checkpoint + 8 turns", len(got))
	}
	if !strings.Contains(got[0].Content[0].Text, "compactions/idle.input.json") {
		t.Fatalf("idle checkpoint missing archive reference: %q", got[0].Content[0].Text)
	}
	if len(sink.notices) != 1 || !strings.HasPrefix(sink.notices[0], "[idle compacted:") {
		t.Fatalf("idle notices = %v", sink.notices)
	}
}

func TestIdleCompactionDiscardsStaleSnapshotBeforeArchiving(t *testing.T) {
	for _, mutate := range []struct {
		name string
		run  func(*Agent)
	}{
		{
			name: "transcript",
			run: func(a *Agent) {
				a.SetTranscript(append(cloneMessages(a.Transcript()), userText("new prompt")))
			},
		},
		{
			name: "runtime",
			run: func(a *Agent) {
				a.SetReasoning(llm.ReasoningConfig{Profile: "high"})
			},
		},
		{
			name: "reasoning replay disabled",
			run: func(a *Agent) {
				a.disableCurrentReasoningReplay()
			},
		},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			fp := llmtest.New("fake", summaryStep("idle summary", 10, 2))
			a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 10_000})
			a.SetTranscript(makeTurns(10))
			archived := false
			a.SetCompactionArchiver(func(_ context.Context, _ CompactionArchive) (string, error) {
				archived = true
				return "archive", nil
			})
			work, ok, err := a.PrepareIdleCompaction(1)
			if err != nil || !ok {
				t.Fatalf("PrepareIdleCompaction = ok %t err %v", ok, err)
			}
			result, err := work(context.Background())
			if err != nil {
				t.Fatalf("idle work: %v", err)
			}
			mutate.run(a)
			before := cloneMessages(a.Transcript())

			applied, err := a.ApplyIdleCompaction(context.Background(), &recordSink{}, result)
			if err != nil || applied {
				t.Fatalf("ApplyIdleCompaction = applied %t err %v, want stale discard", applied, err)
			}
			if archived {
				t.Fatal("stale idle candidate ran archive callback")
			}
			if !reflect.DeepEqual(a.Transcript(), before) {
				t.Fatal("stale idle candidate changed the live transcript")
			}
		})
	}
}

func TestPrepareIdleCompactionEligibility(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "local", ContextWindow: 1_000_000})
	a.SetTranscript(makeTurns(2))
	if _, ok, err := a.PrepareIdleCompaction(40); err != nil || ok {
		t.Fatalf("below-threshold preparation = ok %t err %v", ok, err)
	}
	if _, ok, err := a.PrepareIdleCompaction(100); err == nil || ok {
		t.Fatalf("invalid trigger preparation = ok %t err %v", ok, err)
	}

	disabled := newAgent(llmtest.New("fake"), tools.Default(), Options{
		Model:                 "local",
		ContextWindow:         1,
		DisableAutoCompaction: true,
	})
	disabled.SetTranscript(makeTurns(10))
	if _, ok, err := disabled.PrepareIdleCompaction(1); err != nil || ok {
		t.Fatalf("disabled preparation = ok %t err %v", ok, err)
	}

	hookConfig, err := hooks.DecodeEventMap(json.RawMessage(`{
		"PreCompact": [{"hooks": [{"type": "command", "command": "true"}]}]
	}`))
	if err != nil {
		t.Fatalf("decode hook config: %v", err)
	}
	hooked := newAgent(llmtest.New("fake"), tools.Default(), Options{
		Model:         "local",
		ContextWindow: 10_000,
		Hooks:         &hooks.Runner{Config: hookConfig},
	})
	hooked.SetTranscript(makeTurns(10))
	if _, ok, err := hooked.PrepareIdleCompaction(1); err != nil || ok {
		t.Fatalf("hooked preparation = ok %t err %v", ok, err)
	}
}

func TestCompactWithFocusStoresOnlyCurrentFocus(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("focused summary", 10, 2), summaryStep("later summary", 11, 3))
	a := newAgent(fp, tools.Default(), Options{})
	a.SetTranscript(makeTurns(10))
	var archives []CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archives = append(archives, archive)
		return "archive", nil
	})
	if _, err := a.CompactWithFocus(context.Background(), &recordSink{}, "  preserve API names  "); err != nil {
		t.Fatalf("focused compact: %v", err)
	}
	if !strings.Contains(fp.Requests[0].System, "preserve API names") || a.Transcript()[0].Compaction.Focus != "preserve API names" || archives[0].Focus != "preserve API names" {
		t.Fatalf("focus was not propagated: request=%q checkpoint=%+v archive=%+v", fp.Requests[0].System, a.Transcript()[0].Compaction, archives[0])
	}

	a.SetTranscript(append(a.Transcript(), makeTurns(9)...))
	if _, _, err := a.compactTriggered(context.Background(), &recordSink{}, "auto"); err != nil {
		t.Fatalf("later automatic compact: %v", err)
	}
	if fp.Requests[1].System != prompts.CompactionUpdate() {
		t.Fatalf("repeated compaction system = %q, want update prompt", fp.Requests[1].System)
	}
	if a.Transcript()[0].Compaction.Focus != "" || archives[1].Focus != "" || strings.Contains(fp.Requests[1].System, "preserve API names") {
		t.Fatalf("one-shot focus leaked into later compaction")
	}
	metadata := fp.Requests[1].Messages[0].Content[0].Text
	if !strings.Contains(metadata, `"previous_summary":"focused summary"`) {
		t.Fatalf("update metadata missing exact prior summary: %s", metadata)
	}
	for _, message := range fp.Requests[1].Messages[1:] {
		if message.Origin == llm.MessageOriginCompactionCheckpoint || strings.Contains(messageTextForCheckpoint(message), checkpointHeader) {
			t.Fatalf("prior checkpoint was sent as ordinary update history: %+v", message)
		}
	}
}

func TestRepeatedCompactionMapReduceUsesPriorSummaryOnlyInFinalUpdate(t *testing.T) {
	fp := llmtest.New("fake",
		summaryStep("chunk one", 10, 1),
		summaryStep("chunk two", 11, 1),
		summaryStep("chunk three", 12, 1),
		summaryStep("updated", 13, 2),
	)
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 4_000})
	checkpoint := userText("rendered checkpoint")
	checkpoint.Origin = llm.MessageOriginCompactionCheckpoint
	checkpoint.Compaction = &llm.CompactionMetadata{Summary: "EXACT PRIOR SUMMARY"}
	older := []llm.Message{checkpoint}
	for i := 0; i < 3; i++ {
		older = append(older, userText("q"), asstText(strings.Repeat(string(rune('a'+i)), 6_000)))
	}
	got, _, err := a.summarizeCompaction(context.Background(), older, checkpoint.Compaction, []string{"read.go"}, 0, nil, "focus here")
	if err != nil {
		t.Fatalf("summarizeCompaction: %v", err)
	}
	if got != "updated" || len(fp.Requests) != 4 {
		t.Fatalf("summary=%q requests=%d, want updated/4", got, len(fp.Requests))
	}
	for i, request := range fp.Requests[:3] {
		if !strings.HasPrefix(request.System, prompts.CompactionSummary()) || !strings.Contains(request.System, "focus here") {
			t.Fatalf("map request %d system = %q", i+1, request.System)
		}
		for _, message := range request.Messages {
			if strings.Contains(messageTextForCheckpoint(message), "EXACT PRIOR SUMMARY") {
				t.Fatalf("map request %d received prior summary", i+1)
			}
		}
	}
	final := fp.Requests[3]
	if !strings.HasPrefix(final.System, prompts.CompactionUpdate()) || !strings.Contains(final.System, "focus here") {
		t.Fatalf("final update system = %q", final.System)
	}
	if count := strings.Count(final.Messages[0].Content[0].Text, "EXACT PRIOR SUMMARY"); count != 1 {
		t.Fatalf("prior summary count in final metadata = %d: %q", count, final.Messages[0].Content[0].Text)
	}
}

func TestFocusedCompactionIncludesFocusInHooks(t *testing.T) {
	dir := t.TempDir()
	prePath := filepath.Join(dir, "pre.json")
	postPath := filepath.Join(dir, "post.json")
	configBody, err := json.Marshal(map[string]any{
		"PreCompact":  []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "cat > " + prePath}}}},
		"PostCompact": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "cat > " + postPath}}}},
	})
	if err != nil {
		t.Fatalf("marshal hooks: %v", err)
	}
	cfg, err := hooks.DecodeEventMap(configBody)
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	fp := llmtest.New("fake", summaryStep("summary", 10, 2))
	a := newAgent(fp, tools.Default(), Options{Hooks: &hooks.Runner{Config: cfg}})
	a.SetTranscript(makeTurns(10))
	if _, err := a.CompactWithFocus(context.Background(), &recordSink{}, "public API"); err != nil {
		t.Fatalf("CompactWithFocus: %v", err)
	}
	for _, path := range []string{prePath, postPath} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read hook payload: %v", err)
		}
		if !strings.Contains(string(raw), `"trigger":"manual"`) || !strings.Contains(string(raw), `"focus":"public API"`) {
			t.Fatalf("hook payload missing focus/trigger: %s", raw)
		}
	}
}

func TestCompactRotatesProxySessionID(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("CANNED SUMMARY", 200, 40))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("system prompt")
	a.SetTranscript(makeTurns(10))
	before := a.ProxySessionID()
	cacheBefore := a.CacheAffinityID()

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	after := a.ProxySessionID()
	if before == "" || after == "" || before == after {
		t.Fatalf("proxy session id = %q then %q, want rotation after transcript rewrite", before, after)
	}
	if cacheAfter := a.CacheAffinityID(); cacheAfter != cacheBefore {
		t.Fatalf("cache affinity id = %q then %q, want it preserved across compaction", cacheBefore, cacheAfter)
	}
}

func TestCompactKeepsToolPairsWhole(t *testing.T) {
	// A turn that spans a tool round-trip must be kept whole: no tool_use may be
	// separated from its tool_result by the kept-turns boundary.
	var transcript []llm.Message
	transcript = append(transcript, makeTurns(6)...)
	// Turn G: user, assistant(tool_use), user(tool_result), assistant(text).
	transcript = append(transcript,
		userText("G question"),
		asstToolUse("call_g", "echo", `{}`),
		toolResult("call_g", "tool output"),
		asstText("G answer"),
	)
	transcript = append(transcript, makeTurns(3)...) // H, I, J relabelled below

	fp := llmtest.New("fake", summaryStep("S", 100, 20))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)

	// The kept boundary must not split the G tool pair: if turn G is kept, both
	// its tool_use and tool_result survive; the validation above already proves
	// no pair is split, but assert the tool_result is present when its use is.
	var sawUse, sawResult bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolUse && b.ToolUseID == "call_g" {
				sawUse = true
			}
			if b.Kind == llm.BlockToolResult && b.ResultForID == "call_g" {
				sawResult = true
			}
		}
	}
	if sawUse != sawResult {
		t.Errorf("tool pair split across the compaction boundary: use=%v result=%v", sawUse, sawResult)
	}
}

func TestCompactBelowThresholdUntouched(t *testing.T) {
	// Only the post-turn trigger should be threshold-gated; a transcript whose
	// last reported input is below 78% of the window is left alone.
	transcript := makeTurns(10)
	fp := llmtest.New("fake") // no summary step scripted
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	window := a.window()
	below := window / 2 // well under 78%

	sink := &recordSink{}
	if _, _, err := a.MaybeCompact(context.Background(), below, sink); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if len(a.Transcript()) != len(transcript) {
		t.Errorf("below-threshold transcript should be untouched, got %d messages", len(a.Transcript()))
	}
	if len(fp.Requests) != 0 {
		t.Errorf("no summary call should happen below threshold, got %d", len(fp.Requests))
	}
	if len(sink.notices) != 0 {
		t.Errorf("no compaction notice below threshold, got %v", sink.notices)
	}
}

func TestMaybeCompactAboveThresholdCompacts(t *testing.T) {
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 50, 10))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	window := a.window()
	above := window * 80 / 100 // ≥ 78%

	if _, _, err := a.MaybeCompact(context.Background(), above, &recordSink{}); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	if len(msgs) != 16 {
		t.Fatalf("above threshold should compact to checkpoint + 8 turns, got %d", len(msgs))
	}
	if len(fp.Requests) != 1 {
		t.Errorf("summary call count = %d, want 1", len(fp.Requests))
	}
}

// A single prompt can contain many conversational turns. Compaction must count
// each assistant response (plus its tool-result batch) as a turn, preserve the
// latest eight pairs intact, and leave enough headroom that the next small turn
// does not immediately trigger another summary call.
func TestMaybeCompactLongSinglePromptUsesTurnBoundariesAndLowWaterMark(t *testing.T) {
	const window = 20_000
	prompt := userText("implement the complete request exactly")
	prompt.Origin = llm.MessageOriginPrompt
	transcript := []llm.Message{prompt}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("call_%02d", i)
		transcript = append(transcript,
			asstToolUse(id, "read", fmt.Sprintf(`{"path":"file_%02d.go"}`, i)),
			toolResult(id, fmt.Sprintf("result-%02d-%s", i, strings.Repeat("x", 5_500))),
		)
		if i == 1 {
			steer := userText("also preserve the public API names")
			steer.Origin = llm.MessageOriginSteer
			transcript = append(transcript, steer)
		}
	}

	fp := llmtest.New("fake",
		summaryStep("FOUR EARLY TURNS COMPLETE", 60, 12),
		summaryStep("NEXT FOUR TURNS COMPLETE", 70, 14),
	)
	a := newAgent(fp, &tools.Registry{}, Options{Model: "local", ContextWindow: window, CompactKeepTurns: 8, CompactKeepTokens: 20_000})
	a.SetSystem("sys")
	a.SetTranscript(transcript)
	if got := len(completedTurnSpans(a.Transcript())); got != 12 {
		t.Fatalf("completed turns = %d, want 12", got)
	}
	if before := a.estimateContext(nil).Total; before*100 < window*compactThresholdPct {
		t.Fatalf("test setup context = %d, want at least %d%% of %d", before, compactThresholdPct, window)
	}

	var archived CompactionArchive
	a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		archived = archive
		return "compactions/0001.input.json", nil
	})
	sink := &recordSink{}
	usage, changed, err := a.MaybeCompact(context.Background(), window*compactThresholdPct/100, sink)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if !changed {
		t.Fatal("long single-prompt transcript was not compacted")
	}
	if usage.InputTokens != 60 || usage.OutputTokens != 12 || len(fp.Requests) != 1 {
		t.Fatalf("summary usage/requests = %+v / %d, want 60/12 and one request", usage, len(fp.Requests))
	}
	if got := countCompletedTurns(archived.Messages); got != 4 {
		t.Fatalf("archived completed turns = %d, want 4", got)
	}

	got := a.Transcript()
	mustValid(t, got)
	if len(got) != 17 || got[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("compacted transcript should be checkpoint + 8 tool turns, got %d messages:\n%s", len(got), dump(got))
	}
	checkpoint := got[0].Content[0].Text
	for _, exact := range []string{"implement the complete request exactly", "also preserve the public API names", "FOUR EARLY TURNS COMPLETE", "compactions/0001.input.json"} {
		if !strings.Contains(checkpoint, exact) {
			t.Fatalf("checkpoint missing %q:\n%s", exact, checkpoint)
		}
	}
	for i := 0; i < 8; i++ {
		wantID := fmt.Sprintf("call_%02d", i+4)
		assistant := got[1+i*2]
		result := got[2+i*2]
		if assistant.Content[0].ToolUseID != wantID || result.Content[0].ResultForID != wantID {
			t.Fatalf("kept turn %d split or reordered: tool_use=%q tool_result=%q want %q", i+1, assistant.Content[0].ToolUseID, result.Content[0].ResultForID, wantID)
		}
	}
	after := a.estimateContext(nil).Total
	if after*100 > window*compactTargetPct {
		t.Fatalf("post-compaction context = %d, want at most %d%% of %d", after, compactTargetPct, window)
	}

	// A small next turn remains below the 78%% trigger, proving the 65%% target
	// provides hysteresis instead of causing per-turn compaction churn.
	withNext := append(a.Transcript(),
		asstToolUse("call_12", "read", `{"path":"small.go"}`),
		toolResult("call_12", "small follow-up complete"),
	)
	a.SetTranscript(withNext)
	nextSize := a.estimateContext(nil).Total
	if nextSize*100 >= window*compactThresholdPct {
		t.Fatalf("small next turn unexpectedly crossed trigger: %d", nextSize)
	}
	_, changed, err = a.MaybeCompact(context.Background(), nextSize, sink)
	if err != nil || changed || len(fp.Requests) != 1 {
		t.Fatalf("small next turn caused compaction churn: changed=%v err=%v requests=%d", changed, err, len(fp.Requests))
	}

	// Once enough new large turns accumulate, a second compaction carries the
	// original prompt and steer forward without recursively nesting the prior
	// checkpoint or treating it as a conversational turn.
	withMore := a.Transcript()
	for i := 13; i < 16; i++ {
		id := fmt.Sprintf("call_%02d", i)
		withMore = append(withMore,
			asstToolUse(id, "read", fmt.Sprintf(`{"path":"file_%02d.go"}`, i)),
			toolResult(id, fmt.Sprintf("result-%02d-%s", i, strings.Repeat("y", 6_000))),
		)
	}
	a.SetTranscript(withMore)
	secondSize := a.estimateContext(nil).Total
	if secondSize*100 < window*compactThresholdPct {
		t.Fatalf("second compaction setup remained below trigger: %d", secondSize)
	}
	_, changed, err = a.MaybeCompact(context.Background(), secondSize, sink)
	if err != nil || !changed || len(fp.Requests) != 2 {
		t.Fatalf("second compaction = changed=%v err=%v requests=%d", changed, err, len(fp.Requests))
	}
	secondCheckpoint := a.Transcript()[0].Content[0].Text
	for _, exact := range []string{"implement the complete request exactly", "also preserve the public API names", "NEXT FOUR TURNS COMPLETE"} {
		if !strings.Contains(secondCheckpoint, exact) {
			t.Fatalf("second checkpoint lost %q:\n%s", exact, secondCheckpoint)
		}
	}
	if strings.Count(secondCheckpoint, checkpointHeader) != 1 {
		t.Fatalf("second checkpoint nested the prior checkpoint header:\n%s", secondCheckpoint)
	}
	if after := a.estimateContext(nil).Total; after*100 > window*compactTargetPct {
		t.Fatalf("second post-compaction context = %d, want at most %d%% of %d", after, compactTargetPct, window)
	}
}

func TestCompactSummaryFailureKeepsTranscript(t *testing.T) {
	transcript := makeTurns(10)
	// A persistent failure: every retry attempt errors, so the summary call gives
	// up and the transcript is kept intact (r32 retries 1 + streamRetries times).
	errSteps := make([]llmtest.Step, streamRetries+1)
	for i := range errSteps {
		errSteps[i] = llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 7, OutputTokens: 1}}},
			Err:    errors.New("api down"),
		}
	}
	fp := llmtest.New("fake", errSteps...)
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	sink := &recordSink{}
	usage, err := a.Compact(context.Background(), sink)
	if err == nil {
		t.Fatalf("Compact should return the summary-call error")
	}
	if usage.InputTokens != 7*(streamRetries+1) || usage.OutputTokens != streamRetries+1 {
		t.Fatalf("failed summary usage = %+v, want every reported retry attempt", usage)
	}
	// Full transcript intact — a visible context-length failure beats data loss.
	if len(a.Transcript()) != len(transcript) {
		t.Errorf("failed compaction must keep the full transcript, got %d messages", len(a.Transcript()))
	}
	mustValid(t, a.Transcript())
	var warned bool
	for _, n := range sink.notices {
		if strings.Contains(strings.ToLower(n), "compact") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a summary-call failure should warn via the sink, notices=%v", sink.notices)
	}
}

func TestForegroundCompactionFallsBackOnSummaryFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Agent, EventSink) (bool, error)
	}{
		{
			name: "manual",
			run: func(a *Agent, sink EventSink) (bool, error) {
				_, err := a.Compact(context.Background(), sink)
				return err == nil, err
			},
		},
		{
			name: "automatic pressure",
			run: func(a *Agent, sink EventSink) (bool, error) {
				_, changed, err := a.compactTriggered(context.Background(), sink, "context-overflow")
				return changed, err
			},
		},
		{
			name: "continuation",
			run: func(a *Agent, sink EventSink) (bool, error) {
				_, changed, err := a.CompactForContinuation(context.Background(), sink)
				return changed, err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			transcript := makeTurns(10)
			fp := llmtest.New("fake", summaryErrorSteps(errors.New("provider unavailable"))...)
			a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
			a.SetSleep(func(time.Duration) {})
			a.SetTranscript(transcript)
			var archived CompactionArchive
			a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
				archived = archive
				return "compactions/fallback.input.json", nil
			})
			sink := &recordSink{}

			changed, err := test.run(a, sink)
			if err != nil || !changed {
				t.Fatalf("fallback compaction = changed %t err %v", changed, err)
			}
			if archived.SummarySource != compactionSummarySourceDeterministic || archived.FallbackReason != compactionFallbackProviderError {
				t.Fatalf("archive provenance = %q/%q", archived.SummarySource, archived.FallbackReason)
			}
			if len(archived.Messages) == 0 || len(archived.Messages) > len(transcript) {
				t.Fatalf("archive message count = %d", len(archived.Messages))
			}
			if !reflect.DeepEqual(archived.Messages, transcript[:len(archived.Messages)]) {
				t.Fatal("fallback archive did not preserve the exact removed prefix")
			}
			checkpoint := a.Transcript()[0]
			if checkpoint.Compaction == nil || checkpoint.Compaction.SummarySource != compactionSummarySourceDeterministic || checkpoint.Compaction.FallbackReason != compactionFallbackProviderError {
				t.Fatalf("checkpoint provenance = %+v", checkpoint.Compaction)
			}
			if !strings.Contains(checkpoint.Compaction.Summary, "Model-generated compaction summary unavailable (provider error)") ||
				!strings.Contains(checkpoint.Content[0].Text, "compactions/fallback.input.json") {
				t.Fatalf("fallback checkpoint = %+v", checkpoint)
			}
			if !slices.Contains(sink.notices, "[compact summary failed; used deterministic checkpoint]") {
				t.Fatalf("fallback notice missing: %v", sink.notices)
			}
			mustValid(t, a.Transcript())
		})
	}
}

func TestCompactionTimeoutUsesDeterministicCheckpoint(t *testing.T) {
	started := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{Block: func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}})
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8", CompactTimeout: 10 * time.Millisecond})
	a.SetTranscript(makeTurns(10))
	var archived CompactionArchive
	a.SetCompactionArchiver(func(ctx context.Context, archive CompactionArchive) (string, error) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("archive inherited expired summary context: %v", err)
		}
		archived = archive
		return "compactions/timeout.input.json", nil
	})
	sink := &recordSink{}

	if _, err := a.Compact(context.Background(), sink); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	<-started
	if archived.FallbackReason != compactionFallbackTimeout || archived.SummarySource != compactionSummarySourceDeterministic {
		t.Fatalf("archive provenance = %q/%q", archived.SummarySource, archived.FallbackReason)
	}
	if !slices.Contains(sink.notices, "[compact summary timed out after 10ms; used deterministic checkpoint]") {
		t.Fatalf("timeout notice missing: %v", sink.notices)
	}
}

func TestCompactionParentCancellationDoesNotFallback(t *testing.T) {
	started := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{Block: func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}})
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8", CompactTimeout: time.Minute})
	transcript := makeTurns(10)
	a.SetTranscript(transcript)
	archived := false
	a.SetCompactionArchiver(func(_ context.Context, _ CompactionArchive) (string, error) {
		archived = true
		return "archive", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := a.Compact(ctx, &recordSink{})
		errCh <- err
	}()
	<-started
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Compact error = %v, want context canceled", err)
	}
	if archived {
		t.Fatal("parent cancellation archived a fallback checkpoint")
	}
	if !reflect.DeepEqual(a.Transcript(), transcript) {
		t.Fatal("parent cancellation rewrote the transcript")
	}
}

func TestIdleCompactionTimeoutDoesNotPrepareFallback(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Block: func(ctx context.Context) { <-ctx.Done() }})
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: 10_000, CompactTimeout: 10 * time.Millisecond})
	transcript := makeTurns(10)
	a.SetTranscript(transcript)
	work, ok, err := a.PrepareIdleCompaction(1)
	if err != nil || !ok {
		t.Fatalf("PrepareIdleCompaction = ok %t err %v", ok, err)
	}
	result, err := work(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || result.Prepared {
		t.Fatalf("idle timeout = prepared %t err %v", result.Prepared, err)
	}
	if !reflect.DeepEqual(a.Transcript(), transcript) {
		t.Fatal("idle timeout rewrote the live transcript")
	}
}

func TestCompactionFallbackArchiveFailureKeepsTranscript(t *testing.T) {
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryErrorSteps(errors.New("provider unavailable"))...)
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetTranscript(transcript)
	a.SetCompactionArchiver(func(_ context.Context, _ CompactionArchive) (string, error) {
		return "", errors.New("disk full")
	})
	sink := &recordSink{}

	if _, err := a.Compact(context.Background(), sink); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Compact error = %v, want archive failure", err)
	}
	if !reflect.DeepEqual(a.Transcript(), transcript) {
		t.Fatal("archive failure rewrote the transcript")
	}
	for _, notice := range sink.notices {
		if strings.Contains(notice, "used deterministic checkpoint") {
			t.Fatalf("failed archive claimed fallback was applied: %v", sink.notices)
		}
	}
}

func TestCompactDegradesToLastTurnWhenOversized(t *testing.T) {
	// The last four turns together exceed the budget but the last turn alone fits;
	// the ladder drops to the last turn. Budget is 78% of the 1M window ≈ 3.12M
	// bytes; four 1M-byte turns overflow, one does not.
	big := strings.Repeat("x", 1_000_000) // ~250k token estimate per kept turn
	var transcript []llm.Message
	transcript = append(transcript, makeTurns(4)...) // older turns, summarized
	for i := 0; i < 4; i++ {
		transcript = append(transcript, userText("Q"+turnLabel(i)), asstText(big))
	}

	fp := llmtest.New("fake", summaryStep("S", 10, 5))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)
	// checkpoint + the single last assistant turn = 2 messages.
	if len(msgs) != 2 {
		t.Fatalf("oversized kept turns should drop to checkpoint + last turn, got %d:\n%s", len(msgs), dumpShort(msgs))
	}
	if msgs[1].Content[0].Text != big {
		t.Errorf("the single kept turn should be the most recent one")
	}
}

func TestCompactHardTruncatesWhenSingleTurnOversized(t *testing.T) {
	// Even the last turn alone is over budget: the ladder hard-truncates the
	// largest tool results in place, leaving a marker, and never wedges.
	huge := strings.Repeat("y", 5_000_000) // ~1.25M token estimate, over the window
	var transcript []llm.Message
	transcript = append(transcript, makeTurns(4)...)
	transcript = append(transcript,
		userText("final"),
		asstToolUse("c1", "big", `{}`),
		toolResult("c1", huge),
		asstText("done"),
	)

	fp := llmtest.New("fake", summaryStep("S", 10, 5))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	msgs := a.Transcript()
	mustValid(t, msgs)

	var retainedHuge bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && len(b.ResultText) >= len(huge) {
				retainedHuge = true
			}
		}
	}
	if retainedHuge {
		t.Errorf("the oversized tool result should not remain in active context")
	}
}

func TestCompactUsageReported(t *testing.T) {
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 9100, 400))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	sink := &recordSink{}
	if _, err := a.Compact(context.Background(), sink); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// The compaction report names semantic turns and the context reduction.
	var report string
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted") {
			report = n
		}
	}
	if report == "" {
		t.Fatalf("expected a [compacted: …] notice, got %v", sink.notices)
	}
	if !strings.Contains(report, "turns → checkpoint") || !strings.Contains(report, "ctx ~") {
		t.Errorf("compaction report should show semantic turns and context reduction, got %q", report)
	}
}

func TestCompactionReportShowsTurnsAndContextReduction(t *testing.T) {
	report := compactionReport(3, 9100, 400)
	if report != "[compacted: 3 turns → checkpoint · ctx ~9.1k → ~0.4k]" {
		t.Fatalf("compaction report = %q", report)
	}
}

func TestCompactArchivesRemovedMessages(t *testing.T) {
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 100, 10))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetTranscript(transcript)
	var archived CompactionArchive
	a.SetCompactionArchiver(func(ctx context.Context, archive CompactionArchive) (string, error) {
		archived = archive
		return "compactions/0001.input.json", nil
	})

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(archived.Messages) != 5 {
		t.Fatalf("archived %d messages, want two turns plus the next turn input", len(archived.Messages))
	}
	if archived.Summary != "S" {
		t.Fatalf("archived summary %q, want S", archived.Summary)
	}
	if archived.SummarySource != compactionSummarySourceModel || archived.FallbackReason != "" {
		t.Fatalf("archived provenance = %q/%q, want model", archived.SummarySource, archived.FallbackReason)
	}
	if !strings.Contains(a.Transcript()[0].Content[0].Text, "Raw compacted transcript archive: compactions/0001.input.json") {
		t.Fatalf("active summary missing archive reference: %q", a.Transcript()[0].Content[0].Text)
	}
}

func TestMaybeCompactReturnsUsageForTotals(t *testing.T) {
	// The summary call's tokens are returned so the caller can fold them into the
	// session totals (design §12, §6).
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 5000, 100))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	window := a.window()
	above := window * 80 / 100

	u, _, err := a.MaybeCompact(context.Background(), above, &recordSink{})
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if u.InputTokens != 5000 || u.OutputTokens != 100 {
		t.Errorf("returned compaction usage = %+v, want 5000 in / 100 out", u)
	}
}

func TestMaybeCompactBelowThresholdReturnsZeroUsage(t *testing.T) {
	transcript := makeTurns(4)
	fp := llmtest.New("fake")
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	below := a.window() / 2
	u, _, err := a.MaybeCompact(context.Background(), below, &recordSink{})
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if u != (llm.Usage{}) {
		t.Errorf("no compaction below threshold should return zero usage, got %+v", u)
	}
}

// TestContextWindowOverrideMovesTrigger is the regression test for the
// -context-window override never reaching compaction (design §6, §12). An
// unknown local model whose real window is far below the 256k registry default,
// run with a small override, must compact at 78% of the OVERRIDE, not 78% of
// 256k. Before the fix MaybeCompact read the registry default and
// never fired here, wedging the context.
func TestContextWindowOverrideMovesTrigger(t *testing.T) {
	const overrideWindow = 8000
	transcript := makeTurns(10)
	fp := llmtest.New("fake", summaryStep("S", 50, 10))
	a := newAgent(fp, tools.Default(), Options{
		Model:         "local-tiny-8k", // unknown model: registry default would be 256k
		ContextWindow: overrideWindow,
	})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	// 80% of the 8k override is ≥ 78% of the override but a tiny fraction of the
	// 256k registry default, so it only triggers when the override is honored.
	above := overrideWindow * 80 / 100
	if above*100 >= llm.NewRegistry(nil).ContextWindow("local-tiny-8k")*compactThresholdPct {
		t.Fatalf("test setup: %d should be below the default trigger", above)
	}

	if _, _, err := a.MaybeCompact(context.Background(), above, &recordSink{}); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("override window should have triggered compaction (1 summary call), got %d", len(fp.Requests))
	}
	if got := len(a.Transcript()); got != 16 {
		t.Fatalf("override-triggered compaction should collapse to checkpoint + 8 turns, got %d", got)
	}
}

// TestContextWindowOverrideMovesDegradeBudget pins the degradation budget to the
// override too: the same -context-window value that sizes the trigger must size
// the ladder. With a small override the ladder drops to the last turn and
// hard-truncates the big tool result; with the 256k default the same transcript
// sails under budget and is left fully intact. Comparing the two proves degrade
// reads the override, not the registry default (design §12 "never wedge").
func TestContextWindowOverrideMovesDegradeBudget(t *testing.T) {
	big := strings.Repeat("x", 20_000) // ~5000 estimated tokens in one result
	build := func() []llm.Message {
		var transcript []llm.Message
		for i := 0; i < 6; i++ {
			transcript = append(transcript,
				userText(turnLabel(i)+" q"),
				asstToolUse("t"+turnLabel(i), "read", `{}`),
				toolResult("t"+turnLabel(i), big),
				asstText(turnLabel(i)+" done"),
			)
		}
		return transcript
	}

	// With the override the ladder must shrink past rung 1 (keep last 8 turns) all
	// the way to a single truncated turn.
	const overrideWindow = 4000
	ov := newAgent(llmtest.New("fake", summaryStep("S", 50, 10)), tools.Default(), Options{
		Model:         "local-tiny",
		ContextWindow: overrideWindow,
	})
	ov.SetSystem("sys")
	ov.SetTranscript(build())
	if _, err := ov.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact (override): %v", err)
	}

	// Same transcript, no override: the 256k default budget leaves all 4 kept
	// turns verbatim (summary + 16 messages), no truncation.
	def := newAgent(llmtest.New("fake", summaryStep("S", 50, 10)), tools.Default(), Options{
		Model: "local-tiny",
	})
	def.SetSystem("sys")
	def.SetTranscript(build())
	if _, err := def.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact (default): %v", err)
	}

	ovEst := estimateTokens(ov.Transcript())
	defEst := estimateTokens(def.Transcript())
	if ovEst >= defEst {
		t.Fatalf("override budget should shrink further than the default: override est %d, default est %d", ovEst, defEst)
	}
	// Sanity: the override result must be near its own budget, the default must
	// keep all four turns verbatim (no truncation under 256k).
	if len(def.Transcript()) != 16 {
		t.Fatalf("default budget should keep the last 8 turns, got %d", len(def.Transcript()))
	}
	if budget := overrideWindow * compactThresholdPct / 100; ovEst > budget+minTruncResult/bytesPerToken {
		t.Fatalf("override degrade left estimate %d well above budget %d", ovEst, budget)
	}
}

// A single in-progress turn whose read-only tool result has ballooned over
// budget has nothing older than the kept turns to summarize, but it must still
// be summarized and archived rather than wedged against the context window.
func TestCompactSummarizesOversizedEarlierTurn(t *testing.T) {
	const window = 10_000 // budget = 7800 tokens ≈ 31.2k bytes
	huge := strings.Repeat("x", 200_000)
	transcript := []llm.Message{
		userText("inspect the file"),
		asstToolUse("c1", "read", `{"path":"a.go"}`),
		toolResult("c1", huge),
		asstText("looked"),
	}
	fp := llmtest.New("fake", summaryStep("LOOKED SUMMARY", 20, 4))
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: window})
	a.SetSystem("sys")
	a.SetTranscript(transcript)

	if estimateTokens(a.Transcript()) <= a.compactBudget() {
		t.Fatal("test setup: transcript should start over budget")
	}
	if len(turnStarts(a.Transcript())) > a.keepTurns() {
		t.Fatal("test setup: transcript should have <= keepTurns turns")
	}

	above := window * 80 / 100
	sink := &recordSink{}
	usage, changed, err := a.MaybeCompact(context.Background(), above, sink)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if !changed {
		t.Fatal("an oversized single-turn transcript should be shrunk (changed=true), not a silent no-op")
	}
	if usage.InputTokens != 20 || usage.OutputTokens != 4 {
		t.Errorf("summary usage = %+v", usage)
	}
	if len(fp.Requests) != 1 {
		t.Errorf("expected one summary call, got %d requests", len(fp.Requests))
	}
	if est := estimateTokens(a.Transcript()); est > a.compactBudget() {
		t.Errorf("shrunk transcript still over budget: est %d > budget %d", est, a.compactBudget())
	}
	mustValid(t, a.Transcript())
	var noticed bool
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted") {
			noticed = true
		}
	}
	if !noticed {
		t.Errorf("degrade-only compaction should emit a visible notice, got %v", sink.notices)
	}
}

func TestCompactUnderKeepTurnsSummarizesOlderTurnsWhenOverBudget(t *testing.T) {
	const window = 1000
	currentResult := strings.Repeat("x", 8000)
	transcript := []llm.Message{
		userText("older question"),
		asstText("older answer"),
		userText("current question"),
		asstToolUse("c1", "read", `{"path":"a.go"}`),
		toolResult("c1", currentResult),
	}
	fp := llmtest.New("fake", summaryStep("OLDER SUMMARY", 40, 8))
	a := newAgent(fp, tools.Default(), Options{Model: "local", ContextWindow: window})
	a.SetTranscript(transcript)
	if len(turnStarts(a.Transcript())) >= a.keepTurns() {
		t.Fatal("test setup should have fewer turns than the normal keep window")
	}

	sink := &recordSink{}
	usage, changed, err := a.MaybeCompact(context.Background(), window*80/100, sink)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if !changed {
		t.Fatal("over-budget transcript with an older turn should summarize it")
	}
	if usage.InputTokens != 40 || usage.OutputTokens != 8 {
		t.Fatalf("summary usage = %+v, want 40 in / 8 out", usage)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("summary requests = %d, want 1", len(fp.Requests))
	}
	if len(fp.Requests[0].Messages) != 4 {
		t.Fatalf("summary input messages = %d, want metadata plus older turn and current input", len(fp.Requests[0].Messages))
	}
	got := a.Transcript()
	mustValid(t, got)
	if len(got) != 3 {
		t.Fatalf("compacted transcript len = %d, want checkpoint + current tool turn", len(got))
	}
	if !strings.HasPrefix(got[0].Content[0].Text, checkpointHeader) || !strings.Contains(got[0].Content[0].Text, "OLDER SUMMARY") {
		t.Fatalf("first message is not the compaction checkpoint: %#v", got[0].Content[0])
	}
	if !strings.Contains(got[0].Content[0].Text, "current question") || got[1].Role != llm.RoleAssistant {
		t.Fatalf("current input/turn was not preserved by checkpoint: %#v", got)
	}
}

func TestCompactCurrentNoShrinkNoticeThrottled(t *testing.T) {
	const window = 1000
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "local", ContextWindow: window})
	a.SetTranscript([]llm.Message{
		userText("single turn"),
		asstText(strings.Repeat("x", 20_000)),
	})
	sink := &recordSink{}

	for i := 0; i < 3; i++ {
		_, changed, err := a.MaybeCompact(context.Background(), window*80/100, sink)
		if err != nil {
			t.Fatalf("MaybeCompact %d: %v", i, err)
		}
		if changed {
			t.Fatalf("MaybeCompact %d changed an unshrinkable transcript", i)
		}
	}
	if got := countNoticesContaining(sink.notices, "nothing left to shrink"); got != 1 {
		t.Fatalf("no-shrink notices = %d, want 1; notices=%v", got, sink.notices)
	}
}

func TestCompactCurrentTinyShrinkNoticeThrottled(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "local"})
	sink := &recordSink{}

	a.noticeCurrentShrink(sink, "auto", 10_000, 9_500)
	a.noticeCurrentShrink(sink, "auto", 11_000, 10_500)
	a.noticeCurrentShrink(sink, "auto", 20_000, 18_000)
	if got := countNoticesContaining(sink.notices, "archived oversized turn payload"); got != 2 {
		t.Fatalf("shrink notices = %d, want first tiny + material; notices=%v", got, sink.notices)
	}
}

// A transcript with <= keepTurns turns that already fits the budget is a genuine
// no-op: compact must report changed=false so the mid-loop caller does not churn
// its trigger state (reset lastInput/appendBoundary and re-estimate the whole
// transcript) every turn.
func TestCompactNoOpReportsUnchanged(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSystem("sys")
	a.SetTranscript(makeTurns(2)) // 2 turns <= keepTurns(8), tiny, well under budget

	before := len(a.Transcript())
	sink := &recordSink{}
	usage, changed, err := a.compact(context.Background(), sink, "auto")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if changed {
		t.Error("a no-op compaction must report changed=false")
	}
	if usage != (llm.Usage{}) {
		t.Errorf("no-op compaction usage = %+v, want zero", usage)
	}
	if len(a.Transcript()) != before {
		t.Errorf("no-op compaction must not rewrite the transcript: %d != %d", len(a.Transcript()), before)
	}
	if len(sink.notices) != 0 {
		t.Errorf("no-op compaction should emit no notice, got %v", sink.notices)
	}
}

func countNoticesContaining(notices []string, needle string) int {
	var count int
	for _, notice := range notices {
		if strings.Contains(notice, needle) {
			count++
		}
	}
	return count
}

// degrade/trim mutate on a deep copy, so when ValidateTranscript fails after the
// shrink, the live transcript is left fully intact — the documented rollback
// guarantee. The transcript is oversized but already INVALID (a trailing
// assistant tool_use with no result); the post-shrink validation must fail and
// the live transcript's huge tool input must be untouched.
func TestCompactValidationFailureLeavesTranscriptIntact(t *testing.T) {
	const window = 1000
	hugeInput := `{"path":"` + strings.Repeat("x", 200_000) + `"}`
	transcript := []llm.Message{
		userText("go"),
		asstToolUse("c1", "read", hugeInput), // no tool_result follows -> invalid
	}
	if err := llm.ValidateTranscript(transcript); err == nil {
		t.Fatal("test setup: transcript should be invalid (dangling tool_use)")
	}
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Model: "local", ContextWindow: window})
	a.SetSystem("sys")
	a.SetTranscript(transcript)
	if estimateTokens(a.Transcript()) <= a.compactBudget() {
		t.Fatal("test setup: transcript should start over budget")
	}

	origInputLen := len(a.Transcript()[1].Content[0].ToolInput)
	above := window * 80 / 100
	_, changed, err := a.MaybeCompact(context.Background(), above, &recordSink{})
	if err == nil {
		t.Fatal("compact should surface the post-shrink validation failure")
	}
	if changed {
		t.Error("a failed shrink must report changed=false")
	}
	if got := len(a.Transcript()[1].Content[0].ToolInput); got != origInputLen {
		t.Errorf("live transcript tool input mutated despite validation failure: len %d, want %d", got, origInputLen)
	}
}

func TestTruncateLargestBlockShrinksToolInput(t *testing.T) {
	largeInput := `{"payload":"` + strings.Repeat("x", 2000) + `"}`
	msgs := []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Kind:      llm.BlockToolUse,
			ToolUseID: "call_big",
			ToolName:  "shell",
			ToolInput: json.RawMessage(largeInput),
		}},
	}}
	before := len(msgs[0].Content[0].ToolInput)

	if !truncateLargestBlock(msgs, 1000) {
		t.Fatal("truncateLargestBlock returned false")
	}
	after := len(msgs[0].Content[0].ToolInput)
	if after >= before {
		t.Fatalf("tool input was not shrunk: before %d after %d", before, after)
	}
	if !strings.Contains(string(msgs[0].Content[0].ToolInput), "_truncated") {
		t.Fatalf("tool input missing truncation marker: %s", msgs[0].Content[0].ToolInput)
	}
}

func TestTruncateLargestBlockReplacesImage(t *testing.T) {
	msgs := []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageName:      "screenshot.png",
			ImageMediaType: "image/png",
			ImageData:      strings.Repeat("x", 2000),
		}},
	}}

	if !truncateLargestBlock(msgs, 1000) {
		t.Fatal("truncateLargestBlock returned false")
	}
	block := msgs[0].Content[0]
	if block.Kind != llm.BlockText || !strings.Contains(block.Text, "image omitted from compaction summary") {
		t.Fatalf("image was not replaced with text placeholder: %+v", block)
	}
}

func TestPrepareSummaryMessagesDegradesRichResultImagesOnDeepCopy(t *testing.T) {
	const imageData = "c2Vuc2l0aXZlLWltYWdlLWRhdGE="
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind:        llm.BlockToolResult,
		ResultForID: "call",
		ResultText:  "attached",
		ResultContent: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageName:      "capture.png",
			ImageMediaType: "image/png",
			ImageData:      imageData,
			ImageDetail:    "high",
			ImageWidth:     320,
			ImageHeight:    200,
		}},
	}}}}

	got := prepareSummaryMessages(msgs, 8)
	result := got[0].Content[0]
	if len(result.ResultContent) != 0 {
		t.Fatalf("summary input retained nested image: %+v", result.ResultContent)
	}
	for _, want := range []string{"image omitted", "capture.png", "image/png", "detail high", "320x200"} {
		if !strings.Contains(result.ResultText, want) {
			t.Errorf("summary placeholder missing %q: %q", want, result.ResultText)
		}
	}
	if strings.Contains(result.ResultText, imageData) {
		t.Fatalf("summary placeholder leaked base64: %q", result.ResultText)
	}
	if len(msgs[0].Content[0].ResultContent) != 1 || msgs[0].Content[0].ResultContent[0].ImageData != imageData {
		t.Fatalf("summary preparation mutated source: %+v", msgs[0].Content[0])
	}
}

func TestPrepareSummaryMessagesOmitsHostedToolSearchState(t *testing.T) {
	responsesRaw := json.RawMessage(`{"type":"tool_search_output","execution":"server","status":"completed","tools":[]}`)
	anthropicRaw := json.RawMessage(`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`)
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Kind: llm.BlockResponsesToolSearch, ResponsesToolSearch: responsesRaw},
			{Kind: llm.BlockAnthropicToolSearch, AnthropicToolSearch: anthropicRaw},
			{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "mcp__demo__search", ToolNamespace: "mcp_demo", ToolInput: json.RawMessage(`{}`)},
			{Kind: llm.BlockText, Text: "kept"},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Kind: llm.BlockResponsesToolSearch, ResponsesToolSearch: responsesRaw},
			{Kind: llm.BlockAnthropicToolSearch, AnthropicToolSearch: anthropicRaw},
		}},
	}

	got := prepareSummaryMessages(msgs, 4096)
	if len(got) != 1 || len(got[0].Content) != 2 || got[0].Content[0].Kind != llm.BlockToolUse ||
		got[0].Content[0].ToolNamespace != "" || got[0].Content[1].Kind != llm.BlockText || got[0].Content[1].Text != "kept" {
		t.Fatalf("summary messages = %+v, want ordinary function call plus model-visible text", got)
	}
	if len(msgs[0].Content) != 4 || !bytes.Equal(msgs[0].Content[0].ResponsesToolSearch, responsesRaw) ||
		!bytes.Equal(msgs[0].Content[1].AnthropicToolSearch, anthropicRaw) || msgs[0].Content[2].ToolNamespace != "mcp_demo" {
		t.Fatalf("summary preparation mutated source: %+v", msgs)
	}
}

func TestPrepareSummaryMessagesDisabledPreviewKeepsRichResultImages(t *testing.T) {
	for _, maxBytes := range []int{0, -1} {
		msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Kind: llm.BlockToolResult, ResultForID: "call", ResultText: "attached",
			ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: "image-data"}},
		}}}}
		got := prepareSummaryMessages(msgs, maxBytes)
		if content := got[0].Content[0].ResultContent; len(content) != 1 || content[0].ImageData != "image-data" {
			t.Fatalf("maxBytes=%d result content = %+v", maxBytes, content)
		}
	}
}

func TestPrepareSummaryMessagesElidesSemanticallyUselessPairPayload(t *testing.T) {
	input := json.RawMessage(`{"query":"large but empty search"}`)
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Kind: llm.BlockToolUse, ToolUseID: "call-empty", ToolName: "search", ToolInput: input,
		}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Kind: llm.BlockToolResult, ResultForID: "call-empty", ToolName: "search",
			ResultText: "no matches", ResultUseless: true,
		}}},
	}

	got := prepareSummaryMessages(msgs, 4096)
	if strings.Contains(string(got[0].Content[0].ToolInput), "large but empty search") {
		t.Fatalf("summary retained useless tool input: %s", got[0].Content[0].ToolInput)
	}
	if !strings.Contains(got[1].Content[0].ResultText, "semantically empty") {
		t.Fatalf("summary result = %q", got[1].Content[0].ResultText)
	}
	if string(msgs[0].Content[0].ToolInput) != string(input) || msgs[1].Content[0].ResultText != "no matches" {
		t.Fatalf("summary preparation mutated source: %+v", msgs)
	}
}

func TestTruncateLargestBlockReplacesRichResultImage(t *testing.T) {
	const imageData = "c2Vuc2l0aXZlLWltYWdlLWRhdGE="
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind:        llm.BlockToolResult,
		ResultForID: "call",
		ResultText:  "attached",
		ResultContent: []llm.ContentBlock{{
			Kind:           llm.BlockImage,
			ImageMediaType: "image/jpeg",
			ImageData:      imageData,
			ImageWidth:     800,
			ImageHeight:    600,
		}},
	}}}}

	if !truncateLargestBlock(msgs, 1000) {
		t.Fatal("truncateLargestBlock returned false")
	}
	result := msgs[0].Content[0]
	if len(result.ResultContent) != 0 || !strings.Contains(result.ResultText, "800x600") {
		t.Fatalf("nested image was not replaced by a parent description: %+v", result)
	}
	if strings.Contains(result.ResultText, imageData) {
		t.Fatalf("degraded result leaked base64: %q", result.ResultText)
	}
}

func TestCloneMessagesDeepCopiesRichResultContent(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind:          llm.BlockToolResult,
		ResultForID:   "call",
		ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: "original"}},
	}}}}
	cloned := cloneMessages(msgs)
	cloned[0].Content[0].ResultContent[0].ImageData = "changed"
	cloned[0].Content[0].ResultContent = append(cloned[0].Content[0].ResultContent, llm.ContentBlock{Kind: llm.BlockImage})

	if got := msgs[0].Content[0].ResultContent; len(got) != 1 || got[0].ImageData != "original" {
		t.Fatalf("clone shared nested result content with source: %+v", got)
	}
}

func TestAgentEstimatesCountRichResultImagesAtFlatWeight(t *testing.T) {
	plain := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call", ResultText: "attached"}}}}
	withImage := cloneMessages(plain)
	withImage[0].Content[0].ResultContent = []llm.ContentBlock{{
		Kind:           llm.BlockImage,
		ImageMediaType: "image/png",
		ImageDetail:    "high",
		ImageName:      "capture.png",
		ImageData:      "YWJj",
	}}
	withHugeImage := cloneMessages(withImage)
	withHugeImage[0].Content[0].ResultContent[0].ImageData = strings.Repeat("A", 1<<20)

	if delta := estimateTokens(withImage) - estimateTokens(plain); delta < imageTokenEstimate {
		t.Fatalf("nested image token delta = %d, want at least flat weight %d", delta, imageTokenEstimate)
	}
	if small, huge := estimateTokens(withImage), estimateTokens(withHugeImage); small != huge {
		t.Fatalf("base64 length changed flat image estimate: small=%d huge=%d", small, huge)
	}
	requestSmall := estimateRequest(llm.Request{Messages: withImage}, 10_000).Messages
	requestHuge := estimateRequest(llm.Request{Messages: withHugeImage}, 10_000).Messages
	requestPlain := estimateRequest(llm.Request{Messages: plain}, 10_000).Messages
	if requestSmall-requestPlain < imageTokenEstimate || requestSmall != requestHuge {
		t.Fatalf("request estimates plain=%d small=%d huge=%d", requestPlain, requestSmall, requestHuge)
	}
}

func TestContextEstimatesIncludePlaintextThinking(t *testing.T) {
	thinking := strings.Repeat("deliberation", 400)
	msgs := []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Kind:              llm.BlockThinking,
			Thinking:          thinking,
			ThinkingSignature: "signature",
		}},
	}}

	wantMinimum := len(thinking) / bytesPerToken
	if got := estimateTokens(msgs); got < wantMinimum {
		t.Fatalf("transcript estimate = %d, want at least %d plaintext-thinking tokens", got, wantMinimum)
	}
	if got := estimateRequest(llm.Request{Messages: msgs}, 100_000); got.Messages < wantMinimum {
		t.Fatalf("request estimate = %d message tokens, want at least %d", got.Messages, wantMinimum)
	}
}

func TestSummarizeUsageSurvivesZeroedDoneFrame(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 55, OutputTokens: 5}},
			textDelta("summary"),
		},
		Stop: llm.StopEndTurn,
	})
	a := newAgent(fp, tools.Default(), Options{})
	_, usage, err := a.summarize(context.Background(), prompts.CompactionSummary(), []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "old"}}},
	}, llm.RequestPurposeCompaction)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if usage.InputTokens != 55 || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want 55 in / 5 out preserved", usage)
	}
}

func dumpShort(msgs []llm.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		b.WriteString(string(rune('0'+i%10)) + ":" + string(m.Role) + " ")
	}
	return b.String()
}

// seedTurns returns n complete small turns so compaction has history to fold.
// The 1-byte "q"/"a" bodies are deliberate: TestProactiveCompactionMidTurn does
// byte-estimate threshold math against them.
func seedTurns(n int) []llm.Message {
	var msgs []llm.Message
	for range n {
		msgs = append(msgs, userText("q"), asstText("a"))
	}
	return msgs
}

func TestProactiveCompactionMidTurn(t *testing.T) {
	// Window 1000 tokens -> trigger at 780 tokens (3120 bytes estimated).
	// The tool result is 8000 bytes, so the estimate crosses the threshold
	// before step 2's request is built.
	big := strings.Repeat("x", 8000)
	tool := &recordTool{name: "blob", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return big, nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{ // step 1: ask for the ballooning tool
			Events: []llm.StreamEvent{toolDone(0, "c1", "blob", `{}`)},
			Stop:   llm.StopToolUse,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 2},
		},
		llmtest.Step{ // the mid-prompt maintenance summary call
			Events: []llm.StreamEvent{textDelta("the summary")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 50, OutputTokens: 5},
		},
		llmtest.Step{ // step 2 proper, against the compacted transcript
			Events: []llm.StreamEvent{textDelta("done")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 20, OutputTokens: 3},
		},
	)
	a := newAgent(fp, reg, Options{ContextWindow: 1000})
	a.SetTranscript(seedTurns(5))
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())

	if len(fp.Requests) != 3 {
		t.Fatalf("provider called %d times, want 3 (step, summary, step)", len(fp.Requests))
	}
	// The post-compaction request starts with the user checkpoint.
	first := fp.Requests[2].Messages[0]
	if first.Role != llm.RoleUser {
		t.Fatalf("post-compaction checkpoint role = %q, want user", first.Role)
	}
	if !strings.HasPrefix(first.Content[0].Text, checkpointHeader) {
		t.Errorf("post-compaction request should start with the checkpoint, got %q", first.Content[0].Text)
	}
	var compacted bool
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted:") {
			compacted = true
		}
	}
	if !compacted {
		t.Errorf("no compaction notice, notices=%v", sink.notices)
	}
	// Summary-call usage folds into the prompt total (10+50+20 inputs).
	if got := sink.promptUsage[0].Usage.InputTokens; got != 80 {
		t.Errorf("prompt input tokens = %d, want 80", got)
	}
	if got := sink.promptUsage[0].Compactions; got != 1 {
		t.Errorf("prompt compactions = %d, want 1", got)
	}
}

func TestNoMidTurnCompactionUnderThreshold(t *testing.T) {
	tool := &recordTool{name: "small", run: func(_ context.Context, _ json.RawMessage) (string, error) {
		return "tiny", nil
	}}
	reg := &tools.Registry{}
	reg.Register(tool)

	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "c1", "small", `{}`)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, reg, Options{ContextWindow: 1_000_000})
	a.SetTranscript(seedTurns(5))
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Errorf("provider called %d times, want 2 (no summary call)", len(fp.Requests))
	}
	if got := sink.promptUsage[0].Compactions; got != 0 {
		t.Errorf("prompt compactions = %d, want 0", got)
	}
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted:") {
			t.Errorf("unexpected compaction: %v", sink.notices)
		}
	}
}

func TestPostTurnCompactionUsesFreshTranscriptEstimate(t *testing.T) {
	// Window 1000 tokens -> trigger at 780 tokens. The final assistant message is
	// large enough to cross the threshold even though the provider-reported input
	// count for that turn is tiny.
	final := strings.Repeat("z", 8000)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta(final)},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 10, OutputTokens: 2},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{textDelta("summary after final")},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 50, OutputTokens: 5},
		},
	)
	a := newAgent(fp, tools.Default(), Options{ContextWindow: 1000})
	a.SetTranscript(seedTurns(5))
	sink := &recordSink{}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(fp.Requests) != 2 {
		t.Fatalf("provider called %d times, want 2 (final response + summary)", len(fp.Requests))
	}
	var compacted bool
	for _, n := range sink.notices {
		if strings.Contains(n, "compacted:") {
			compacted = true
		}
	}
	if !compacted {
		t.Fatalf("post-turn compaction did not run, notices=%v", sink.notices)
	}
	if got := sink.promptUsage[0].Usage.InputTokens; got != 60 {
		t.Errorf("prompt input tokens = %d, want 60 including summary call", got)
	}
}

func opaqueMaintenanceTranscript(turns int) []llm.Message {
	messages := makeTurns(turns)
	messages[1].Content = append([]llm.ContentBlock{
		{
			Kind:                  llm.BlockReasoning,
			ReasoningReplayDomain: "source-domain",
			ReasoningID:           "rs_source",
			ReasoningEncrypted:    "SOURCE-ONLY-OPAQUE",
		},
	}, messages[1].Content...)
	return messages
}

func TestCompactionMaintenanceFiltersMismatchedOpaqueReasoning(t *testing.T) {
	tests := []struct {
		name      string
		run       func(context.Context, *Agent, []llm.Message) ([]llm.Message, error)
		exactLive bool
	}{
		{
			name: "Compact",
			run: func(ctx context.Context, a *Agent, _ []llm.Message) ([]llm.Message, error) {
				var archived CompactionArchive
				a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
					archived = archive
					return "archive", nil
				})
				_, err := a.Compact(ctx, &recordSink{})
				return archived.Messages, err
			},
		},
		{
			name: "CompactForContinuation",
			run: func(ctx context.Context, a *Agent, _ []llm.Message) ([]llm.Message, error) {
				var archived CompactionArchive
				a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
					archived = archive
					return "archive", nil
				})
				_, _, err := a.CompactForContinuation(ctx, &recordSink{})
				return archived.Messages, err
			},
			exactLive: true,
		},
		{
			name: "GenerateBranchSummary",
			run: func(ctx context.Context, a *Agent, source []llm.Message) ([]llm.Message, error) {
				_, _, err := a.GenerateBranchSummary(ctx, source, "")
				return source, err
			},
			exactLive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := llmtest.New("fake", summaryStep("summary", 10, 2))
			a := newAgent(fp, tools.Default(), Options{
				Model:                 "target-model",
				ReasoningReplayDomain: "target-domain",
			})
			source := opaqueMaintenanceTranscript(10)
			before := cloneMessages(source)
			a.SetTranscript(source)

			durable, err := tt.run(context.Background(), a, source)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if len(fp.Requests) != 1 {
				t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
			}
			if requestHasReasoningEncrypted(fp.Requests[0], "SOURCE-ONLY-OPAQUE") {
				t.Fatalf("mismatched reasoning reached maintenance request:\n%s", dump(fp.Requests[0].Messages))
			}
			if !strings.Contains(dump(fp.Requests[0].Messages), "A answer") {
				t.Fatalf("ordinary durable input was filtered from request:\n%s", dump(fp.Requests[0].Messages))
			}
			if !requestHasReasoningEncrypted(llm.Request{Messages: durable}, "SOURCE-ONLY-OPAQUE") {
				t.Fatalf("request filtering removed reasoning from durable input:\n%s", dump(durable))
			}
			if tt.exactLive && !reflect.DeepEqual(durable, before) {
				t.Fatalf("maintenance operation mutated durable input:\n%s", dump(durable))
			}
		})
	}
}

func TestPrepareIdleCompactionSnapshotsReasoningReplayPolicy(t *testing.T) {
	for _, tt := range []struct {
		name             string
		disableBefore    bool
		wantSameDomainIn bool
	}{
		{name: "same domain retained", wantSameDomainIn: true},
		{name: "copied disabled map removes same domain", disableBefore: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const domain = "target-domain"
			fp := llmtest.New("fake", summaryStep("idle summary", 10, 2))
			a := newAgent(fp, tools.Default(), Options{
				Model:                 "target-model",
				ContextWindow:         10_000,
				ReasoningReplayDomain: domain,
			})
			transcript := opaqueMaintenanceTranscript(10)
			transcript[1].Content = append([]llm.ContentBlock{{
				Kind:                  llm.BlockReasoning,
				ReasoningReplayDomain: domain,
				ReasoningID:           "rs_target",
				ReasoningEncrypted:    "TARGET-DOMAIN-OPAQUE",
			}}, transcript[1].Content...)
			a.SetTranscript(transcript)
			if tt.disableBefore {
				a.disableCurrentReasoningReplay()
			}

			work, ok, err := a.PrepareIdleCompaction(1)
			if err != nil || !ok {
				t.Fatalf("PrepareIdleCompaction = ok %t err %v, want work", ok, err)
			}
			if tt.disableBefore {
				delete(a.disabledReasoningReplay, domain)
			}
			result, err := work(context.Background())
			if err != nil || !result.Prepared {
				t.Fatalf("idle work = prepared %t err %v", result.Prepared, err)
			}
			if len(fp.Requests) != 1 {
				t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
			}
			request := fp.Requests[0]
			if requestHasReasoningEncrypted(request, "SOURCE-ONLY-OPAQUE") {
				t.Fatalf("mismatched reasoning reached idle request:\n%s", dump(request.Messages))
			}
			if got := requestHasReasoningEncrypted(request, "TARGET-DOMAIN-OPAQUE"); got != tt.wantSameDomainIn {
				t.Fatalf("same-domain reasoning present = %t, want %t:\n%s", got, tt.wantSameDomainIn, dump(request.Messages))
			}
			live := llm.Request{Messages: a.Transcript()}
			if !requestHasReasoningEncrypted(live, "SOURCE-ONLY-OPAQUE") ||
				!requestHasReasoningEncrypted(live, "TARGET-DOMAIN-OPAQUE") {
				t.Fatalf("idle preparation mutated live transcript:\n%s", dump(a.Transcript()))
			}
		})
	}
}

func TestGenerateBranchSummaryInvalidEncryptedContentFallback(t *testing.T) {
	invalidEncrypted := func(in, out int) llmtest.Step {
		return llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: in, OutputTokens: out}}},
			Err: &llm.APIError{
				StatusCode: 400,
				Code:       "invalid_encrypted_content",
				Message:    "encrypted content could not be verified",
			},
		}
	}
	for _, tt := range []struct {
		name        string
		second      llmtest.Step
		wantSummary string
		wantErr     bool
	}{
		{name: "fallback succeeds", second: summaryStep("recovered", 7, 3), wantSummary: "recovered"},
		{name: "fallback failure is not retried", second: invalidEncrypted(7, 3), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fp := llmtest.New("fake", invalidEncrypted(11, 2), tt.second)
			a := newAgent(fp, tools.Default(), Options{
				Model:                 "target-model",
				ReasoningReplayDomain: "source-domain",
			})
			a.SetTranscript(opaqueMaintenanceTranscript(2))
			before := cloneMessages(a.Transcript())

			summary, usage, err := a.GenerateBranchSummary(context.Background(), opaqueMaintenanceTranscript(2), "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("GenerateBranchSummary error = %v, wantErr %t", err, tt.wantErr)
			}
			if summary != tt.wantSummary {
				t.Fatalf("summary = %q, want %q", summary, tt.wantSummary)
			}
			if usage.InputTokens != 18 || usage.OutputTokens != 5 {
				t.Fatalf("usage = %+v, want summed 18/5", usage)
			}
			if len(fp.Requests) != 2 {
				t.Fatalf("provider requests = %d, want exactly 2", len(fp.Requests))
			}
			if !requestHasReasoningEncrypted(fp.Requests[0], "SOURCE-ONLY-OPAQUE") {
				t.Fatalf("first request did not exercise encrypted replay:\n%s", dump(fp.Requests[0].Messages))
			}
			if requestHasReasoningEncrypted(fp.Requests[1], "SOURCE-ONLY-OPAQUE") {
				t.Fatalf("fallback request retained opaque reasoning:\n%s", dump(fp.Requests[1].Messages))
			}
			if !reflect.DeepEqual(a.Transcript(), before) {
				t.Fatalf("fallback mutated durable transcript:\n%s", dump(a.Transcript()))
			}
		})
	}
}

func TestGenerateBranchSummaryFiltersMismatchedOpaqueBeforeChunking(t *testing.T) {
	const opaque = "LARGE-MISMATCH-"
	fp := llmtest.New("fake", summaryStep("one pass", 10, 2))
	a := newAgent(fp, tools.Default(), Options{
		Model:                 "target-model",
		ContextWindow:         4_000,
		ReasoningReplayDomain: "target-domain",
	})
	transcript := opaqueMaintenanceTranscript(2)
	transcript[1].Content[0].ReasoningEncrypted = opaque + strings.Repeat("x", 80_000)
	a.SetTranscript(transcript)
	before := cloneMessages(a.Transcript())

	summary, _, err := a.GenerateBranchSummary(context.Background(), transcript, "")
	if err != nil {
		t.Fatalf("GenerateBranchSummary: %v", err)
	}
	if summary != "one pass" || len(fp.Requests) != 1 {
		t.Fatalf("filtered summary = %q with %d requests, want one unchunked pass", summary, len(fp.Requests))
	}
	if strings.Contains(dump(fp.Requests[0].Messages), opaque) {
		t.Fatalf("large mismatched reasoning reached request:\n%s", dump(fp.Requests[0].Messages))
	}
	if !reflect.DeepEqual(a.Transcript(), before) {
		t.Fatal("pre-chunk filtering mutated durable transcript")
	}
}

// TestEstimateTokensWeightsOpaqueFieldsSeparately pins the WI-2 estimator
// split: provider-opaque payloads (thinking signatures, encrypted/redacted
// reasoning, interaction steps) tokenize far coarser than prose, so they use
// opaqueBytesPerToken while text (including thinking text) uses bytesPerToken.
func TestEstimateTokensWeightsOpaqueFieldsSeparately(t *testing.T) {
	thinking := strings.Repeat("t", 400)
	signature := strings.Repeat("s", 4340) // session-observed Kimi signature size
	msgs := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Kind: llm.BlockThinking, Thinking: thinking, ThinkingSignature: signature},
	}}}
	got := estimateTokens(msgs)
	want := len(thinking)/bytesPerToken + len(signature)/opaqueBytesPerToken
	if got != want {
		t.Fatalf("estimateTokens = %d, want %d (text %d bytes / %d + signature %d bytes / %d)",
			got, want, len(thinking), bytesPerToken, len(signature), opaqueBytesPerToken)
	}

	// The request-side estimator agrees (its block also counts the kind tag and
	// the message role).
	req := estimateRequest(llm.Request{Messages: msgs}, 0)
	wantReq := (len(string(llm.RoleAssistant))+len(string(llm.BlockThinking))+len(thinking))/bytesPerToken + len(signature)/opaqueBytesPerToken
	if req.Messages != wantReq {
		t.Fatalf("estimateRequest messages = %d, want %d", req.Messages, wantReq)
	}
}

// TestCompactionReadFilesCapBoundsCheckpointIndex pins P5: the recognized
// read-file index is capped, modified files are never capped, and the checkpoint
// JSON reports how many reads were omitted.
func TestCompactionReadFilesCapBoundsCheckpointIndex(t *testing.T) {
	reg := tools.Default()
	var msgs []llm.Message
	// 300 read turns plus 5 modified paths, all successful.
	for i := 0; i < compactionReadFilesCap+100; i++ {
		id := fmt.Sprintf("r%d", i)
		readInput := fmt.Sprintf(`{"path":"read_%03d.go"}`, i)
		msgs = append(msgs,
			asstToolUse(id, "read", readInput),
			toolResult(id, fmt.Sprintf("read %d", i)),
		)
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("w%d", i)
		writeInput := fmt.Sprintf(`{"path":"mod_%d.go","content":"x"}`, i)
		msgs = append(msgs,
			asstToolUse(id, "write", writeInput),
			toolResult(id, fmt.Sprintf("wrote %d", i)),
		)
	}
	a := newAgent(llmtest.New("fake", summaryStep("S", 10, 1)), reg, Options{Model: "claude-opus-4-8"})
	reads, modified, omitted := a.compactionFileActivity(msgs, nil)
	if len(reads) != compactionReadFilesCap {
		t.Fatalf("capped read files = %d, want %d", len(reads), compactionReadFilesCap)
	}
	// The oldest first-touches are dropped.
	if containsPath(reads, "read_000.go") {
		t.Fatalf("oldest read kept: %v", reads[:3])
	}
	if !containsPath(reads, fmt.Sprintf("read_%03d.go", compactionReadFilesCap+99)) {
		t.Fatalf("newest read dropped")
	}
	if len(modified) != 5 {
		t.Fatalf("modified files capped: %v", modified)
	}
	if omitted != 100 {
		t.Fatalf("read files omitted = %d, want 100", omitted)
	}

	// The activity collector passes the omission count through to the checkpoint.
	checkpoint := a.checkpointMessage("S", msgs, "", "model", "", "", reads, omitted, modified)
	text := checkpoint.Content[0].Text
	if !strings.Contains(text, `"read_files_omitted":100`) {
		t.Fatalf("checkpoint missing read_files_omitted: %.200s", text[len(text)-400:])
	}
	if strings.Contains(text, "read_000.go") {
		t.Fatalf("checkpoint JSON includes a dropped read path")
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// TestCompactionPriorMetadataCarryForwardIsCapped verifies a prior checkpoint's
// read list is capped too, and modified paths always win over reads.
func TestCompactionPriorMetadataCarryForwardIsCapped(t *testing.T) {
	reg := tools.Default()
	prior := &llm.CompactionMetadata{ReadFilesOmitted: 7}
	for i := 0; i < compactionReadFilesCap+50; i++ {
		prior.ReadFiles = append(prior.ReadFiles, fmt.Sprintf("old_%03d.go", i))
	}
	prior.ModifiedFiles = []string{"kept.go"}
	a := newAgent(llmtest.New("fake"), reg, Options{Model: "claude-opus-4-8"})
	reads, modified, omitted := a.compactionFileActivity(nil, prior)
	if len(reads) != compactionReadFilesCap {
		t.Fatalf("prior carry-forward reads = %d, want %d", len(reads), compactionReadFilesCap)
	}
	if containsPath(reads, "old_000.go") {
		t.Fatalf("oldest prior read kept")
	}
	if !containsPath(modified, "kept.go") {
		t.Fatalf("prior modified path dropped: %v", modified)
	}
	if omitted != 57 {
		t.Fatalf("carried omission count = %d, want 57", omitted)
	}
}

// TestKeepTokensWindowAdaptive pins P6: an unset compact_keep_tokens scales with
// the window between 4k and 20k; an explicit value wins.
func TestKeepTokensWindowAdaptive(t *testing.T) {
	for _, tc := range []struct {
		name            string
		window, setWant int
		set             int
	}{
		{name: "32k window uses window/5", window: 32_000, setWant: 6_400},
		{name: "20k window stays at 4k", window: 20_000, setWant: adaptiveKeepTokensMin},
		{name: "200k window uses 40k clamped to 20k", window: 200_000, setWant: adaptiveKeepTokensMax},
		{name: "1M window caps at 20k", window: 1_000_000, setWant: adaptiveKeepTokensMax},
		{name: "100k window caps at 20k", window: 100_000, setWant: adaptiveKeepTokensMax},
		{name: "explicit wins", window: 1_000_000, set: 7_000, setWant: 7_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAgent(llmtest.New("fake"), &tools.Registry{}, Options{Model: "local", ContextWindow: tc.window, CompactKeepTokens: tc.set})
			if got := a.keepTokens(); got != tc.setWant {
				t.Fatalf("keepTokens() = %d, want %d", got, tc.setWant)
			}
		})
	}
}

// TestCompactionFixtureContextReduction is the fixture-level exit check: a
// synthetic transcript with repeated 20KB writes, full-mode shell results, and
// 300 read paths must show a measurably lower estimateContext after retention.
func TestCompactionFixtureContextReduction(t *testing.T) {
	reg := tools.Default()
	bigBody := strings.Repeat("x", 20_000)
	var msgs []llm.Message
	// Several superseded 20KB writes to one path.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("w%d", i)
		input := fmt.Sprintf(`{"path":"main.go","content":"%s"}`, bigBody)
		msgs = append(msgs, asstToolUse(id, "write", input), toolResult(id, "overwrote main.go"))
	}
	// A few full-mode shell results (non-read-only, large).
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("s%d", i)
		msgs = append(msgs, asstToolUse(id, "shell", `{"argv":["ls"]}`), toolResult(id, bigBody+bigBody))
	}
	// 300 read paths.
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("r%d", i)
		msgs = append(msgs, asstToolUse(id, "read", fmt.Sprintf(`{"path":"f_%03d.go"}`, i)), toolResult(id, "ok"))
	}
	// Filler turns to push everything above past the retention boundaries.
	for i := 0; i < 6; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("filler %d", i)), asstText("ok"))
	}
	// A recent successful write inside the kept suffix: it supersedes the three
	// old 20KB writes at the head of the transcript.
	msgs = append(msgs,
		asstToolUse("w_new", "write", `{"path":"main.go","content":"package main\n"}`),
		toolResult("w_new", "overwrote main.go"),
	)

	a := newAgent(llmtest.New("fake"), reg, Options{RetentionPolicy: RetentionPolicyAge, Model: "local"})
	a.SetTranscript(msgs)
	mustValid(t, a.Transcript())
	before := a.estimateContextForTranscript(nil, a.Transcript()).Total
	bytesBefore := retentionTranscriptBytes(a.Transcript())

	sink := &archiveSink{archive: ToolResultArchive{
		DisplayPath: "artifacts/tool-results/r.txt",
		ModelPath:   "/session/artifacts/tool-results/r.txt",
	}}
	if changed := a.applyRetention(sink); !changed {
		t.Fatal("retention pass made no changes on the fixture transcript")
	}
	mustValid(t, a.Transcript())
	after := a.estimateContextForTranscript(nil, a.Transcript()).Total
	bytesAfter := retentionTranscriptBytes(a.Transcript())
	if after >= before {
		t.Fatalf("context estimate did not fall: %d -> %d", before, after)
	}
	if removed := bytesBefore - bytesAfter; removed < 50_000 {
		t.Fatalf("retention reclaimed only %d bytes, want at least 50000", removed)
	}
	// Every write input to main.go but the newest must be a receipt.
	receipts, verbatim := 0, 0
	for _, m := range a.Transcript() {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolUse && b.ToolName == "write" {
				if strings.Contains(string(b.ToolInput), retentionInputMarker) {
					receipts++
				} else {
					verbatim++
				}
			}
		}
	}
	if receipts != 3 || verbatim != 1 {
		t.Fatalf("write inputs: %d receipts, %d verbatim; want 3 receipts and 1 verbatim", receipts, verbatim)
	}
}
