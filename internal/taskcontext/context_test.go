package taskcontext

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/tools"
)

func TestCatalogContextToolDefinitionsAndSchemaFallback(t *testing.T) {
	dir := t.TempDir()
	empty := ""
	description := "model search"
	parameters := `{"type":"object","properties":{"query":{"type":"string","description":"model query"}},"required":["query"]}`
	incompatible := `{"type":"object","properties":{"item_id":{"type":"string"}},"required":["item_id"]}`
	invalid := `not JSON`
	manager := New(func() string { return dir })
	manager.SetPolicy(func() Policy {
		return Policy{Reset: true, Identity: "model", ToolDefinitions: map[string]llm.ContextToolDefinition{
			"task_notes":     {Description: &empty},
			"history_search": {Description: &description, Parameters: &parameters},
			"history_read":   {Parameters: &incompatible},
			"new_context":    {Parameters: &invalid},
		}}
	})
	registry := &tools.Registry{}
	manager.Register(registry)
	specs := map[string]llm.ToolSchema{}
	for _, spec := range registry.Specs() {
		specs[spec.Name] = spec
	}
	if specs["task_notes"].Description != "" || specs["history_search"].Description != description {
		t.Fatalf("catalog descriptions not applied: %+v", specs)
	}
	if got := string(specs["history_search"].Parameters); !strings.Contains(got, `"model query"`) || !strings.Contains(got, `"required":["query"]`) {
		t.Fatalf("compatible schema = %s", got)
	}
	if got := string(specs["history_read"].Parameters); strings.Contains(got, "item_id") || !strings.Contains(got, `"id"`) {
		t.Fatalf("incompatible schema did not fall back: %s", got)
	}
	if got := string(specs["new_context"].Parameters); !strings.Contains(got, `"properties":{"_stage"`) || strings.Contains(got, "not JSON") {
		t.Fatalf("invalid schema did not fall back: %s", got)
	}
	base := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":20},"action":{"type":"string","enum":["read","write"]}}}`)
	for name, candidate := range map[string]string{
		"missing type":          `{"type":"object","properties":{"query":{}}}`,
		"wide maximum":          `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":21}}}`,
		"missing maximum":       `{"type":"object","properties":{"limit":{"type":"integer","minimum":1}}}`,
		"additional properties": `{"type":"object","additionalProperties":false}`,
		"combinator":            `{"type":"object","not":{}}`,
		"unknown action":        `{"type":"object","properties":{"action":{"type":"string","enum":["destroy"]}}}`,
		"unbounded action":      `{"type":"object","properties":{"action":{"type":"string"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if compatibleContextSchema(base, json.RawMessage(candidate)) {
				t.Fatal("accepted schema that advertises unsupported calls")
			}
		})
	}
}

func TestInvalidCatalogSchemasRetainBuiltInSchema(t *testing.T) {
	for name, candidate := range map[string]string{
		"duplicate required":      `{"type":"object","properties":{"query":{"type":"string"}},"required":["query","query"]}`,
		"null description":        `{"type":"object","description":null}`,
		"null property guidance":  `{"type":"object","properties":{"query":{"type":"string","description":null}}}`,
		"null properties":         `{"type":"object","properties":null}`,
		"null property":           `{"type":"object","properties":{"query":null}}`,
		"null required":           `{"type":"object","required":null}`,
		"null required entry":     `{"type":"object","required":[null]}`,
		"null additional policy":  `{"type":"object","additionalProperties":null}`,
		"invalid encrypted":       `{"type":"object","encrypted":"yes"}`,
		"invalid property cipher": `{"type":"object","properties":{"query":{"type":"string","encrypted":null}}}`,
		"null minimum":            `{"type":"object","properties":{"offset":{"type":"integer","minimum":null}}}`,
		"null maximum":            `{"type":"object","properties":{"query":{"type":"string","maximum":null}}}`,
		"empty enum":              `{"type":"object","properties":{"query":{"type":"string","enum":[]}}}`,
		"null enum":               `{"type":"object","properties":{"query":{"type":"string","enum":null}}}`,
		"duplicate enum":          `{"type":"object","properties":{"query":{"type":"string","enum":["same","same"]}}}`,
		"wrong enum type":         `{"type":"object","properties":{"query":{"type":"string","enum":[1]}}}`,
		"object enum":             `{"type":"object","properties":{"query":{"type":"string","enum":[{}]}}}`,
		"fractional integer enum": `{"type":"object","properties":{"offset":{"type":"integer","minimum":0,"enum":[0.5]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			manager := New(nil)
			search := &tool{Manager: manager, name: "history_search"}
			want := string(search.Schema())
			manager.SetPolicy(func() Policy {
				return Policy{ToolDefinitions: map[string]llm.ContextToolDefinition{
					"history_search": {Parameters: &candidate},
				}}
			})
			if got := string(search.Schema()); got != want {
				t.Fatalf("invalid override did not retain built-in schema: %s", got)
			}
		})
	}
}

func TestCompatibleCatalogSchemaKeywordTypes(t *testing.T) {
	candidate := `{"type":"object","description":"","properties":{"query":{"type":"string","description":"","enum":["needle"],"encrypted":false},"offset":{"type":"integer","minimum":0,"enum":[0,1]},"windows":{"type":"boolean","enum":[true,false]}},"required":["query"],"additionalProperties":true,"encrypted":true}`
	manager := New(nil)
	manager.SetPolicy(func() Policy {
		return Policy{ToolDefinitions: map[string]llm.ContextToolDefinition{
			"history_search": {Parameters: &candidate},
		}}
	})
	search := &tool{Manager: manager, name: "history_search"}
	if got := string(search.Schema()); got != candidate {
		t.Fatalf("valid compatible override was discarded: %s", got)
	}
}

func TestHistoryLookupIsBoundedAndOmitsOpaqueState(t *testing.T) {
	dir := t.TempDir()
	tree, err := session.LinearTree(time.Now(), "", []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Find the Astra roadmap."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockReasoning, ReasoningEncrypted: "secret-opaque"}, {Kind: llm.BlockText, Text: strings.Repeat("evidence ", 1000)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	body, err := search(context.Background(), dir, "ASTRA", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Hits []hit `json:"hits"`
		Next int64 `json:"next_offset"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits: %s", body)
	}
	body, err = readEntry(dir, result.Hits[0].Offset, 0, 12, nil)
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Text string `json:"text"`
		Next int    `json:"next_text_offset"`
	}
	if err := json.Unmarshal([]byte(body), &entry); err != nil {
		t.Fatal(err)
	}
	if len(entry.Text) > 12 || entry.Next != len(entry.Text) {
		t.Fatalf("unbounded entry: %s", body)
	}
	body, err = search(context.Background(), dir, "secret-opaque", 0, 20)
	if err != nil || strings.Contains(body, "secret-opaque") {
		t.Fatalf("opaque state exposed: %s %v", body, err)
	}
	if _, err := readEntry(dir, result.Hits[0].Offset+2, 0, 12, nil); err == nil {
		t.Fatal("accepted offset inside an entry")
	}
}

func TestHistoryWindowsFiltersAndStableIDs(t *testing.T) {
	dir := t.TempDir()
	initial := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "debug cache"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "test", ToolName: "shell", ToolInput: json.RawMessage(`{"command":"go test"}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "test", ResultText: "FAIL: stale cache"}}},
	}
	tree, err := session.LinearTree(time.Now(), "", initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.AppendContextReset([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "fresh context"}}}}, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	decode := func(in historyInput) []hit {
		t.Helper()
		body, err := lookupHistory(context.Background(), dir, in)
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ Hits []hit }
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatal(err)
		}
		return result.Hits
	}
	windows := decode(historyInput{Windows: true, Limit: 20})
	if len(windows) != 2 || windows[0].ID != tree.Header.ID || windows[1].ID != tree.ActiveLeaf {
		t.Fatalf("windows = %+v", windows)
	}
	if got := decode(historyInput{Windows: true, WindowID: tree.ActiveLeaf, Limit: 20}); len(got) != 1 || got[0].ID != tree.ActiveLeaf {
		t.Fatalf("filtered windows = %+v", got)
	}
	if got := decode(historyInput{WindowID: tree.ActiveLeaf, Role: "user", Limit: 20}); len(got) != 1 {
		t.Fatalf("reset role filter = %+v", got)
	}
	entries := decode(historyInput{WindowID: tree.Header.ID, Role: "tool", ToolName: "shell", Limit: 20})
	if len(entries) != 1 || entries[0].WindowID != tree.Header.ID || !strings.Contains(entries[0].Text, "stale cache") {
		t.Fatalf("filtered entries = %+v", entries)
	}
	if got := decode(historyInput{WindowID: tree.ActiveLeaf, Query: "stale cache", Limit: 20}); len(got) != 0 {
		t.Fatal("window filter leaked old entries")
	}
	if err := tree.SyncTranscript(append([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "fresh context"}}}}, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "new work"}}})); err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	body, err := readHistory(context.Background(), dir, historyInput{ID: entries[0].ID, MaxBytes: 4096})
	if err != nil || !strings.Contains(body, "stale cache") {
		t.Fatalf("stable lookup after save = %s %v", body, err)
	}
}
