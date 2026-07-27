package llm

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFingerprintMessagesIgnoresTimeAndIncludesNestedRichContent(t *testing.T) {
	base := []Message{
		{
			Role: RoleAssistant,
			Time: time.Unix(1, 0),
			Content: []ContentBlock{{
				Kind:      BlockToolUse,
				ToolUseID: "call",
				ToolName:  "view_image",
				ToolInput: json.RawMessage(`{"path":"screen.png"}`),
			}},
		},
		{
			Role: RoleUser,
			Time: time.Unix(2, 0),
			Content: []ContentBlock{{
				Kind:        BlockToolResult,
				ResultForID: "call",
				ResultText:  "attached",
				ResultContent: []ContentBlock{{
					Kind:           BlockImage,
					ImageMediaType: "image/png",
					ImageData:      "YWJj",
				}},
			}},
		},
	}
	want, err := FingerprintMessages(base)
	if err != nil {
		t.Fatal(err)
	}
	timeChanged := cloneFingerprintMessages(base)
	timeChanged[0].Time = time.Unix(999, 0)
	if got, err := FingerprintMessages(timeChanged); err != nil || got != want {
		t.Fatalf("timestamp-only fingerprint = %q, %v; want %q", got, err, want)
	}

	for _, mutate := range []func([]Message){
		func(messages []Message) { messages[1].Content[0].ResultText = "trimmed" },
		func(messages []Message) { messages[1].Content[0].ResultContent = nil },
		func(messages []Message) { messages[0].Content[0].ToolInput = json.RawMessage(`{"path":"other.png"}`) },
	} {
		changed := cloneFingerprintMessages(base)
		mutate(changed)
		if MatchesMessageFingerprint(changed, want) {
			t.Fatal("fingerprint ignored a nested rich-content rewrite")
		}
	}
}

func TestMatchesMessageFingerprintRejectsMalformedDigests(t *testing.T) {
	messages := []Message{{Role: RoleUser, Content: []ContentBlock{{Kind: BlockText, Text: "hello"}}}}
	digest, err := FingerprintMessages(messages)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchesMessageFingerprint(messages, digest) {
		t.Fatal("canonical fingerprint did not match")
	}
	for _, malformed := range []string{"", "abc", digest[:63], "G" + digest[1:]} {
		if MatchesMessageFingerprint(messages, malformed) {
			t.Fatalf("malformed fingerprint %q matched", malformed)
		}
	}
	if empty, err := FingerprintMessages(nil); err != nil || len(empty) != 64 {
		t.Fatalf("empty fingerprint = %q, %v", empty, err)
	}
}

func cloneFingerprintMessages(messages []Message) []Message {
	out := append([]Message(nil), messages...)
	for i := range out {
		out[i].Content = append([]ContentBlock(nil), messages[i].Content...)
		for j := range out[i].Content {
			out[i].Content[j].ResultContent = append([]ContentBlock(nil), messages[i].Content[j].ResultContent...)
		}
	}
	return out
}
