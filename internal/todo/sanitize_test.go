package todo

import (
	"strings"
	"testing"
)

func TestTextSanitizerRunsBeforeValidationAndStorage(t *testing.T) {
	store := NewStore()
	tool := NewToolWithTextSanitizer(store, func(text string) string { return strings.ReplaceAll(text, "unsafe", "") })
	if _, err := runUpdate(t, tool, []Item{{Step: "unsafe inspect", Status: StatusPending}}); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot()[0].Step; got != "inspect" {
		t.Fatalf("stored step = %q", got)
	}
	if _, err := runUpdate(t, tool, []Item{{Step: "unsafe", Status: StatusPending}}); err == nil {
		t.Fatal("step empty after sanitization was accepted")
	}
	if got := store.Snapshot()[0].Step; got != "inspect" {
		t.Fatalf("invalid step changed store: %q", got)
	}
	if _, err := runUpdate(t, NewTool(store), []Item{{Step: "unsafe", Status: StatusPending}}); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot()[0].Step; got != "unsafe" {
		t.Fatal("ordinary tool unexpectedly sanitizes text")
	}
}
