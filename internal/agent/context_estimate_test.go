package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
)

// These are content estimates, not estimates of the persisted JSON envelope.
// In particular, quoting, bookkeeping, and base64 size must not drive resets.
func TestContextEstimatesIgnoreEnvelopeAndEscaping(t *testing.T) {
	estimates := func(messages []llm.Message) [2]int {
		return [2]int{estimateTokens(messages), estimateRequest(llm.Request{Messages: messages}, 100_000).Messages}
	}
	plain := []llm.Message{userText(strings.Repeat("x", 400))}
	want := estimates(plain)
	for _, text := range []string{strings.Repeat("\"", 400), strings.Repeat("\n", 400), strings.Repeat("\\", 400)} {
		messages := []llm.Message{userText(text)}
		messages[0].Time = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
		messages[0].SteerID = strings.Repeat("metadata", 100)
		messages[0].Origin = llm.MessageOriginSteer
		messages[0].Compaction = &llm.CompactionMetadata{Summary: strings.Repeat("not replayed", 100)}
		before := cloneMessages(messages)
		if got := estimates(messages); got != want {
			t.Fatalf("equal-length content estimated as %v, want %v", got, want)
		}
		if !reflect.DeepEqual(messages, before) {
			t.Fatal("estimation mutated history")
		}
	}
}

func TestContextEstimatesCountStructuredToolPayloadOnce(t *testing.T) {
	// Tool arguments really are JSON; count that syntax, but not another layer
	// of JSON escaping added by the transport or saved transcript.
	input := json.RawMessage(`{"path":"quoted\\name","text":"line\nnext"}`)
	messages := []llm.Message{asstToolUse("call", "write", `{}`), toolResult("call", "")}
	baseHistory := estimateTokens(messages)
	baseRequest := estimateRequest(llm.Request{Messages: messages}, 100_000).Messages
	messages[0].Content[0].ToolInput = input
	messages[1].Content[0].ResultText = strings.Repeat(`{"ok":true}`, 100)
	added := len(input) - 2 + len(messages[1].Content[0].ResultText)
	for name, delta := range map[string]int{
		"history": estimateTokens(messages) - baseHistory,
		"request": estimateRequest(llm.Request{Messages: messages}, 100_000).Messages - baseRequest,
	} {
		if delta < added/bytesPerToken || delta > (added+bytesPerToken-1)/bytesPerToken {
			t.Errorf("%s delta=%d, want content bytes/4 for %d bytes", name, delta, added)
		}
	}
}

func TestContextEstimatesImagePayloadAndMetadataIndependent(t *testing.T) {
	for _, nested := range []bool{false, true} {
		image := llm.ContentBlock{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageDetail: "original", ImageData: "YWJj"}
		messages := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{image}}}
		block := &messages[0].Content[0]
		if nested {
			messages = []llm.Message{toolResult("call", "attached")}
			messages[0].Content[0].ResultContent = []llm.ContentBlock{image}
			block = &messages[0].Content[0].ResultContent[0]
		}
		before := [2]int{estimateTokens(messages), estimateRequest(llm.Request{Messages: messages}, 100_000).Messages}
		block.ImageData = strings.Repeat("A", 1<<20)
		block.ImageWidth, block.ImageHeight, block.ImageBytes, block.ImageEncodedBytes = 4096, 4096, 900_000, 1<<20
		if after := [2]int{estimateTokens(messages), estimateRequest(llm.Request{Messages: messages}, 100_000).Messages}; after != before {
			t.Fatalf("nested=%v: image storage changed estimate %v -> %v", nested, before, after)
		}
		if before[0] < imageTokenEstimate || before[1] < imageTokenEstimate {
			t.Fatalf("nested=%v: missing image weight: %v", nested, before)
		}
	}
}
