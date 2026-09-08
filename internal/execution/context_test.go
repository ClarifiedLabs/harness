package execution

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestComposeRequestCountsPayloadsWithoutSystemHistory(t *testing.T) {
	req := llm.Request{
		System: "sys", RequestContext: []string{"volatile"},
		Tools:              []llm.ToolSchema{{Name: "tool", Description: "desc", Parameters: json.RawMessage(`{"a":1}`)}},
		DeferredToolGroups: []llm.ToolGroup{{Name: "ns", Description: "group", Tools: []llm.ToolSchema{{Name: "name", Description: "hint", Parameters: json.RawMessage(`{}`)}}}},
		ServerTools:        []llm.ServerTool{{Name: "web", Kind: "kind", Parameters: json.RawMessage(`{}`)}},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{Kind: llm.BlockText, Text: "input"},
				{Kind: llm.BlockImage, ImageData: "abc", ImageEncodedBytes: 999},
			}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockText, Text: "answer"},
				{Kind: llm.BlockToolUse, ToolInput: json.RawMessage(`{"q":1}`)},
				{Kind: llm.BlockThinking, Thinking: "thought", ThinkingSignature: "signed"},
				{Kind: llm.BlockRedactedThinking, RedactedData: "opaque"},
				{Kind: llm.BlockReasoning, ReasoningID: "ignored-id", ReasoningEncrypted: "cipher"},
				{Kind: llm.BlockInteractionThought, InteractionThoughtSummary: "sum", InteractionThoughtSignature: "stamp"},
				{Kind: llm.BlockInteractionStep, InteractionStep: json.RawMessage(`{"a":1}`)},
				{Kind: llm.BlockResponsesToolSearch, ResponsesToolSearch: json.RawMessage(`{"bb":2}`)},
				{Kind: llm.BlockAnthropicToolSearch, AnthropicToolSearch: json.RawMessage(`{"ccc":3}`)},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{Kind: llm.BlockToolResult, ResultText: "result", ResultContent: []llm.ContentBlock{
					{Kind: llm.BlockImage, ImageData: "abcd"},
					{Kind: llm.BlockText, Text: "richtext"},
				}},
				{Kind: llm.BlockProviderCompaction, ProviderCompaction: []json.RawMessage{json.RawMessage("raw1"), json.RawMessage("raw22")}},
			}},
		},
	}
	want := ContextComposition{Messages: 3, Blocks: 15, SystemTextBytes: 11, UserTextBytes: 5, AssistantTextBytes: 6,
		ToolInputBytes: 7, ToolResultBytes: 14, ToolSchemaBytes: 41, ReasoningTextBytes: 10, ReasoningOpaqueBytes: 23, ProviderStateBytes: 33, ImageEncodedBytes: 7}
	if got := ComposeRequest(req); got != want {
		t.Fatalf("composition = %+v, want %+v", got, want)
	}
	// System/request-only instructions never masquerade as extra messages/blocks.
	if got := ComposeRequest(llm.Request{System: "system", RequestContext: []string{"only"}}); got != (ContextComposition{SystemTextBytes: 10}) {
		t.Fatalf("system became history: %+v", got)
	}
}

func TestComposeRequestIgnoresRoutingAndTranscriptMetadata(t *testing.T) {
	req := llm.Request{System: "system", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "text"}}}}}
	want := ComposeRequest(req)
	req.Model, req.ProxySessionID, req.CacheAffinityID, req.PreviousResponseID = "private-model", "private-proxy", "private-affinity", "private-response"
	req.Messages[0].SteerID = "private-steer"
	req.Messages[0].Compaction = &llm.CompactionMetadata{Summary: "not model content", ReadFiles: []string{"secret/path"}}
	req.Messages[0].ParallelToolBatches = []llm.ParallelToolBatch{{ToolUseIDs: []string{"private-call"}}}
	req.Messages[0].Content[0].ReasoningReplayDomain = "private-domain"
	if got := ComposeRequest(req); got != want {
		t.Fatalf("routing/transcript metadata affected payload counts: %+v != %+v", got, want)
	}
}

func TestComposeRequestImageMetadataAndNestedResults(t *testing.T) {
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Kind: llm.BlockImage, ImageEncodedBytes: 20, ImageBytes: 15},
		{Kind: llm.BlockImage, ImageEncodedBytes: -1, ImageBytes: 15},
		{Kind: llm.BlockToolResult, ResultContent: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageEncodedBytes: 7}}}}},
	}}}}
	if got := ComposeRequest(req); got != (ContextComposition{Messages: 1, Blocks: 5, ImageEncodedBytes: 27}) {
		t.Fatalf("image metadata/nested composition = %+v", got)
	}
}

func TestComposeRequestIsNumericPrivateAndDoesNotCopyLargePayloads(t *testing.T) {
	payload := strings.Repeat("private-image-and-schema-", 1<<18)
	req := llm.Request{System: "private-system", Tools: []llm.ToolSchema{{Name: "private-tool", Description: "private-description", Parameters: json.RawMessage(payload)}}, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: payload}}}}}
	var composition ContextComposition
	if allocations := testing.AllocsPerRun(10, func() { composition = ComposeRequest(req) }); allocations != 0 {
		t.Fatalf("composition copied/serialized request: %g allocations", allocations)
	}
	if composition.ImageEncodedBytes != len(payload) {
		t.Fatalf("image was decoded/estimated: %d != %d", composition.ImageEncodedBytes, len(payload))
	}
	typ := reflect.TypeOf(composition)
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type.Kind() != reflect.Int {
			t.Fatalf("composition can retain nonnumeric content: %s", typ.Field(i).Name)
		}
	}
	serialized, err := json.Marshal(composition)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "private-") {
		t.Fatalf("composition retained request content: %s", serialized)
	}
	// Counting is read-only, including large opaque raw JSON that is not decoded.
	if req.Messages[0].Content[0].ImageData != payload || string(req.Tools[0].Parameters) != payload {
		t.Fatal("composition mutated request")
	}
}
