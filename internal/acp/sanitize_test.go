package acp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeModelFacingText(t *testing.T) {
	input := "pre\x1b[31mred\x1b[0m\x00\r\nline\tok" +
		"\x1b]8;;https://example.test\x07link\x1b]8;;\x1b\\post\x1b(B!" +
		"\u009b32mgreen\u009b0m\u202ebidi"
	got := SanitizeModelFacingText(input)
	want := "prered\nline\toklinkpost!greenbidi"
	if got != want {
		t.Fatalf("sanitized = %q, want %q", got, want)
	}
}

func TestSanitizeModelFacingTextDropsControlStrings(t *testing.T) {
	tests := map[string]string{
		"OSC BEL":        "a\x1b]title\x07b",
		"OSC ST":         "a\x1b]title\x1b\\b",
		"DCS":            "a\x1bPpayload\x1b\\b",
		"unterminated":   "safe\x1b]unsafe",
		"C1 OSC":         "a\u009dtitle\u009cb",
		"backspace bell": "a\bb\a",
	}
	want := map[string]string{
		"OSC BEL":        "ab",
		"OSC ST":         "ab",
		"DCS":            "ab",
		"unterminated":   "safe",
		"C1 OSC":         "ab",
		"backspace bell": "ab",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if got := SanitizeModelFacingText(input); got != want[name] {
				t.Fatalf("got %q, want %q", got, want[name])
			}
		})
	}
}

func TestSanitizeMalformedCSIPreservesFollowingUnicode(t *testing.T) {
	input := "safe\x1b[\n世界"
	if got, want := SanitizeModelFacingText(input), "safe\n世界"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSanitizeModelFacingTextBoundsUTF8(t *testing.T) {
	input := strings.Repeat("界", MaxModelFacingTextBytes) + "tail"
	got := SanitizeModelFacingText(input)
	if len(got) > MaxModelFacingTextBytes {
		t.Fatalf("length = %d, maximum %d", len(got), MaxModelFacingTextBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("sanitizer split UTF-8")
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Fatalf("missing truncation marker: suffix %q", got[len(got)-8:])
	}
}

func TestSanitizeModelFacingTextBoundsInputWork(t *testing.T) {
	input := strings.Repeat("\x00", MaxTextBytes+100) + "unsafe"
	got := SanitizeModelFacingText(input)
	if got != truncationMarker {
		t.Fatalf("sanitized oversized controls = %q", got)
	}
}

func TestSanitizeModelFacingContentDoesNotMutateInput(t *testing.T) {
	resourceText := "\x1b[31mresource\x1b[0m"
	input := ContentBlock{
		Type:        ContentTypeResource,
		Name:        "\x1b[1mname\x1b[0m",
		Description: "bad\x00description",
		Resource:    &EmbeddedResource{URI: "file:///x", Text: &resourceText},
	}
	got := SanitizeModelFacingContent(input)
	if got.Name != "name" || got.Description != "baddescription" || *got.Resource.Text != "resource" {
		t.Fatalf("sanitized content = %+v resource=%+v", got, got.Resource)
	}
	if input.Name == got.Name || *input.Resource.Text == *got.Resource.Text {
		t.Fatal("input was mutated or test input was not sanitized")
	}
	if input.Resource == got.Resource {
		t.Fatal("resource pointer was not detached")
	}
}

func TestSanitizeModelFacingUpdateDeepCopiesText(t *testing.T) {
	oldText := "\x1b[31mold\x1b[0m"
	content := []ToolCallContent{{
		Type:    ToolCallContentDiff,
		Path:    "/work/x\x00",
		OldText: &oldText,
		NewText: "\x1b[32mnew\x1b[0m",
	}}
	title := "\x1b[1mrun\x1b[0m"
	input := SessionUpdate{
		Kind: UpdateToolCallUpdate,
		ToolCallUpdate: &ToolCallUpdate{
			ToolCallID: "tc", Title: &title, Content: &content,
		},
	}
	got := SanitizeModelFacingUpdate(input)
	if got.ToolCallUpdate == input.ToolCallUpdate || got.ToolCallUpdate.Content == input.ToolCallUpdate.Content {
		t.Fatal("update payload was not detached")
	}
	if *got.ToolCallUpdate.Title != "run" {
		t.Fatalf("title = %q", *got.ToolCallUpdate.Title)
	}
	item := (*got.ToolCallUpdate.Content)[0]
	if item.Path != "/work/x" || *item.OldText != "old" || item.NewText != "new" {
		t.Fatalf("content = %+v", item)
	}
	if *input.ToolCallUpdate.Title != title || (*input.ToolCallUpdate.Content)[0].NewText == "new" {
		t.Fatal("input update was mutated")
	}
}

func TestSanitizeModelFacingContentCleansURIs(t *testing.T) {
	block := ContentBlock{
		Type: ContentTypeResourceLink,
		URI:  "https://example.test/\x1b]8;;https://evil.test\x07x\x1b]8;;\x1b\\",
		Name: "link",
	}
	clean := SanitizeModelFacingContent(block)
	if strings.ContainsAny(clean.URI, "\x1b\x07") {
		t.Fatalf("resource link URI retained control characters: %q", clean.URI)
	}
	if !strings.HasPrefix(clean.URI, "https://example.test/") || !strings.HasSuffix(clean.URI, "x") {
		t.Fatalf("resource link URI lost its valid content: %q", clean.URI)
	}

	blob := "aGVsbG8="
	resource := &EmbeddedResource{URI: "file:///tmp/\x1b[4mx", Blob: &blob}
	clean = SanitizeModelFacingContent(ContentBlock{Type: ContentTypeResource, Resource: resource})
	if strings.ContainsAny(clean.Resource.URI, "\x1b") {
		t.Fatalf("embedded resource URI retained control characters: %q", clean.Resource.URI)
	}
	if clean.Resource.Blob == nil || *clean.Resource.Blob != blob {
		t.Fatalf("embedded resource blob was modified: %+v", clean.Resource)
	}

	// A well-formed URI is left byte-identical.
	plain := ContentBlock{Type: ContentTypeResourceLink, URI: "https://example.test/path?q=1", Name: "n"}
	if got := SanitizeModelFacingContent(plain).URI; got != plain.URI {
		t.Fatalf("valid URI rewritten: %q", got)
	}
}
