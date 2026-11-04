package runtime

import (
	"context"

	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/llm/tokencount"
	"harness/internal/modelproxy"
	"io"
	"iter"
	"strings"
	"time"
)

var _ modelproxy.Backend = (*Runtime)(nil)
var _ modelproxy.Accounts = (*Runtime)(nil)

// Provider is a target-bound handle. Connections are acquired per execution,
// from the runtime-owned pool, rather than pinned to this handle.
type Provider struct {
	runtime  *Runtime
	targetID string
}

func (h *Runtime) Provider(target string) llm.Provider { return &Provider{h, target} }
func (p *Provider) Name() string                       { return p.targetID }
func callerAttempt(ctx context.Context) *modelproxy.CallerAttempt {
	m := llm.AttemptMetadataFromContext(ctx)
	a := (modelproxy.CallerAttempt{Cause: m.Cause, RetryLayer: m.RetryLayer}).Normalized()
	return &a
}

// Stream keeps yield on the iterator caller's goroutine. The execution worker
// is joined even when the consumer breaks or panics, and cancellation leaves
// the runtime itself alive for later turns.
func (p *Provider) Stream(ctx context.Context, request llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		child, cancel := context.WithCancel(ctx)
		type delivery struct {
			frame streamFrame
			ack   chan error
		}
		frames := make(chan delivery)
		workerPanic := make(chan any, 1)
		owned := modelproxy.CloneRequest(request)
		go func() {
			defer func() { workerPanic <- recover(); close(frames) }()
			p.runtime.stream(child, streamRequest{TargetID: p.targetID, Request: owned, ReasoningProfile: owned.Reasoning.Profile, CallerAttempt: callerAttempt(ctx)}, func(f streamFrame) error {
				ack := make(chan error, 1)
				frames <- delivery{f, ack}
				return <-ack
			})
		}()
		defer func() {
			cancel()
			// A consumer break rejects further logical events but still receives the
			// source's final physical facts and releases its provider lease.
			for d := range frames {
				if d.frame.Attempt != nil {
					llm.EmitAttempt(ctx, *d.frame.Attempt)
				}
				d.ack <- io.ErrClosedPipe
			}
			if value := <-workerPanic; value != nil {
				panic(value)
			}
		}()
		var terminalError error
		consume := func(d delivery) bool {
			var ack error
			defer func() { d.ack <- ack }()
			if d.frame.Attempt != nil {
				llm.EmitAttempt(ctx, *d.frame.Attempt)
			}
			if d.frame.Error != nil {
				terminalError = d.frame.Error
				return true
			}
			if d.frame.Event != nil {
				// If yield panics, unblock the worker before joining it in the outer
				// cleanup. The panic then propagates normally on the caller goroutine.
				ack = io.ErrClosedPipe
				if !yield(*d.frame.Event, nil) {
					return false
				}
				ack = nil
			}
			return true
		}
		for d := range frames {
			if !consume(d) {
				return
			}
		}
		if terminalError != nil {
			if ctx.Err() != nil {
				terminalError = ctx.Err()
			}
			yield(llm.StreamEvent{}, terminalError)
		}
	}
}
func (p *Provider) Steer(ctx context.Context, s llm.SteerSubmission) error {
	s.Messages = llm.CloneMessages(s.Messages)
	return p.runtime.steer(ctx, p.targetID, s)
}
func invalidRequest(err error) error {
	if err == nil {
		return nil
	}
	return &llm.APIError{StatusCode: 400, Code: "invalid_request", Message: "invalid model request content"}
}
func (p *Provider) CountInputTokens(ctx context.Context, request llm.Request) (llm.InputTokenCount, error) {
	req := modelproxy.CloneRequest(request)
	if err := invalidRequest(validateProxyRequestMessages(req)); err != nil {
		return llm.InputTokenCount{}, err
	}
	target, err := p.runtime.resolveTarget(strings.TrimSpace(p.targetID))
	if err != nil {
		return llm.InputTokenCount{}, &llm.APIError{StatusCode: 400, Message: err.Error()}
	}
	if err := applyServiceTierForTarget(target, &req); err != nil {
		return llm.InputTokenCount{}, err
	}
	req.Model = target.entry.Name
	req.ServerTools = resolveServerToolsForTarget(target, req.ServerTools)
	_, req = prepareProviderRequest(p.runtime.mapReasoningStates(target, req))
	if codexResponsesUsesLocalTokenCount(target.pc) {
		n := tokencount.EstimateOpenAIChat(req)
		if n <= 0 {
			return llm.InputTokenCount{}, llm.ErrInputTokenCountUnsupported
		}
		scope := llm.InputTokenCountScopeEffectiveContext
		if req.PreviousResponseID != "" {
			scope = llm.InputTokenCountScopeRequestPayload
		}
		return llm.InputTokenCount{InputTokens: n, Source: "o200k_base", Scope: scope}, nil
	}
	opts, err := p.runtime.runtimeOptionsForTarget(ctx, target)
	if err != nil {
		return llm.InputTokenCount{}, err
	}
	provider, err := p.runtime.newProvider(opts)
	if err != nil {
		return llm.InputTokenCount{}, err
	}
	defer closeProvider(provider)
	counter, ok := provider.(llm.InputTokenCounter)
	if !ok {
		return llm.InputTokenCount{}, llm.ErrInputTokenCountUnsupported
	}
	count, err := counter.CountInputTokens(ctx, req)
	count.Scope = llm.NormalizeInputTokenCountScope(count.Scope)
	if err == nil && count.InputTokens <= 0 {
		err = llm.ErrInputTokenCountUnsupported
	}
	return count, err
}
func (p *Provider) CompactContext(ctx context.Context, request llm.Request) (result llm.CompactedContext, err error) {
	h := p.runtime
	start := time.Now()
	var providerID, model string
	defer func() {
		h.recordMetrics(ctx, providerID, model, llm.RequestPurposeCompaction, result.Usage, time.Since(start), err != nil)
	}()
	req := modelproxy.CloneRequest(request)
	if err = invalidRequest(validateProxyRequestMessages(req)); err != nil {
		return result, err
	}
	target, e := h.resolveTarget(p.targetID)
	if e != nil {
		return result, &llm.APIError{StatusCode: 400, Message: e.Error()}
	}
	providerID = target.pc.Name
	model = target.entry.Name
	if !providerResponsesCompaction(target.pc) {
		return result, llm.ErrContextCompactionUnsupported
	}
	if err = applyServiceTierForTarget(target, &req); err != nil {
		return result, err
	}
	req.Model = model
	req.Purpose = llm.RequestPurposeCompaction
	req.ServerTools = resolveServerToolsForTarget(target, req.ServerTools)
	req.Reasoning = h.reasoningForTarget(target, req.Reasoning.Profile, req.Reasoning)
	_, req = prepareProviderRequest(h.mapReasoningStates(target, req))
	budget, be := h.checkCostBudget(ctx, target, req, nil)
	if be != nil {
		return result, be
	}
	opts, e := h.runtimeOptionsForTarget(ctx, target)
	if e != nil {
		return result, e
	}
	provider, e := h.newProvider(opts)
	if e != nil {
		return result, e
	}
	defer closeProvider(provider)
	compactor, ok := provider.(llm.ContextCompactor)
	if !ok {
		return result, llm.ErrContextCompactionUnsupported
	}
	observed := llm.WithAttemptObserver(ctx, llm.AttemptObserverFunc(func(event llm.AttemptEvent) { llm.EmitAttempt(ctx, h.priceAttempt(target, req, event)) }))
	observed = withTargetAttemptMetadata(observed, target, opts.Provider, req.Purpose, callerAttempt(ctx))
	result, err = compactor.CompactContext(observed, req)
	result.Items = llm.CloneRawMessages(result.Items)
	if err != nil {
		err = &modelproxy.UpstreamError{Err: err}
	}
	if !result.Usage.CostKnown && result.Usage.CostUSD == 0 {
		result.Usage = h.priceUsage(target.targetID, req, result.Usage)
	}
	if result.Usage.CostKnown {
		h.recordUsage(target.targetID, result.Usage, result.Usage.CostUSD)
		if budget != nil {
			if e := budget.Add(result.Usage.CostUSD); e != nil {
				h.logger.Warn("persist api key cost budget state failed", "err", e)
			}
		}
	}
	return result, err
}

var _ llm.InputTokenCounter = (*Provider)(nil)
var _ llm.ContextCompactor = (*Provider)(nil)
var _ llm.LiveSteerer = (*Provider)(nil)

func validateProxyRequestMessages(req llm.Request) error {
	if _, err := inputimage.ValidateMessages(req.Messages); err != nil {
		return err
	}
	if req.PreviousResponseID != "" {
		return llm.ValidateMessageContent(req.Messages)
	}
	return llm.ValidateTranscript(req.Messages)
}
