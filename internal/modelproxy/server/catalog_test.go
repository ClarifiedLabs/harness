package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
	"harness/internal/modelproxy/modeldiscovery"
	"harness/internal/modelproxy/protocol"
)

const contextMetadataProviderJSON = `{
  "name":"testai","api_type":"openai","base_url":"https://api.test/v1","managed":true,
  "models":[
    {"name":"alpha","supports_experimental_context":false,"service_tiers":[{"id":"fast"}],
     "context_tool_definitions":{"history_search":{"description":"configured","parameters":"{}"}}},
    {"name":"fallback","supports_experimental_context":true}
  ]
}`

func contextDefinitions(description, parameters string) map[string]llm.ContextToolDefinition {
	return map[string]llm.ContextToolDefinition{
		"history_search": {Description: &description, Parameters: &parameters},
	}
}

func assertContextMetadata(t *testing.T, supported bool, definitions map[string]llm.ContextToolDefinition, wantSupported bool, wantDefinitions map[string]llm.ContextToolDefinition) {
	t.Helper()
	if supported != wantSupported {
		t.Errorf("SupportsExperimentalContext = %v, want %v", supported, wantSupported)
	}
	if !reflect.DeepEqual(definitions, wantDefinitions) {
		got, _ := json.Marshal(definitions)
		want, _ := json.Marshal(wantDefinitions)
		t.Errorf("ContextToolDefinitions = %s, want %s", got, want)
	}
}

func servedContextCatalog(t *testing.T, h *Handler) protocol.Catalog {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: status %d: %s", w.Code, w.Body.String())
	}
	var catalog protocol.Catalog
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode served catalog: %v", err)
	}
	return catalog
}

func TestLiveContextMetadataThroughEffectiveProvidersAndServedCatalog(t *testing.T) {
	for _, scenario := range []struct {
		name              string
		authoritative     bool
		configuredSupport bool
	}{
		{"authoritative_config_false", true, false},
		{"authoritative_config_true", true, true},
		{"stale_cached_config_false", false, false},
		{"stale_cached_config_true", false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			authoritative := scenario.authoritative
			providerJSON := contextMetadataProviderJSON
			if scenario.configuredSupport {
				providerJSON = strings.Replace(providerJSON, `"supports_experimental_context":false`, `"supports_experimental_context":true`, 1)
			}
			h := newTestHandler(t, "testai.json", providerJSON, Options{New: fixedUsageProvider(llm.Usage{})})
			configuredDefinitions := contextDefinitions("configured", "{}")
			initial := catalogTarget(t, servedContextCatalog(t, h), "testai", "alpha")
			assertContextMetadata(t, initial.SupportsExperimentalContext, initial.ContextToolDefinitions, scenario.configuredSupport, configuredDefinitions)

			enabled, disabled := true, false
			for _, step := range []struct {
				name        string
				supported   *bool
				definitions map[string]llm.ContextToolDefinition
				wantSupport bool
				wantDefs    map[string]llm.ContextToolDefinition
			}{
				{"enable_and_update_schema", &enabled, contextDefinitions("live search", `{"type":"object","properties":{"query":{"type":"string"}}}`), true, contextDefinitions("live search", `{"type":"object","properties":{"query":{"type":"string"}}}`)},
				{"disable_and_replace_definition", &disabled, contextDefinitions("disabled search", `{"type":"object","properties":{"limit":{"type":"integer"}}}`), false, contextDefinitions("disabled search", `{"type":"object","properties":{"limit":{"type":"integer"}}}`)},
				{"omitted_fields_use_config", nil, nil, scenario.configuredSupport, configuredDefinitions},
				{"explicit_empty_definitions_clear_config", &disabled, map[string]llm.ContextToolDefinition{}, false, map[string]llm.ContextToolDefinition{}},
				{"definitions_without_capability", nil, contextDefinitions("schema only", `{"type":"object","required":["query"]}`), scenario.configuredSupport, contextDefinitions("schema only", `{"type":"object","required":["query"]}`)},
			} {
				t.Run(step.name, func(t *testing.T) {
					state := modeldiscovery.State{Authoritative: authoritative, Snapshot: modeldiscovery.Snapshot{
						Complete: true,
						Models: map[string]modeldiscovery.Model{
							"alpha": {ID: "alpha", Eligible: true, SupportsExperimentalContext: step.supported, ContextToolDefinitions: step.definitions},
						},
					}}
					h.UpdateProviderCatalogs(map[string]modeldiscovery.State{"testai": state})

					providers, pruned := h.effectiveProviders(h.modelsDevCatalog, h.providerCatalogs)
					if pruned != authoritative {
						t.Errorf("pruned = %v, want %v", pruned, authoritative)
					}
					if len(providers) != 1 || len(providers[0].Models) == 0 {
						t.Fatalf("effective providers = %+v", providers)
					}
					entry := providers[0].Models[0]
					if entry.Name != "alpha" {
						t.Fatalf("first effective model = %q, want alpha", entry.Name)
					}
					assertContextMetadata(t, entry.SupportsExperimentalContext, entry.ContextToolDefinitions, step.wantSupport, step.wantDefs)

					catalog := servedContextCatalog(t, h)
					for _, model := range []string{"alpha", "alpha:fast"} {
						target := catalogTarget(t, catalog, "testai", model)
						// Empty maps are omitted on the wire.
						wantDefs := step.wantDefs
						if len(wantDefs) == 0 {
							wantDefs = nil
						}
						assertContextMetadata(t, target.SupportsExperimentalContext, target.ContextToolDefinitions, step.wantSupport, wantDefs)
					}
					if got := catalogHasTarget(catalog, "testai", "fallback"); got == authoritative {
						t.Errorf("fallback target present = %v, want %v", got, !authoritative)
					}
					if !authoritative {
						fallback := catalogTarget(t, catalog, "testai", "fallback")
						assertContextMetadata(t, fallback.SupportsExperimentalContext, fallback.ContextToolDefinitions, true, nil)
					}

					// Effective entries must own their maps and pointed-to strings.
					if definition, ok := entry.ContextToolDefinitions["history_search"]; ok {
						*definition.Description = "mutated description"
						*definition.Parameters = "mutated schema"
						delete(entry.ContextToolDefinitions, "history_search")
					}
					assertContextMetadata(t, h.providers[0].Models[0].SupportsExperimentalContext, h.providers[0].Models[0].ContextToolDefinitions, scenario.configuredSupport, configuredDefinitions)
					if !reflect.DeepEqual(h.providerCatalogs["testai"].Snapshot.Models["alpha"].ContextToolDefinitions, step.definitions) {
						t.Error("effective entry mutation changed the cached snapshot")
					}
					again, _ := h.effectiveProviders(h.modelsDevCatalog, h.providerCatalogs)
					assertContextMetadata(t, again[0].Models[0].SupportsExperimentalContext, again[0].Models[0].ContextToolDefinitions, step.wantSupport, step.wantDefs)
				})
			}
		})
	}
}

func TestEffectiveProvidersContextMetadataSourceFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		manual      bool
		unsupported bool
		model       *modelcatalog.Model
		wantSupport bool
		wantDefs    map[string]llm.ContextToolDefinition
	}{
		{name: "configured_fallback", wantDefs: contextDefinitions("configured", "{}")},
		{name: "models_dev_enables", model: &modelcatalog.Model{ID: "alpha", SupportsExperimentalContext: true, ContextToolDefinitions: contextDefinitions("models.dev", `{"type":"object"}`)}, wantSupport: true, wantDefs: contextDefinitions("models.dev", `{"type":"object"}`)},
		{name: "models_dev_omitted_definitions_keep_config", model: &modelcatalog.Model{ID: "alpha", SupportsExperimentalContext: true}, wantSupport: true, wantDefs: contextDefinitions("configured", "{}")},
		{name: "unsupported_discovery_uses_model_id_lookup", unsupported: true, model: &modelcatalog.Model{ID: "alpha", SupportsExperimentalContext: true, ContextToolDefinitions: contextDefinitions("authoritative models.dev", `{"required":["query"]}`)}, wantSupport: true, wantDefs: contextDefinitions("authoritative models.dev", `{"required":["query"]}`)},
		{name: "manual_does_not_opt_into_discovery", manual: true, model: &modelcatalog.Model{ID: "alpha", SupportsExperimentalContext: true, ContextToolDefinitions: contextDefinitions("ignored", `{"type":"object"}`)}, wantDefs: contextDefinitions("configured", "{}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providerJSON := contextMetadataProviderJSON
			if tc.manual {
				providerJSON = strings.Replace(providerJSON, `"managed":true`, `"managed":false`, 1)
			}
			var md *modelcatalog.Catalog
			if tc.model != nil {
				key := "alpha"
				if tc.unsupported {
					// The authoritative catalog can use a different map key.
					key = "catalog-key"
				}
				md = &modelcatalog.Catalog{Providers: map[string]modelcatalog.Provider{
					"testai": {ID: "testai", Models: map[string]modelcatalog.Model{key: *tc.model}},
				}}
			}
			var states map[string]modeldiscovery.State
			if tc.unsupported {
				states = map[string]modeldiscovery.State{"testai": {Unsupported: true}}
			}
			h := newTestHandler(t, "testai.json", providerJSON, Options{ModelsDevCatalog: md, ProviderCatalogs: states, New: fixedUsageProvider(llm.Usage{})})
			providers, _ := h.effectiveProviders(md, states)
			if len(providers) != 1 || len(providers[0].Models) == 0 {
				t.Fatalf("effective providers = %+v", providers)
			}
			entry := providers[0].Models[0]
			assertContextMetadata(t, entry.SupportsExperimentalContext, entry.ContextToolDefinitions, tc.wantSupport, tc.wantDefs)
			target := catalogTarget(t, servedContextCatalog(t, h), "testai", "alpha")
			assertContextMetadata(t, target.SupportsExperimentalContext, target.ContextToolDefinitions, tc.wantSupport, tc.wantDefs)
		})
	}
}
