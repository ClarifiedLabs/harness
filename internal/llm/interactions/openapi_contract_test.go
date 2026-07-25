package interactions

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type openAPISurface struct {
	Source struct {
		URL       string `json:"url"`
		Retrieved string `json:"retrieved"`
		SHA256    string `json:"sha256"`
		OpenAPI   string `json:"openapi"`
		Version   string `json:"version"`
		Revision  string `json:"revision"`
	} `json:"source"`
	Request struct {
		Required                   []string `json:"required"`
		InputProperties            []string `json:"input_properties"`
		GenerationConfigProperties []string `json:"generation_config_properties"`
	} `json:"request"`
	Tools          []string            `json:"tools"`
	Steps          []string            `json:"steps"`
	Content        []string            `json:"content"`
	Events         []string            `json:"events"`
	Usage          []string            `json:"usage_properties"`
	RequiredFields map[string][]string `json:"required_fields"`
}

func TestOpenAPISupportedSurfaceIsExplicitlyPartitioned(t *testing.T) {
	data, err := os.ReadFile("testdata/openapi_surface.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec openAPISurface
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Source.URL != "https://ai.google.dev/static/api/interactions.openapi.json" ||
		spec.Source.OpenAPI != "3.0.3" || spec.Source.Version != "v1beta" ||
		spec.Source.Revision != "0" || spec.Source.Retrieved == "" || spec.Source.SHA256 == "" {
		t.Fatalf("incomplete OpenAPI source metadata: %+v", spec.Source)
	}
	assertStringsEqual(t, "required request fields", spec.Request.Required, []string{"input", "model"})
	assertStringsEqual(t, "FunctionResultStep required fields", spec.RequiredFields["FunctionResultStep"], []string{"call_id", "result", "type"})
	assertStringsEqual(t, "TextContent required fields", spec.RequiredFields["TextContent"], []string{"text", "type"})

	assertSurfacePartition(t, "request properties", spec.Request.InputProperties,
		[]string{"generation_config", "input", "model", "previous_interaction_id", "response_format", "service_tier", "store", "stream", "system_instruction", "tools"},
		[]string{"background", "environment", "labels", "response_mime_type", "response_modalities", "safety_settings", "webhook_config"})
	assertSurfacePartition(t, "generation config", spec.Request.GenerationConfigProperties,
		[]string{"max_output_tokens", "stop_sequences", "thinking_level", "thinking_summaries"},
		[]string{"image_config", "seed", "speech_config", "tool_choice", "transcription_config", "video_config"})
	assertSurfacePartition(t, "tools", spec.Tools,
		[]string{"function", "google_search"},
		[]string{"code_execution", "url_context", "computer_use", "mcp_server", "file_search", "google_maps", "retrieval"})
	assertSurfacePartition(t, "steps", spec.Steps,
		[]string{"user_input", "model_output", "thought", "function_call", "google_search_call", "function_result", "google_search_result"},
		[]string{"code_execution_call", "url_context_call", "mcp_server_tool_call", "file_search_call", "google_maps_call", "code_execution_result", "url_context_result", "mcp_server_tool_result", "file_search_result", "google_maps_result"})
	assertSurfacePartition(t, "content", spec.Content,
		[]string{"text", "image"},
		[]string{"audio", "document", "video"})
	assertSurfacePartition(t, "events", spec.Events,
		[]string{"interaction.created", "interaction.completed", "interaction.status_update", "error", "step.start", "step.delta", "step.stop"},
		nil)
	assertSurfacePartition(t, "usage", spec.Usage,
		[]string{"total_cached_tokens", "total_input_tokens", "total_output_tokens", "total_thought_tokens"},
		[]string{"cached_tokens_by_modality", "grounding_tool_count", "input_tokens_by_modality", "output_tokens_by_modality", "tool_use_tokens_by_modality", "total_tokens", "total_tool_use_tokens"})
}

func assertSurfacePartition(t *testing.T, name string, all, supported, unsupported []string) {
	t.Helper()
	partition := append(append([]string(nil), supported...), unsupported...)
	assertStringsEqual(t, name, all, partition)
	if hasDuplicate(partition) {
		t.Fatalf("%s partition contains duplicates: %v", name, partition)
	}
}

func assertStringsEqual(t *testing.T, name string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func hasDuplicate(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}
