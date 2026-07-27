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
	"harness/internal/metrics"
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

func newContinuationTestServer(t *testing.T, provider llm.Provider, registries ...*metrics.Registry) *httptest.Server {
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
	var registry *metrics.Registry
	if len(registries) > 0 {
		registry = registries[0]
	}
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"openai.json"}},
		Metrics:   registry,
		New: func(factory.Options) (llm.Provider, error) {
			return provider, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	return httptest.NewServer(handler)
}

func postStreamRequest(t *testing.T, srv *httptest.Server, request llm.Request) (*http.Response, []byte) {
	t.Helper()
	body, err := json.Marshal(protocol.StreamRequest{TargetID: "openai:gpt-5.5", Request: request})
	if err != nil {
		t.Fatalf("marshal stream request: %v", err)
	}
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return resp, data
}

func TestHandlerContinuationPassesClientStateUnchanged(t *testing.T) {
	fp := llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "resp-next"})
	srv := newContinuationTestServer(t, fp)
	defer srv.Close()

	messages := []llm.Message{{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "suffix"}},
	}}
	resp, body := postStreamRequest(t, srv, llm.Request{
		ProxySessionID:     "harness-session-one",
		CacheAffinityID:    "harness-cache-one",
		Messages:           messages,
		StoreResponse:      true,
		PreviousResponseID: "resp-old",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(fp.Requests))
	}
	got := fp.Requests[0]
	if !got.StoreResponse || got.PreviousResponseID != "resp-old" || len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "suffix" {
		t.Fatalf("provider continuation request = %+v", got)
	}
	if got.ProxySessionID != "" || got.CacheAffinityID != "" {
		t.Fatalf("harness-local identities reached provider: %+v", got)
	}
}

func TestHandlerInteractionsContinuationPassesClientStateUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "google.json"), []byte(`{
  "name": "google",
  "api_type": "interactions",
  "base_url": "https://generativelanguage.googleapis.com/v1beta",
  "api_key": "test-key",
  "models": [{"name":"gemini","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := llmtest.New("interactions", llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "interaction-next"})
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
	defer handler.Close()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	data, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "google:gemini",
		Request: llm.Request{
			StoreResponse:      true,
			PreviousResponseID: "interaction-old",
			Messages: []llm.Message{{
				Role:    llm.RoleUser,
				Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "suffix"}},
			}},
		},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", protocol.ContentTypeNDJSON, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(fp.Requests) != 1 ||
		!fp.Requests[0].StoreResponse ||
		fp.Requests[0].PreviousResponseID != "interaction-old" ||
		len(fp.Requests[0].Messages) != 1 ||
		fp.Requests[0].Messages[0].Content[0].Text != "suffix" {
		t.Fatalf("interactions continuation request = %+v", fp.Requests)
	}
}

func TestHandlerContinuationUnavailableReturns409BeforeStream(t *testing.T) {
	fp := &continuationAwareFakeProvider{FakeProvider: llmtest.New("responses")}
	srv := newContinuationTestServer(t, fp)
	defer srv.Close()

	resp, body := postStreamRequest(t, srv, llm.Request{
		ProxySessionID:     "harness-session-one",
		PreviousResponseID: "resp-on-other-pod",
		Messages:           []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "suffix"}}}},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var proxyErr protocol.Error
	if err := json.Unmarshal(body, &proxyErr); err != nil {
		t.Fatalf("decode error: %v body=%s", err, body)
	}
	if proxyErr.Code != protocol.ErrCodePreviousResponseUnavailable || proxyErr.Retryable {
		t.Fatalf("error = %+v", proxyErr)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider received %d requests before liveness rejection", len(fp.Requests))
	}
}

func TestHandlerUnsupportedContinuationIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chat.json"), []byte(`{
  "name": "chat",
  "api_type": "openai",
  "base_url": "https://example.test/v1",
  "api_key": "sk-test",
  "models": [{"name":"model","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	constructions := 0
	handler, err := NewHandler(Options{
		ConfigDir: dir,
		Config:    Config{ProviderConfigs: []string{"chat.json"}},
		New: func(factory.Options) (llm.Provider, error) {
			constructions++
			return llmtest.New("openai"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	data, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "chat:model",
		Request: llm.Request{
			StoreResponse: true,
			Messages:      []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}}},
		},
	})
	resp, err := srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var proxyErr protocol.Error
	if err := json.NewDecoder(resp.Body).Decode(&proxyErr); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || proxyErr.Code != protocol.ErrCodeContinuationUnsupported || constructions != 0 {
		t.Fatalf("status=%d error=%+v constructions=%d", resp.StatusCode, proxyErr, constructions)
	}
}

func TestHandlerUpstreamPreviousResponseRejectionPassesThrough(t *testing.T) {
	fp := llmtest.New("responses", llmtest.Step{Err: &llm.APIError{
		StatusCode: http.StatusBadRequest,
		Code:       "previous_response_not_found",
		Message:    "missing",
		Retryable:  false,
	}})
	srv := newContinuationTestServer(t, fp)
	defer srv.Close()

	resp, body := postStreamRequest(t, srv, llm.Request{
		PreviousResponseID: "resp-old",
		Messages:           []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "suffix"}}}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	found := false
	for {
		var envelope protocol.StreamEnvelope
		if err := dec.Decode(&envelope); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if envelope.Error != nil && envelope.Error.Code == "previous_response_not_found" {
			found = true
		}
	}
	if !found || len(fp.Requests) != 1 {
		t.Fatalf("upstream rejection not passed through: found=%t requests=%d body=%s", found, len(fp.Requests), body)
	}
}

func TestHandlerContinuationMetricsRecordOneBoundedResult(t *testing.T) {
	tests := []struct {
		name     string
		previous string
		provider func() llm.Provider
		want     string
	}{
		{
			name: "not offered",
			provider: func() llm.Provider {
				return llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn})
			},
			want: "not_offered",
		},
		{
			name:     "served",
			previous: "resp-old",
			provider: func() llm.Provider {
				return llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn})
			},
			want: "served",
		},
		{
			name:     "unavailable",
			previous: "resp-other-pod",
			provider: func() llm.Provider {
				return &continuationAwareFakeProvider{FakeProvider: llmtest.New("responses")}
			},
			want: "unavailable",
		},
		{
			name:     "rejected upstream",
			previous: "resp-old",
			provider: func() llm.Provider {
				return llmtest.New("responses", llmtest.Step{Err: &llm.APIError{
					Code:    "previous_response_not_found",
					Message: "missing",
				}})
			},
			want: "rejected_upstream",
		},
		{
			name:     "failed",
			previous: "resp-old",
			provider: func() llm.Provider {
				return llmtest.New("responses", llmtest.Step{Err: &llm.APIError{
					Code:    "server_error",
					Message: "boom",
				}})
			},
			want: "failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := metrics.New()
			srv := newContinuationTestServer(t, tc.provider(), reg)
			defer srv.Close()
			_, _ = postStreamRequest(t, srv, llm.Request{
				PreviousResponseID: tc.previous,
				Messages: []llm.Message{{
					Role:    llm.RoleUser,
					Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}},
				}},
			})
			var out strings.Builder
			reg.Render(&out)
			want := `model_proxy_continuation_total{result="` + tc.want + `"} 1`
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q:\n%s", want, out.String())
			}
			if strings.Count(out.String(), "model_proxy_continuation_total{") != 1 {
				t.Fatalf("continuation metric recorded more than one result:\n%s", out.String())
			}
		})
	}
}

func TestOperationalMetricLabelNormalizationIsBounded(t *testing.T) {
	if got := normalizeContinuationResult("client-controlled"); got != "failed" {
		t.Fatalf("continuation sentinel = %q", got)
	}
	if got := normalizeWSPoolEvent("client-controlled"); got != "unknown" {
		t.Fatalf("pool sentinel = %q", got)
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
	defer handler.Close()
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
