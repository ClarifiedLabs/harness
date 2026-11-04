package modelproxy

import (
	"encoding/json"
	"reflect"
	"testing"

	"harness/internal/llm"
)

func TestCloneCatalogOwnsEveryMutableField(t *testing.T) {
	label, schema := "description", `{"type":"object"}`
	source := Catalog{Targets: []Target{{ID: "p:m", Aliases: []string{"m"}, InputModalities: []string{"text"}, ServerTools: []string{"web_search"}, Price: llm.Price{Tiers: []llm.PriceTier{{Threshold: 10, Input: 2}}}, ContextToolDefinitions: map[string]llm.ContextToolDefinition{"search": {Description: &label, Parameters: &schema}}}}, Pricing: &PricingInfo{MaxAgeSeconds: 100}}
	want := CloneCatalog(source)
	cloned := CloneCatalog(source)
	mutateOwnedValue(reflect.ValueOf(&cloned).Elem())
	if !reflect.DeepEqual(source, want) {
		t.Fatal("catalog copy shares mutable runtime metadata")
	}
}
func TestCloneRequestOwnsEveryMutableField(t *testing.T) {
	enabled, budget, temperature := true, 100, 0.5
	source := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "original", InteractionStep: json.RawMessage(`{"opaque":true}`), ProviderCompaction: []json.RawMessage{json.RawMessage(`{"private":true}`)}, ResultContent: []llm.ContentBlock{{Kind: llm.BlockText, Text: "nested"}}}}}}, Tools: []llm.ToolSchema{{Name: "tool", Parameters: json.RawMessage(`{"type":"object"}`)}}, DeferredToolGroups: []llm.ToolGroup{{Name: "group", Tools: []llm.ToolSchema{{Name: "deferred", Parameters: json.RawMessage(`{}`)}}}}, ServerTools: []llm.ServerTool{{Name: "web_search", Parameters: json.RawMessage(`{}`)}}, Reasoning: llm.ReasoningConfig{Enabled: &enabled, BudgetTokens: &budget}, Temperature: &temperature, StopSeqs: []string{"stop"}, Betas: []string{"beta"}, RequestContext: []string{"context"}}
	want := CloneRequest(source)
	cloned := CloneRequest(source)
	mutateOwnedValue(reflect.ValueOf(&cloned).Elem())
	if !reflect.DeepEqual(source, want) {
		t.Fatal("request copy shares caller-owned state")
	}
}
func mutateOwnedValue(v reflect.Value) {
	if !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			mutateOwnedValue(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			mutateOwnedValue(v.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			mutateOwnedValue(v.Index(i))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			entry := reflect.New(v.Type().Elem()).Elem()
			entry.Set(v.MapIndex(key))
			mutateOwnedValue(entry)
			v.SetMapIndex(key, entry)
		}
	case reflect.String:
		v.SetString("changed")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int64:
		v.SetInt(-1)
	case reflect.Uint8:
		v.SetUint('!')
	case reflect.Float64:
		v.SetFloat(-1)
	}
}
