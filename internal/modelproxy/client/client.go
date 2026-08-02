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
	"harness/internal/tracing"
)

const maxErrorBodyBytes = 1 << 20

const requesterHeader = "X-Harness-Requester"

type Client struct {
	baseURL string
	http    *http.Client
	apiKey  string
	tracer  *tracing.Tracer
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the API key sent on every request as Authorization: Bearer.
func WithAPIKey(key string) Option {
	return func(c *Client) {
		c.apiKey = key
	}
}

// WithTracer enables W3C Trace Context headers on outbound proxy requests.
func WithTracer(t *tracing.Tracer) Option {
	return func(c *Client) {
		c.tracer = t
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

func (c *Client) setTrace(req *http.Request) {
	if c.tracer == nil {
		return
	}
	ctx, tc, err := c.tracer.Start(req.Context())
	if err != nil {
		return
	}
	tracing.Inject(req.Header, tc)
	*req = *req.WithContext(ctx)
}

func (c *Client) URL() string { return c.baseURL }

func (c *Client) Catalog(ctx context.Context) (protocol.Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return protocol.Catalog{}, err
	}
	req.Header.Set(requesterHeader, "harness")
	c.setAuth(req)
	c.setTrace(req)
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
	if !target.Reasoning {
		return &llm.ReasoningInfo{Supported: false}
	}
	return &llm.ReasoningInfo{
		Supported: true,
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
		if req.ProxySessionID != "" {
			httpReq.Header.Set("X-Harness-Session", req.ProxySessionID)
		}
		p.client.setAuth(httpReq)
		p.client.setTrace(httpReq)

		resp, err := p.client.http.Do(httpReq)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				yield(modelRequestEventFromClientError(ctxErr, llm.ModelRequestCancelled), nil)
				yield(llm.StreamEvent{}, ctxErr)
				return
			}
			apiErr := &llm.APIError{Message: err.Error(), Retryable: true}
			yield(modelRequestEventFromClientError(apiErr, llm.ModelRequestFailed), nil)
			yield(llm.StreamEvent{}, apiErr)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			apiErr := readHTTPError(resp)
			yield(modelRequestEventFromClientError(apiErr, llm.ModelRequestFailed), nil)
			yield(llm.StreamEvent{}, apiErr)
			return
		}

		dec := json.NewDecoder(resp.Body)
		terminalEventSeen := false
		for {
			var env protocol.StreamEnvelope
			if err := dec.Decode(&env); err != nil {
				if err == io.EOF {
					return
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					if !terminalEventSeen {
						yield(modelRequestEventFromClientError(ctxErr, llm.ModelRequestCancelled), nil)
					}
					yield(llm.StreamEvent{}, ctxErr)
					return
				}
				apiErr := &llm.APIError{Message: "decode proxy stream: " + err.Error(), Retryable: true}
				if !terminalEventSeen {
					yield(modelRequestEventFromClientError(apiErr, llm.ModelRequestFailed), nil)
				}
				yield(llm.StreamEvent{}, apiErr)
				return
			}
			if env.Error != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					if !terminalEventSeen {
						yield(modelRequestEventFromClientError(ctxErr, llm.ModelRequestCancelled), nil)
					}
					yield(llm.StreamEvent{}, ctxErr)
					return
				}
				apiErr := env.Error.APIError()
				if !terminalEventSeen {
					yield(modelRequestEventFromClientError(apiErr, llm.ModelRequestFailed), nil)
				}
				yield(llm.StreamEvent{}, apiErr)
				return
			}
			if env.Event != nil {
				if env.Event.Kind == llm.EventModelRequest && env.Event.ModelRequest != nil {
					switch env.Event.ModelRequest.State {
					case llm.ModelRequestFailed, llm.ModelRequestCancelled:
						terminalEventSeen = true
					case llm.ModelRequestUpstreamAttemptFailed:
						terminalEventSeen = env.Event.ModelRequest.Outcome == llm.ModelRequestOutcomeTerminal
					}
				}
				if !yield(*env.Event, nil) {
					return
				}
			}
		}
	}
}

func modelRequestEventFromClientError(err error, state llm.ModelRequestState) llm.StreamEvent {
	event := &llm.ModelRequestEvent{
		State:   state,
		Outcome: llm.ModelRequestOutcomeTerminal,
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		event.StatusCode = apiErr.StatusCode
		event.Code = apiErr.Code
		event.Message = apiErr.Message
		event.ResponsePayload = apiErr.ResponsePayload
		event.Retryable = apiErr.Retryable
		event.RetryAfterMS = apiErr.RetryAfter.Milliseconds()
		if apiErr.Diagnostic != nil {
			event.Stage = apiErr.Diagnostic.Stage
			event.ProxyInstanceID = apiErr.Diagnostic.ProxyInstanceID
			event.ProxyRequestID = apiErr.Diagnostic.ProxyRequestID
			event.UpstreamRequestID = apiErr.Diagnostic.UpstreamRequestID
			event.TraceID = apiErr.Diagnostic.TraceID
			event.SpanID = apiErr.Diagnostic.SpanID
			event.TargetID = apiErr.Diagnostic.TargetID
			event.Provider = apiErr.Diagnostic.Provider
			event.APIType = apiErr.Diagnostic.APIType
			event.Model = apiErr.Diagnostic.Model
		}
	} else if err != nil {
		event.Message = err.Error()
	}
	return llm.StreamEvent{Kind: llm.EventModelRequest, ModelRequest: event}
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
	p.client.setTrace(httpReq)
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
	return llm.InputTokenCount{
		InputTokens: out.InputTokens,
		Source:      out.Source,
		Scope:       llm.NormalizeInputTokenCountScope(out.Scope),
	}, nil
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
	return &llm.APIError{
		StatusCode:      resp.StatusCode,
		Message:         msg,
		ResponsePayload: llm.SafeResponsePayload(body),
	}
}
