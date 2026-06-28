package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

const maxErrorBodyBytes = 1 << 20

const requesterHeader = "X-Harness-Requester"

type Client struct {
	baseURL string
	http    *http.Client
	apiKey  string
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the API key sent on every request as Authorization: Bearer.
func WithAPIKey(key string) Option {
	return func(c *Client) {
		c.apiKey = key
	}
}

func New(baseURL string, httpClient *http.Client, opts ...Option) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = protocol.DefaultURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("model proxy URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("model proxy URL %q must use http or https", baseURL)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	c := &Client{baseURL: baseURL, http: httpClient}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

func (c *Client) URL() string { return c.baseURL }

func (c *Client) Catalog(ctx context.Context) (protocol.Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return protocol.Catalog{}, err
	}
	req.Header.Set(requesterHeader, "harness")
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return protocol.Catalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protocol.Catalog{}, readHTTPError(resp)
	}
	var catalog protocol.Catalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return protocol.Catalog{}, fmt.Errorf("model proxy catalog: %w", err)
	}
	return catalog, nil
}

func (c *Client) Provider(targetID string) llm.Provider {
	return &Provider{client: c, targetID: targetID}
}

// Registry builds a local model metadata registry from a proxy catalog.
func Registry(catalog protocol.Catalog) *llm.Registry {
	models := map[string]llm.ModelInfo{}
	for _, target := range catalog.Targets {
		if target.ID == "" {
			continue
		}
		info := llm.ModelInfo{
			ContextWindow:   target.ContextWindow,
			OutputLimit:     target.OutputLimit,
			InputModalities: append([]string(nil), target.InputModalities...),
			ServerTools:     llm.NormalizeServerTools(target.ServerTools),
			Price:           target.Price,
			Reasoning:       proxyTargetReasoning(target),
		}
		models[target.ID] = info
		for _, alias := range target.Aliases {
			if alias != "" {
				models[alias] = info
			}
		}
	}
	return llm.NewRegistry(models)
}

func proxyTargetReasoning(target protocol.Target) *llm.ReasoningInfo {
	if target.Reasoning == nil || !target.Reasoning.Supported {
		return &llm.ReasoningInfo{Supported: false}
	}
	values := append([]string(nil), target.Reasoning.Profiles...)
	if len(values) == 0 {
		values = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	}
	return &llm.ReasoningInfo{
		Supported: true,
		Options:   []llm.ReasoningOption{{Type: "effort", Values: values}},
	}
}

type Provider struct {
	client   *Client
	targetID string
}

func (p *Provider) Name() string {
	if p.targetID != "" {
		return p.targetID
	}
	return "model-proxy"
}

func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		profile := req.Reasoning.Profile
		if profile == "" {
			profile = req.Reasoning.Effort
		}
		body, err := json.Marshal(protocol.StreamRequest{
			TargetID:         p.targetID,
			Request:          req,
			ReasoningProfile: profile,
		})
		if err != nil {
			yield(llm.StreamEvent{}, &llm.APIError{Message: "marshal proxy request: " + err.Error()})
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.client.baseURL+"/v1/stream", bytes.NewReader(body))
		if err != nil {
			yield(llm.StreamEvent{}, &llm.APIError{Message: "build proxy request: " + err.Error()})
			return
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("accept", protocol.ContentTypeNDJSON)
		httpReq.Header.Set(requesterHeader, "harness")
		p.client.setAuth(httpReq)

		resp, err := p.client.http.Do(httpReq)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				yield(llm.StreamEvent{}, ctxErr)
				return
			}
			yield(llm.StreamEvent{}, &llm.APIError{Message: err.Error(), Retryable: true})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			yield(llm.StreamEvent{}, readHTTPError(resp))
			return
		}

		dec := json.NewDecoder(resp.Body)
		for {
			var env protocol.StreamEnvelope
			if err := dec.Decode(&env); err != nil {
				if err == io.EOF {
					return
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					yield(llm.StreamEvent{}, ctxErr)
					return
				}
				yield(llm.StreamEvent{}, &llm.APIError{Message: "decode proxy stream: " + err.Error(), Retryable: true})
				return
			}
			if env.Error != nil {
				yield(llm.StreamEvent{}, env.Error.APIError())
				return
			}
			if env.Event != nil {
				if !yield(*env.Event, nil) {
					return
				}
			}
		}
	}
}

func (p *Provider) CountInputTokens(ctx context.Context, req llm.Request) (llm.InputTokenCount, error) {
	body, err := json.Marshal(protocol.TokenCountRequest{
		TargetID: p.targetID,
		Request:  req,
	})
	if err != nil {
		return llm.InputTokenCount{}, fmt.Errorf("marshal proxy input token request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.client.baseURL+"/v1/input_tokens", bytes.NewReader(body))
	if err != nil {
		return llm.InputTokenCount{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set(requesterHeader, "harness")
	p.client.setAuth(httpReq)
	resp, err := p.client.http.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return llm.InputTokenCount{}, ctxErr
		}
		return llm.InputTokenCount{}, &llm.APIError{Message: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := readHTTPError(resp)
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "input_token_count_unsupported" {
			return llm.InputTokenCount{}, llm.ErrInputTokenCountUnsupported
		}
		return llm.InputTokenCount{}, err
	}
	var out protocol.TokenCountResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return llm.InputTokenCount{}, fmt.Errorf("decode proxy input token response: %w", err)
	}
	if out.InputTokens <= 0 {
		return llm.InputTokenCount{}, llm.ErrInputTokenCountUnsupported
	}
	return llm.InputTokenCount{InputTokens: out.InputTokens, Source: out.Source}, nil
}

func readHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var env protocol.StreamEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		return env.Error.APIError()
	}
	var wireErr protocol.Error
	if json.Unmarshal(body, &wireErr) == nil && wireErr.Message != "" {
		return wireErr.APIError()
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return &llm.APIError{StatusCode: resp.StatusCode, Message: msg}
}
