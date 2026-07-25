package responses

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type apiSurface struct {
	Source struct {
		Retrieved string `json:"retrieved"`
		Create    apiSurfaceSource
		Count     apiSurfaceSource
		Streaming apiSurfaceSource
	} `json:"source"`
	RequestProperties      []string            `json:"request_properties"`
	CountRequestProperties []string            `json:"count_request_properties"`
	InputContentTypes      []string            `json:"input_content_types"`
	InputItemTypes         []string            `json:"input_item_types"`
	OutputItemTypes        []string            `json:"output_item_types"`
	OutputContentTypes     []string            `json:"output_content_types"`
	Tools                  []string            `json:"tools"`
	Events                 []string            `json:"events"`
	UsageProperties        []string            `json:"usage_properties"`
	RequiredFields         map[string][]string `json:"required_fields"`
}

type apiSurfaceSource struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func TestAPISupportedSurfaceIsExplicitlyPartitioned(t *testing.T) {
	data, err := os.ReadFile("testdata/api_surface.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec apiSurface
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Source.Retrieved == "" ||
		spec.Source.Create.URL != "https://developers.openai.com/api/reference/resources/responses/methods/create" ||
		spec.Source.Count.URL != "https://developers.openai.com/api/reference/resources/responses/subresources/input_tokens/methods/count" ||
		spec.Source.Streaming.URL != "https://developers.openai.com/api/reference/resources/responses/streaming-events" ||
		spec.Source.Create.SHA256 == "" || spec.Source.Count.SHA256 == "" || spec.Source.Streaming.SHA256 == "" {
		t.Fatalf("incomplete API source metadata: %+v", spec.Source)
	}
	assertContractStringsEqual(t, "FunctionCallOutput required fields", spec.RequiredFields["FunctionCallOutput"], []string{"call_id", "output", "type"})

	assertContractPartition(t, "request properties", spec.RequestProperties,
		[]string{"include", "input", "instructions", "max_output_tokens", "model", "parallel_tool_calls", "previous_response_id", "prompt_cache_key", "reasoning", "service_tier", "store", "stream", "temperature", "tools"},
		[]string{"background", "context_management", "conversation", "max_tool_calls", "metadata", "moderation", "prompt", "prompt_cache_options", "prompt_cache_retention", "safety_identifier", "stream_options", "text", "tool_choice", "top_logprobs", "top_p", "truncation", "user"})
	assertContractPartition(t, "count request properties", spec.CountRequestProperties,
		[]string{"input", "instructions", "model", "previous_response_id", "reasoning", "tools"},
		[]string{"conversation", "parallel_tool_calls", "personality", "text", "tool_choice", "truncation"})
	assertContractPartition(t, "input content", spec.InputContentTypes,
		[]string{"input_image", "input_text"},
		[]string{"input_audio", "input_file"})
	assertContractPartition(t, "input items", spec.InputItemTypes,
		[]string{"function_call", "function_call_output", "message", "reasoning"},
		[]string{"apply_patch_call", "apply_patch_call_output", "code_interpreter_call", "computer_call", "computer_call_output", "custom_tool_call", "custom_tool_call_output", "file_search_call", "image_generation_call", "item_reference", "local_shell_call", "local_shell_call_output", "mcp_approval_request", "mcp_approval_response", "mcp_call", "mcp_list_tools", "shell_call", "shell_call_output", "web_search_call"})
	assertContractPartition(t, "output items", spec.OutputItemTypes,
		[]string{"function_call", "message", "reasoning", "web_search_call"},
		[]string{"apply_patch_call", "code_interpreter_call", "compaction", "computer_call", "custom_tool_call", "file_search_call", "image_generation_call", "local_shell_call", "mcp_approval_request", "mcp_call", "mcp_list_tools", "shell_call"})
	assertContractPartition(t, "output content", spec.OutputContentTypes,
		[]string{"output_text", "refusal"}, nil)
	assertContractPartition(t, "tools", spec.Tools,
		[]string{"function", "web_search"},
		[]string{"apply_patch", "code_interpreter", "computer_use_preview", "custom", "file_search", "image_generation", "local_shell", "mcp", "shell"})
	assertContractPartition(t, "events", spec.Events,
		[]string{"error", "response.completed", "response.content_part.added", "response.content_part.done", "response.created", "response.failed", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.in_progress", "response.incomplete", "response.output_item.added", "response.output_item.done", "response.output_text.annotation.added", "response.output_text.delta", "response.output_text.done", "response.queued", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done", "response.refusal.delta", "response.refusal.done", "response.web_search_call.completed", "response.web_search_call.in_progress", "response.web_search_call.searching"},
		[]string{"response.audio.delta", "response.audio.done", "response.audio.transcript.delta", "response.audio.transcript.done", "response.code_interpreter_call.completed", "response.code_interpreter_call.in_progress", "response.code_interpreter_call.interpreting", "response.code_interpreter_call_code.delta", "response.code_interpreter_call_code.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.file_search_call.completed", "response.file_search_call.in_progress", "response.file_search_call.searching", "response.image_generation_call.completed", "response.image_generation_call.generating", "response.image_generation_call.in_progress", "response.image_generation_call.partial_image", "response.mcp_call.completed", "response.mcp_call.failed", "response.mcp_call.in_progress", "response.mcp_call_arguments.delta", "response.mcp_call_arguments.done", "response.mcp_list_tools.completed", "response.mcp_list_tools.failed", "response.mcp_list_tools.in_progress"})
	assertContractPartition(t, "usage", spec.UsageProperties,
		[]string{"input_tokens", "input_tokens_details", "output_tokens", "output_tokens_details"},
		[]string{"total_tokens"})
}

func assertContractPartition(t *testing.T, name string, all, supported, unsupported []string) {
	t.Helper()
	partition := append(append([]string(nil), supported...), unsupported...)
	assertContractStringsEqual(t, name, all, partition)
	if contractHasDuplicate(partition) {
		t.Fatalf("%s partition contains duplicates: %v", name, partition)
	}
}

func assertContractStringsEqual(t *testing.T, name string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func contractHasDuplicate(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}
