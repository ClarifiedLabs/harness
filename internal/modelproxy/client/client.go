package client

import (
	"bytes"
	"context"
	"encoding/json"
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

// OpenRouter normalizes reasoning controls for supported models.
// https://openrouter.ai/docs/api/reference/parameters
var openRouterEffortValues = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string, httpClient *http.Client) (*Client, error) {
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
	return &Client{baseURL: baseURL, http: httpClient}, nil
}

func (c *Client) URL() string { return c.baseURL }

func (c *Client) Catalog(ctx context.Context) (protocol.Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return protocol.Catalog{}, err
	}
	req.Header.Set(requesterHeader, "harness")
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

func (c *Client) Provider(provider string) llm.Provider {
	return &Provider{client: c, provider: provider}
}

// Registry builds a local model metadata registry from a proxy catalog.
func Registry(catalog protocol.Catalog) *llm.Registry {
	models := map[string]llm.ModelInfo{}
	qualified := map[string]llm.ModelInfo{}
	for _, provider := range catalog.Providers {
		for _, model := range provider.Models {
			if model.ID == "" {
				continue
			}
			info := llm.ModelInfo{
				ContextWindow: model.ContextWindow,
				OutputLimit:   model.OutputLimit,
				Price:         model.Price,
				Reasoning:     proxyModelReasoning(provider.ID, model),
			}
			if _, ok := models[model.ID]; !ok {
				models[model.ID] = info
			}
			if provider.ID != "" {
				qualified[provider.ID+":"+model.ID] = info
			}
		}
	}
	return llm.NewRegistryWithQualified(models, qualified)
}

func proxyModelReasoning(provider string, model protocol.Model) *llm.ReasoningInfo {
	info := model.Reasoning.Clone()
	if provider != "openrouter" || info == nil || !info.Supported {
		return info
	}
	next := info.Clone()
	if values, ok := next.EffortValues(); !ok || len(values) == 0 {
		ensureReasoningOption(&next.Options, llm.ReasoningOption{
			Type:   "effort",
			Values: append([]string(nil), openRouterEffortValues...),
		})
	}
	ensureReasoningOption(&next.Options, llm.ReasoningOption{Type: "toggle"})
	ensureReasoningOption(&next.Options, llm.ReasoningOption{Type: "budget_tokens"})
	return next
}

func ensureReasoningOption(options *[]llm.ReasoningOption, want llm.ReasoningOption) {
	for i, opt := range *options {
		if opt.Type != want.Type {
			continue
		}
		if want.Type == "effort" && len(opt.Values) == 0 {
			(*options)[i].Values = append([]string(nil), want.Values...)
		}
		return
	}
	*options = append(*options, want)
}

type Provider struct {
	client   *Client
	provider string
}

func (p *Provider) Name() string {
	if p.provider != "" {
		return p.provider
	}
	return "model-proxy"
}

func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		body, err := json.Marshal(protocol.StreamRequest{
			Provider: p.provider,
			Request:  req,
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
