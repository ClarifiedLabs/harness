package anthropic

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type anthropicAPISurface struct {
	Source struct {
		Retrieved  string `json:"retrieved"`
		Create     anthropicAPISource
		Count      anthropicAPISource
		Streaming  anthropicAPISource
		Versioning anthropicAPISource
	} `json:"source"`
	RequestProperties         []string `json:"request_properties"`
	InputContentTypes         []string `json:"input_content_types"`
	OutputContentTypes        []string `json:"output_content_types"`
	ToolTypes                 []string `json:"tool_types"`
	EventTypes                []string `json:"event_types"`
	DeltaTypes                []string `json:"delta_types"`
	StopReasons               []string `json:"stop_reasons"`
	UsageProperties           []string `json:"usage_properties"`
	CacheCreationProperties   []string `json:"cache_creation_properties"`
	OutputDetailProperties    []string `json:"output_detail_properties"`
	ServerToolUsageProperties []string `json:"server_tool_usage_properties"`
}

type anthropicAPISource struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func TestAPISupportedSurfaceIsExplicitlyPartitioned(t *testing.T) {
	data, err := os.ReadFile("testdata/api_surface.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec anthropicAPISurface
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Source.Retrieved == "" ||
		spec.Source.Create.URL != "https://platform.claude.com/docs/en/api/messages/create" ||
		spec.Source.Count.URL != "https://platform.claude.com/docs/en/api/messages/count_tokens" ||
		spec.Source.Streaming.URL != "https://platform.claude.com/docs/en/build-with-claude/streaming" ||
		spec.Source.Versioning.URL != "https://platform.claude.com/docs/en/api/versioning" ||
		spec.Source.Create.SHA256 == "" || spec.Source.Count.SHA256 == "" ||
		spec.Source.Streaming.SHA256 == "" || spec.Source.Versioning.SHA256 == "" {
		t.Fatalf("incomplete API source metadata: %+v", spec.Source)
	}

	assertAnthropicPartition(t, "request properties", spec.RequestProperties,
		[]string{"max_tokens", "messages", "model", "output_config", "service_tier", "speed", "stop_sequences", "stream", "system", "temperature", "thinking", "tools"},
		[]string{"cache_control", "container", "inference_geo", "metadata", "tool_choice", "top_k", "top_p"})
	assertAnthropicPartition(t, "input content", spec.InputContentTypes,
		[]string{"image", "redacted_thinking", "server_tool_use", "text", "thinking", "tool_result", "tool_use", "web_search_tool_result"},
		[]string{"bash_code_execution_tool_result", "code_execution_tool_result", "container_upload", "document", "mid_conv_system", "search_result", "text_editor_code_execution_tool_result", "tool_search_tool_result", "web_fetch_tool_result"})
	assertAnthropicPartition(t, "output content", spec.OutputContentTypes,
		[]string{"redacted_thinking", "server_tool_use", "text", "thinking", "tool_use", "web_search_tool_result"},
		[]string{"bash_code_execution_tool_result", "code_execution_tool_result", "container_upload", "text_editor_code_execution_tool_result", "tool_search_tool_result", "web_fetch_tool_result"})
	assertAnthropicPartition(t, "tools", spec.ToolTypes,
		[]string{"custom", "web_search_20250305"},
		[]string{"bash", "code_execution", "computer", "memory", "text_editor", "tool_search", "web_fetch", "web_search_20260209"})
	assertAnthropicPartition(t, "events", spec.EventTypes,
		[]string{"content_block_delta", "content_block_start", "content_block_stop", "error", "message_delta", "message_start", "message_stop", "ping"},
		nil)
	assertAnthropicPartition(t, "deltas", spec.DeltaTypes,
		[]string{"citations_delta", "input_json_delta", "signature_delta", "text_delta", "thinking_delta"},
		nil)
	assertAnthropicPartition(t, "stop reasons", spec.StopReasons,
		[]string{"end_turn", "max_tokens", "model_context_window_exceeded", "pause_turn", "refusal", "stop_sequence", "tool_use"},
		nil)
	assertAnthropicPartition(t, "usage", spec.UsageProperties,
		[]string{"cache_creation", "cache_creation_input_tokens", "cache_read_input_tokens", "input_tokens", "output_tokens", "output_tokens_details", "service_tier", "speed"},
		[]string{"inference_geo", "server_tool_use"})
	assertAnthropicPartition(t, "cache creation details", spec.CacheCreationProperties,
		[]string{"ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens"},
		nil)
	assertAnthropicPartition(t, "output details", spec.OutputDetailProperties,
		[]string{"thinking_tokens"},
		nil)
	assertAnthropicPartition(t, "server-tool usage", spec.ServerToolUsageProperties,
		nil,
		[]string{"web_fetch_requests", "web_search_requests"})
}

func assertAnthropicPartition(t *testing.T, name string, all, supported, unsupported []string) {
	t.Helper()
	partition := append(append([]string(nil), supported...), unsupported...)
	got := append([]string(nil), all...)
	slices.Sort(got)
	slices.Sort(partition)
	if !slices.Equal(got, partition) {
		t.Fatalf("%s = %v, want %v", name, got, partition)
	}
	for i := 1; i < len(partition); i++ {
		if partition[i] == partition[i-1] {
			t.Fatalf("%s partition contains duplicate %q", name, partition[i])
		}
	}
}
