package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"harness/internal/llm"
)

const inputTokensPath = "/responses/input_tokens"

type countRequest struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions,omitempty"`
	Input              []wireInputItem `json:"input"`
	Tools              []wireTool      `json:"tools,omitempty"`
	Reasoning          *wireReasoning  `json:"reasoning,omitempty"`
	Store              bool            `json:"store,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
}

type countResponse struct {
	InputTokens int `json:"input_tokens"`
}

func (p *Provider) CountInputTokens(ctx context.Context, req llm.Request) (llm.InputTokenCount, error) {
	w := buildRequestWithOptions(req, p.contextWindow, p.outputLimit, true)
	body, err := json.Marshal(countRequest{
		Model:              w.Model,
		Instructions:       w.Instructions,
		Input:              w.Input,
		Tools:              w.Tools,
		Reasoning:          w.Reasoning,
		Store:              w.Store,
		PreviousResponseID: w.PreviousResponseID,
	})
	if err != nil {
		return llm.InputTokenCount{}, fmt.Errorf("marshal input token count request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+inputTokensPath, bytes.NewReader(body))
	if err != nil {
		return llm.InputTokenCount{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	for k, v := range p.authHeaders {
		httpReq.Header.Set(k, v)
	}
	if len(p.authHeaders) == 0 && p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
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
	return llm.InputTokenCount{InputTokens: out.InputTokens, Source: "responses"}, nil
}
