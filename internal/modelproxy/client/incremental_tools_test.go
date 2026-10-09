package client

import (
	"encoding/json"
	"testing"

	"harness/internal/modelproxy/protocol"
)

func TestRegistryPreservesIncrementalToolsCapability(t *testing.T) {
	var catalog protocol.Catalog
	if err := json.Unmarshal([]byte(`{"targets":[{"id":"openai:gpt-5.6","aliases":["gpt-5.6"],"incremental_tools":true},{"id":"openai:gpt-5.5"}]}`), &catalog); err != nil {
		t.Fatal(err)
	}
	registry := Registry(catalog)
	for _, tc := range []struct {
		model string
		want  bool
	}{{"openai:gpt-5.6", true}, {"gpt-5.6", true}, {"openai:gpt-5.5", false}} {
		info, ok := registry.Lookup(tc.model)
		if !ok || info.IncrementalTools != tc.want {
			t.Errorf("Lookup(%q) incremental tools = %v, ok = %v; want %v", tc.model, info.IncrementalTools, ok, tc.want)
		}
	}
}
