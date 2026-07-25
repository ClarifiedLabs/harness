package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
)

type continuationAwareFakeProvider struct {
	*llmtest.FakeProvider
	mu                  sync.Mutex
	availableResponseID string
}

func (p *continuationAwareFakeProvider) CanContinueResponse(responseID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return responseID != "" && responseID == p.availableResponseID
}

func (p *continuationAwareFakeProvider) setAvailableResponse(responseID string) {
	p.mu.Lock()
	p.availableResponseID = responseID
	p.mu.Unlock()
}

func newContinuationTestServer(t *testing.T, provider llm.Provider) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "responses",
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-test",
  "models": [{"name":"gpt-5.5","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			return provider, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return httptest.NewServer(handler)
}

func postContinuationStream(t *testing.T, srv *httptest.Server, messages []llm.Message) {
	t.Helper()
	body, err := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request: llm.Request{
			Model:          "openai:gpt-5.5",
			ProxySessionID: "continuation-test",
			Messages:       messages,
		},
	})
	if err != nil {
		t.Fatalf("marshal stream request: %v", err)
	}
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status = %d body=%s", resp.StatusCode, data)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestContinuationFingerprintIncludesNestedRichContent(t *testing.T) {
	base := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Kind:      llm.BlockToolUse,
			ToolUseID: "call",
			ToolName:  "view_image",
			ToolInput: json.RawMessage(`{"path":"screen.png"}`),
		}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Kind:        llm.BlockToolResult,
			ResultForID: "call",
			ResultText:  "attached",
			ResultContent: []llm.ContentBlock{{
				Kind:           llm.BlockImage,
				ImageMediaType: "image/png",
				ImageData:      serverOnePixelPNG,
			}},
		}}},
	}
	want, err := fingerprintMessages(base)
	if err != nil {
		t.Fatalf("fingerprint base: %v", err)
	}
	for _, mutate := range []func([]llm.Message){
		func(messages []llm.Message) { messages[1].Content[0].ResultText = "trimmed" },
		func(messages []llm.Message) { messages[1].Content[0].ResultContent = nil },
	} {
		changed := append([]llm.Message(nil), base...)
		changed[0].Content = append([]llm.ContentBlock(nil), base[0].Content...)
		changed[1].Content = append([]llm.ContentBlock(nil), base[1].Content...)
		changed[1].Content[0].ResultContent = append([]llm.ContentBlock(nil), base[1].Content[0].ResultContent...)
		mutate(changed)
		got, err := fingerprintMessages(changed)
		if err != nil {
			t.Fatalf("fingerprint changed: %v", err)
		}
		if got == want {
			t.Fatal("fingerprint ignored nested rich-content rewrite")
		}
	}
}

func TestHandlerContinuationHonorsExplicitAssistantPhase(t *testing.T) {
	fp := llmtest.New("responses",
		llmtest.Step{
			Events: []llm.StreamEvent{
				{Kind: llm.EventAssistantPhase, Phase: llm.AssistantPhaseCommentary},
				{Kind: llm.EventTextDelta, Text: "working"},
			},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp-phase",
		},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp-done"},
	)
	srv := newContinuationTestServer(t, fp)
	defer srv.Close()

	first := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}}}
	postContinuationStream(t, srv, first)
	second := append([]llm.Message(nil), first...)
	second = append(second,
		llm.BuildAssistantMessage(nil, "working", nil, llm.AssistantPhaseCommentary, llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "next"}}},
	)
	postContinuationStream(t, srv, second)

	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	if fp.Requests[1].PreviousResponseID != "resp-phase" || len(fp.Requests[1].Messages) != 1 {
		t.Fatalf("continued request = prev %q messages %d", fp.Requests[1].PreviousResponseID, len(fp.Requests[1].Messages))
	}
}

func TestHandlerContinuationResendsFullHistoryWhenProviderConnectionClosed(t *testing.T) {
	fp := &continuationAwareFakeProvider{FakeProvider: llmtest.New("responses",
		llmtest.Step{
			Events:     []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "first answer"}},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp-closed",
		},
		llmtest.Step{
			Events:     []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "second answer"}},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp-live",
		},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp-third"},
	)}
	srv := newContinuationTestServer(t, fp)
	defer srv.Close()

	first := []llm.Message{{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}},
	}}
	postContinuationStream(t, srv, first)

	second := append([]llm.Message(nil), first...)
	second = append(second,
		llm.BuildAssistantMessage(nil, "first answer", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second"}}},
	)
	postContinuationStream(t, srv, second)
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(fp.Requests))
	}
	if fp.Requests[1].PreviousResponseID != "" || len(fp.Requests[1].Messages) != len(second) {
		t.Fatalf(
			"closed-connection request = prev %q messages %d, want full history with %d messages",
			fp.Requests[1].PreviousResponseID,
			len(fp.Requests[1].Messages),
			len(second),
		)
	}

	fp.setAvailableResponse("resp-live")
	third := append([]llm.Message(nil), second...)
	third = append(third,
		llm.BuildAssistantMessage(nil, "second answer", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "third"}}},
	)
	postContinuationStream(t, srv, third)
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(fp.Requests))
	}
	if fp.Requests[2].PreviousResponseID != "resp-live" || len(fp.Requests[2].Messages) != 1 {
		t.Fatalf(
			"live-connection request = prev %q messages %d, want resp-live and one tail message",
			fp.Requests[2].PreviousResponseID,
			len(fp.Requests[2].Messages),
		)
	}
}

func TestHandlerInteractionsContinuationIncludesSignedProviderState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "google.json"), []byte(`{
  "name": "google",
  "api_type": "interactions",
  "base_url": "https://generativelanguage.googleapis.com/v1beta",
  "api_key": "gemini-key",
  "models": [{"name":"gemini-3.6-flash","context_window":1000000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	thoughtEvent := llm.StreamEvent{
		Kind:            llm.EventReasoningSummary,
		ReasoningFormat: llm.ReasoningFormatGeminiInteractions,
		Text:            "searching",
		Signature:       "thought-sig",
	}
	searchEvent := llm.StreamEvent{
		Kind:            llm.EventInteractionStep,
		InteractionStep: json.RawMessage(`{"type":"google_search_call","id":"search-1","signature":"search-sig"}`),
	}
	fp := llmtest.New("interactions",
		llmtest.Step{
			Events:     []llm.StreamEvent{thoughtEvent, searchEvent, {Kind: llm.EventTextDelta, Text: "answer"}},
			Stop:       llm.StopEndTurn,
			ResponseID: "interaction-1",
		},
		llmtest.Step{Err: &llm.APIError{Code: "previous_interaction_not_found", Message: "previous_interaction_id is invalid"}},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "interaction-2"},
	)
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"google.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			return fp, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	first := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}}}
	postTargetContinuationStream(t, srv, "google:gemini-3.6-flash", first)
	reasoning := []llm.ContentBlock{
		{
			Kind:                        llm.BlockInteractionThought,
			InteractionThoughtSummary:   "searching",
			InteractionThoughtSignature: "thought-sig",
		},
		{
			Kind:            llm.BlockInteractionStep,
			InteractionStep: json.RawMessage(`{"type":"google_search_call","id":"search-1","signature":"search-sig"}`),
		},
	}
	second := append([]llm.Message(nil), first...)
	second = append(second,
		llm.BuildAssistantMessage(reasoning, "answer", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "next"}}},
	)
	postTargetContinuationStream(t, srv, "google:gemini-3.6-flash", second)

	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(fp.Requests))
	}
	if !fp.Requests[0].StoreResponse {
		t.Fatal("first Interactions request should store state")
	}
	if fp.Requests[1].PreviousResponseID != "interaction-1" || len(fp.Requests[1].Messages) != 1 {
		t.Fatalf("continued request = prev %q messages %d", fp.Requests[1].PreviousResponseID, len(fp.Requests[1].Messages))
	}
	if fp.Requests[2].PreviousResponseID != "" || fp.Requests[2].StoreResponse || len(fp.Requests[2].Messages) != len(second) {
		t.Fatalf("stateless retry = prev %q store=%v messages=%d, want full signed history",
			fp.Requests[2].PreviousResponseID, fp.Requests[2].StoreResponse, len(fp.Requests[2].Messages))
	}
	if got := fp.Requests[2].Messages[1].Content; len(got) < 2 ||
		got[0].Kind != llm.BlockInteractionThought ||
		got[1].Kind != llm.BlockInteractionStep {
		t.Fatalf("stateless retry interaction state = %+v", got)
	}
}

func postTargetContinuationStream(t *testing.T, srv *httptest.Server, target string, messages []llm.Message) {
	t.Helper()
	body, err := json.Marshal(protocol.StreamRequest{
		TargetID: target,
		Request: llm.Request{
			Model:          target,
			ProxySessionID: "continuation-test",
			Messages:       messages,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status = %d body=%s", resp.StatusCode, data)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestHandlerContinuationMismatchResendsAndEstablishesFreshAnchor(t *testing.T) {
	fp := llmtest.New("responses",
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp-original"},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp-fresh"},
		llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp-third"},
	)
	srv := newContinuationTestServer(t, fp)
	defer srv.Close()

	first := []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: serverOnePixelPNG},
			{Kind: llm.BlockText, Text: "inspect"},
		},
	}}
	postContinuationStream(t, srv, first)

	rewritten := []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockText, Text: "[image omitted by retention]"},
			{Kind: llm.BlockText, Text: "inspect"},
		},
	}}
	rewritten = append(rewritten,
		llm.BuildAssistantMessage(nil, "", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second"}}},
	)
	postContinuationStream(t, srv, rewritten)
	if fp.Requests[1].PreviousResponseID != "" || len(fp.Requests[1].Messages) != len(rewritten) {
		t.Fatalf("mismatch request = prev %q messages %d, want full %d", fp.Requests[1].PreviousResponseID, len(fp.Requests[1].Messages), len(rewritten))
	}

	third := append([]llm.Message(nil), rewritten...)
	third = append(third,
		llm.BuildAssistantMessage(nil, "", nil, "", llm.StopEndTurn),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "third"}}},
	)
	postContinuationStream(t, srv, third)
	if fp.Requests[2].PreviousResponseID != "resp-fresh" || len(fp.Requests[2].Messages) != 1 {
		t.Fatalf("fresh continuation = prev %q messages %d", fp.Requests[2].PreviousResponseID, len(fp.Requests[2].Messages))
	}
}

func TestHandlerRejectsInvalidRichContentBeforeProviderOrCounter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "responses",
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-test",
  "models": [{"name":"gpt-5.5","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	constructions := 0
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			constructions++
			return &countingFakeProvider{FakeProvider: llmtest.New("responses"), count: 10}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	invalid := llm.Request{
		Model: "openai:gpt-5.5",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Kind:           llm.BlockImage,
				ImageMediaType: "image/png",
				ImageData:      "not-base64",
			}},
		}},
	}
	for _, tc := range []struct {
		path string
		body any
	}{
		{path: "/v1/stream", body: protocol.StreamRequest{TargetID: "openai:gpt-5.5", Request: invalid}},
		{path: "/v1/input_tokens", body: protocol.TokenCountRequest{TargetID: "openai:gpt-5.5", Request: invalid}},
	} {
		data, err := json.Marshal(tc.body)
		if err != nil {
			t.Fatalf("marshal %s: %v", tc.path, err)
		}
		resp, err := srv.Client().Post(srv.URL+tc.path, "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("POST %s: %v", tc.path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", tc.path, resp.StatusCode)
		}
	}
	if constructions != 0 {
		t.Fatalf("provider constructions = %d, want 0", constructions)
	}
}

func TestHandlerPlainImageStreamErrorRemainsRetryableOnWire(t *testing.T) {
	fp := llmtest.New("responses", llmtest.Step{Err: errors.New("truncated image stream " + serverOnePixelPNG)})
	srv := newContinuationTestServer(t, fp)
	defer srv.Close()

	body, err := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request: llm.Request{
			Model:          "openai:gpt-5.5",
			ProxySessionID: "retryable-error-test",
			Messages:       richImageRequest(serverOnePixelPNG).Messages,
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 stream envelope", resp.StatusCode)
	}
	envelope := decodeStreamError(t, resp.Body)
	if envelope.Error == nil || !envelope.Error.Retryable {
		t.Fatalf("stream error = %+v, want retryable", envelope.Error)
	}
	apiErr := envelope.Error.APIError()
	if !apiErr.Retryable || apiErr.Diagnostic == nil || strings.Contains(apiErr.Error(), serverOnePixelPNG) {
		t.Fatalf("reconstructed APIError = %+v", apiErr)
	}
}
