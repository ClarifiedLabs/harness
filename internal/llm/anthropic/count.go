package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"harness/internal/llm"
)

const countTokensPath = "/v1/messages/count_tokens"

type countRequest struct {
	Model        string          `json:"model"`
	System       []wireTextBlock `json:"system,omitempty"`
	Messages     []wireMessage   `json:"messages"`
	Tools        []wireTool      `json:"tools,omitempty"`
	OutputConfig *outputConfig   `json:"output_config,omitempty"`
	Thinking     *thinkingConfig `json:"thinking,omitempty"`
}

type countResponse struct {
	InputTokens int `json:"input_tokens"`
}

func (p *Provider) CountInputTokens(ctx context.Context, req llm.Request) (llm.InputTokenCount, error) {
	w := buildRequest(req, p.contextWindow, p.outputLimit)
	body, err := json.Marshal(countRequest{
		Model:        w.Model,
		System:       w.System,
		Messages:     w.Messages,
		Tools:        w.Tools,
		OutputConfig: w.OutputConfig,
		Thinking:     w.Thinking,
	})
	if err != nil {
		return llm.InputTokenCount{}, fmt.Errorf("marshal input token count request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+countTokensPath, bytes.NewReader(body))
	if err != nil {
		return llm.InputTokenCount{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	for k, v := range p.authHeaders {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("anthropic-version", apiVersion)
	if len(p.authHeaders) == 0 && p.apiKey != "" {
		httpReq.Header.Set("x-api-key", p.apiKey)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return llm.InputTokenCount{}, ctxErr
		}
		return llm.InputTokenCount{}, &llm.APIError{Message: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return llm.InputTokenCount{}, parseErrorResponse(resp)
	}
	var out countResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return llm.InputTokenCount{}, fmt.Errorf("decode input token count response: %w", err)
	}
	if out.InputTokens <= 0 {
		return llm.InputTokenCount{}, llm.ErrInputTokenCountUnsupported
	}
	return llm.InputTokenCount{InputTokens: out.InputTokens, Source: "anthropic"}, nil
}
