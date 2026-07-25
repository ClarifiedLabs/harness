package openai

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
		Streaming apiSurfaceSource
	} `json:"source"`
	RequestProperties          []string `json:"request_properties"`
	MessageRoles               []string `json:"message_roles"`
	InputContentTypes          []string `json:"input_content_types"`
	ToolTypes                  []string `json:"tool_types"`
	ChunkProperties            []string `json:"chunk_properties"`
	DeltaProperties            []string `json:"delta_properties"`
	FinishReasons              []string `json:"finish_reasons"`
	UsageProperties            []string `json:"usage_properties"`
	PromptDetailProperties     []string `json:"prompt_detail_properties"`
	CompletionDetailProperties []string `json:"completion_detail_properties"`
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
		spec.Source.Create.URL != "https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create" ||
		spec.Source.Streaming.URL != "https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events" ||
		spec.Source.Create.SHA256 == "" || spec.Source.Streaming.SHA256 == "" {
		t.Fatalf("incomplete API source metadata: %+v", spec.Source)
	}

	assertContractPartition(t, "request properties", spec.RequestProperties,
		[]string{"max_completion_tokens", "max_tokens", "messages", "model", "parallel_tool_calls", "prompt_cache_key", "reasoning_effort", "service_tier", "stop", "stream", "stream_options", "temperature", "tools"},
		[]string{"audio", "frequency_penalty", "function_call", "functions", "logit_bias", "logprobs", "metadata", "modalities", "moderation", "n", "prediction", "presence_penalty", "prompt_cache_options", "prompt_cache_retention", "response_format", "safety_identifier", "seed", "store", "tool_choice", "top_logprobs", "top_p", "user", "verbosity", "web_search_options"})
	assertContractPartition(t, "message roles", spec.MessageRoles,
		[]string{"assistant", "system", "tool", "user"},
		[]string{"developer", "function"})
	assertContractPartition(t, "input content", spec.InputContentTypes,
		[]string{"image_url", "text"},
		[]string{"file", "input_audio"})
	assertContractPartition(t, "tools", spec.ToolTypes,
		[]string{"function"},
		[]string{"custom"})
	assertContractPartition(t, "chunk properties", spec.ChunkProperties,
		[]string{"choices", "service_tier", "usage"},
		[]string{"created", "id", "model", "object", "system_fingerprint"})
	assertContractPartition(t, "delta properties", spec.DeltaProperties,
		[]string{"content", "refusal", "role", "tool_calls"},
		[]string{"audio", "function_call"})
	assertContractPartition(t, "finish reasons", spec.FinishReasons,
		[]string{"content_filter", "length", "stop", "tool_calls"},
		[]string{"function_call"})
	assertContractPartition(t, "usage", spec.UsageProperties,
		[]string{"completion_tokens", "completion_tokens_details", "prompt_tokens", "prompt_tokens_details"},
		[]string{"total_tokens"})
	assertContractPartition(t, "prompt details", spec.PromptDetailProperties,
		[]string{"cached_tokens"},
		[]string{"audio_tokens"})
	assertContractPartition(t, "completion details", spec.CompletionDetailProperties,
		[]string{"reasoning_tokens"},
		[]string{"accepted_prediction_tokens", "audio_tokens", "rejected_prediction_tokens"})
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
