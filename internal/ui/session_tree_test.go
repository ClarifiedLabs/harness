package ui

import (
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/session"
)

func TestTreeEntryPreviewRichToolResultUsesSafeMetadata(t *testing.T) {
	payload := "SECRET_BASE64_PAYLOAD"
	entry := session.Entry{Type: session.EntrySegment, Messages: []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "private result text",
			ResultContent: []llm.ContentBlock{{
				Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: payload, ImageName: "/private/screen.png",
			}},
		}},
	}}}
	got := treeEntryPreview(entry)
	if got != "[tool result: 1 image image/png]" {
		t.Fatalf("preview = %q", got)
	}
	for _, secret := range []string{payload, "/private/screen.png", "private result text", "call_1"} {
		if strings.Contains(got, secret) {
			t.Fatalf("preview leaked %q: %q", secret, got)
		}
	}
}

func TestLoadedTreeImagesUsesPersistedSizes(t *testing.T) {
	images := loadedTreeImages([]llm.ContentBlock{{
		Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "", ImageBytes: 69, ImageEncodedBytes: 92,
	}})
	if len(images) != 1 || images[0].Info.Bytes != 69 || images[0].Info.EncodedBytes != 92 {
		t.Fatalf("loaded image metadata = %+v", images)
	}
}
