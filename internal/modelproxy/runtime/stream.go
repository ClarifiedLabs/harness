package runtime

import (
	"context"
	"errors"
	"harness/internal/llm"
	"harness/internal/modelproxy"
	"harness/internal/tracing"
	"net/http"
	"strings"
	"time"
)

func (h *Runtime) stream(ctx context.Context, req streamRequest, deliver func(streamFrame) error) {
	start := time.Now()
	traceCtx, traceOK := tracing.TraceFromContext(ctx)
	requestID := h.nextRequestID.Add(1)
	statusCode := 200
	writeError := func(_ int, e *llm.APIError) { _ = deliver(streamFrame{Error: e}) }
	var (
		targetID           string
		providerID         string
		apiType            string
		model              string
		purpose            = llm.RequestPurposeUnknown
		usage              llm.Usage
		stop               llm.StopReason
		streamErr          string
		events             int
		toolCalls          int
		reqBytes           = modelproxy.ExecutionFromContext(ctx).RequestBytes
		errAttrs           []any
		budget             *costBudgetTracker
		requestShape       *llm.MultimodalRequestShape
		finalDiagnostic    *llm.APIErrorDiagnostic
		eventSequence      int
		continuationResult = "not_offered"
	)
	diagnosticFor := func(stage llm.APIErrorStage) *llm.APIErrorDiagnostic {
		diagnostic := &llm.APIErrorDiagnostic{
			Stage:           stage,
			ProxyInstanceID: h.instanceID,
			ProxyRequestID:  requestID,
			TargetID:        targetID,
			Provider:        providerID,
			APIType:         apiType,
			Model:           model,
		}
		if traceOK {
			diagnostic.TraceID = traceCtx.TraceID
			diagnostic.SpanID = traceCtx.SpanID
		}
		return diagnostic
	}
	writeFailure := func(status int, stage llm.APIErrorStage, proxyErr *llm.APIError) {
		finalDiagnostic = diagnosticFor(stage)
		proxyErr.Diagnostic = finalDiagnostic
		errAttrs = []any{
			"err_kind", "api",
			"api_status_code", proxyErr.StatusCode,
			"api_code", proxyErr.Code,
			"api_message", proxyErr.Message,
			"api_retryable", proxyErr.Retryable,
			"api_retry_after_ms", proxyErr.RetryAfter.Milliseconds(),
		}
		statusCode = status
		writeError(status, proxyErr)
	}
	defer func() {
		attrs := []any{
			"request_id", requestID,
			"requester", modelproxy.ExecutionFromContext(ctx).Requester,
			"remote_addr", modelproxy.ExecutionFromContext(ctx).RemoteAddr,
			"target_id", targetID,
			"provider", providerID,
			"api_type", apiType,
			"model", model,
			"purpose", purpose,
			"status", statusCode,
			"request_bytes", reqBytes,
			"response_bytes", modelproxy.ExecutionFromContext(ctx).ResponseSize(),
			"duration", time.Since(start),
			"events", events,
			"tool_calls", toolCalls,
			"stop_reason", string(stop),
			"input_tokens", usage.InputTokens,
			"output_tokens", usage.OutputTokens,
			"cache_read_tokens", usage.CacheReadTokens,
			"cache_write_tokens", usage.CacheWriteTokens,
			"cache_write_1h_tokens", usage.CacheWrite1hTokens,
			"reasoning_tokens", usage.ReasoningTokens,
		}
		failed := streamFailed(ctx, streamErr, statusCode)
		if targetID != "" && usage.CostKnown {
			attrs = append(attrs, "cost_usd", usage.CostUSD)
			h.recordUsage(targetID, usage, usage.CostUSD)
			if budget != nil && !failed {
				if err := budget.Add(usage.CostUSD); err != nil {
					h.logger.Warn("persist api key cost budget state failed", "err", err)
				}
			}
		} else if budget != nil && targetID != "" && !failed {
			h.logger.Warn("api key cost budget enabled but request cost was unknown", "target_id", targetID, "provider", providerID, "model", model)
		}
		if traceOK {
			attrs = append(attrs, tracing.LogAttrs(traceCtx)...)
		}
		attrs = append(attrs, shapeLogAttrs(requestShape)...)
		attrs = append(attrs, diagnosticLogAttrs(finalDiagnostic)...)
		// Record every stream request, even one that failed before the target
		// resolved (provider/model empty), so requests_total/errors_total reflect
		// all client-facing failures, not just post-resolution ones.
		h.recordMetrics(ctx, providerID, model, purpose, usage, time.Since(start), failed)
		h.recordContinuation(continuationResult)
		if streamErr != "" {
			attrs = append(attrs, "err", streamErr)
			attrs = append(attrs, errAttrs...)
			if !failed && errors.Is(ctx.Err(), context.Canceled) {
				h.logger.Info("model request completed", attrs...)
			} else {
				h.logger.Warn("model request completed", attrs...)
			}
			return
		}
		if statusCode >= http.StatusBadRequest {
			h.logger.Warn("model request completed", attrs...)
			return
		}
		h.logger.Info("model request completed", attrs...)
	}()

	if req.Request.PreviousResponseID != "" {
		continuationResult = "failed"
	}
	if err := validateProxyRequestMessages(req.Request); err != nil {
		streamErr = "invalid model request content"
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyDecode, &llm.APIError{
			StatusCode: http.StatusBadRequest,
			Code:       "invalid_request",
			Message:    "invalid model request content",
		})
		return
	}
	purpose = llm.NormalizeRequestPurpose(req.Request.Purpose)
	req.Request.Purpose = purpose
	targetID = strings.TrimSpace(req.TargetID)
	if targetID == "" {
		streamErr = "target_id is required"
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyResolve, &llm.APIError{StatusCode: http.StatusBadRequest, Message: "target_id is required"})
		return
	}
	target, err := h.resolveTarget(targetID)
	if err != nil {
		streamErr = err.Error()
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyResolve, &llm.APIError{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	targetID = target.targetID
	providerID = target.pc.Name
	apiType = target.pc.APIType
	model = target.entry.Name
	if err := applyServiceTierForTarget(target, &req.Request); err != nil {
		streamErr = err.Error()
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyPrepare, &llm.APIError{StatusCode: http.StatusBadRequest, Message: err.Error()})
		return
	}
	req.Request.Model = model
	req.Request.ServerTools = resolveServerToolsForTarget(target, req.Request.ServerTools)
	req.Request.Reasoning = h.reasoningForTarget(target, req.ReasoningProfile, req.Request.Reasoning)
	sessionKey, providerRequest := prepareProviderRequest(h.mapReasoningStates(target, req.Request))
	req.Request = providerRequest
	stateful := providerContinuationStateful(target.pc)
	if !stateful && (req.Request.StoreResponse || req.Request.PreviousResponseID != "") {
		streamErr = "target does not support provider continuation"
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProxyPrepare, &llm.APIError{
			StatusCode: http.StatusBadRequest,
			Code:       modelproxy.ErrCodeContinuationUnsupported,
			Message:    streamErr,
			Retryable:  false,
		})
		return
	}
	var budgetErr *llm.APIError
	budget, budgetErr = h.checkCostBudget(ctx, target, req.Request, diagnosticFor(llm.APIErrorStageProxyPrepare))
	if budgetErr != nil {
		streamErr = budgetErr.Error()
		writeFailure(budgetErr.StatusCode, llm.APIErrorStageProxyPrepare, budgetErr)
		return
	}
	opts, err := h.runtimeOptionsForTarget(ctx, target)
	if err != nil {
		streamErr = err.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = deliver(streamFrame{Error: err})
			return
		}
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProviderRuntime, executionErrorFrom(err))
		return
	}
	apiType = opts.Provider
	semanticRequest := req.Request
	semanticRequest.Messages = append([]llm.Message(nil), req.Request.Messages...)
	provider, releaseProvider, err := h.streamProvider(opts, target.baseTargetID, sessionKey, modelproxy.ExecutionFromContext(ctx).Principal)
	if err != nil {
		streamErr = err.Error()
		writeFailure(http.StatusBadRequest, llm.APIErrorStageProviderRuntime, executionErrorFrom(err))
		return
	}
	defer releaseProvider()
	if req.Request.NativeSteering && purpose == llm.RequestPurposeTurn && targetReasoningUpdates(target.pc, target.entry) && targetResponsesWebSocket(target.pc, target.entry) {
		if steerer, ok := provider.(llm.LiveSteerer); ok {
			defer h.registerLiveStream(ctx, target.targetID, sessionKey, steerer)()
		}
	} else {
		req.Request.NativeSteering = false
	}
	if req.Request.PreviousResponseID != "" {
		if availability, ok := provider.(responseContinuationAvailability); ok &&
			!availability.CanContinueResponse(req.Request.PreviousResponseID) {
			continuationResult = "unavailable"
			streamErr = "previous response is unavailable on this proxy instance"
			writeFailure(http.StatusConflict, llm.APIErrorStageProxyPrepare, &llm.APIError{
				StatusCode: http.StatusConflict,
				Code:       modelproxy.ErrCodePreviousResponseUnavailable,
				Message:    streamErr,
				Retryable:  false,
			})
			return
		}
	}

	wire := newAttemptWriter(deliver, true)
	defer wire.close()
	writeEnvelope := wire.write
	enrichModelRequestEvent := func(event llm.ModelRequestEvent) llm.ModelRequestEvent {
		eventSequence++
		event.Sequence = eventSequence
		event.ProxyInstanceID = h.instanceID
		event.ProxyRequestID = requestID
		event.TargetID = targetID
		event.Provider = providerID
		event.APIType = apiType
		event.Model = model
		event.Purpose = purpose
		event.ElapsedMS = time.Since(start).Milliseconds()
		if traceOK {
			event.TraceID = traceCtx.TraceID
			event.SpanID = traceCtx.SpanID
		}
		if event.UpstreamRequestID == "" && finalDiagnostic != nil {
			event.UpstreamRequestID = finalDiagnostic.UpstreamRequestID
		}
		return event
	}
	writeModelRequestEvent := func(event llm.ModelRequestEvent) bool {
		event = enrichModelRequestEvent(event)
		switch event.State {
		case llm.ModelRequestUpstreamAttemptFailed, llm.ModelRequestFailed:
			h.logger.Warn("model upstream attempt failed", modelRequestEventLogAttrs(event)...)
		case llm.ModelRequestRetryScheduled:
			h.logger.Info("model upstream retry scheduled", modelRequestEventLogAttrs(event)...)
		case llm.ModelRequestCancelled:
			h.logger.Info("model request cancelled", modelRequestEventLogAttrs(event)...)
		}
		streamEvent := llm.StreamEvent{Kind: llm.EventModelRequest, ModelRequest: &event}
		if err := writeEnvelope(streamFrame{Event: &streamEvent}); err != nil {
			streamErr = err.Error()
			return false
		}
		return true
	}

	accepted := enrichModelRequestEvent(llm.ModelRequestEvent{
		State: llm.ModelRequestAccepted,
	})
	h.logger.Info("model request started", modelRequestEventLogAttrs(accepted)...)
	acceptedEvent := llm.StreamEvent{Kind: llm.EventModelRequest, ModelRequest: &accepted}
	if err := writeEnvelope(streamFrame{Event: &acceptedEvent}); err != nil {
		streamErr = err.Error()
		return
	}
	stopCancellationLog := context.AfterFunc(ctx, func() {
		cancelled := accepted
		cancelled.State = llm.ModelRequestCancelled
		cancelled.ElapsedMS = time.Since(start).Milliseconds()
		h.logger.Info("model request cancellation observed", modelRequestEventLogAttrs(cancelled)...)
	})
	defer stopCancellationLog()

	type streamRetry int
	const (
		streamRetryNone streamRetry = iota
		streamRetryServerTools
		streamRetryMinOutputTokens
		streamRetryWebSocketReconnect
	)
	// Install exactly once: compatibility and proxy retries must not reset
	// the physical attempt sequence namespace inherited by the dialect.
	attemptCtx := llm.WithAttemptObserver(ctx, llm.AttemptObserverFunc(func(event llm.AttemptEvent) {
		event = h.priceAttempt(target, req.Request, event)
		wire.observe(streamFrame{Attempt: &event})
	}))
	attemptCtx = withTargetAttemptMetadata(attemptCtx, target, apiType, purpose, req.CallerAttempt)
	retryMinOutputTokens := 0
	webSocketConnectionLimitRetried := false
	streamAttempt := func(ctx context.Context, request, semanticRequest llm.Request) streamRetry {
		attemptStart := time.Now()
		semanticRequest.StoreResponse = request.StoreResponse
		semanticRequest.PreviousResponseID = request.PreviousResponseID
		semanticRequest.MaxTokens = request.MaxTokens
		semanticRequest.ServerTools = request.ServerTools
		streamErr = ""
		errAttrs = nil
		requestShape = richToolResultShape(semanticRequest, apiType)
		finalDiagnostic = nil
		sentEvents := false
		attemptToolCalls := 0
		var responseUsage, carriedUsage llm.Usage
		var pendingTerminalEvent *llm.ModelRequestEvent
		cancelEventSent := false
		for ev, err := range provider.Stream(ctx, request) {
			if err != nil {
				rawErr := err
				if errors.Is(rawErr, context.Canceled) || errors.Is(rawErr, context.DeadlineExceeded) {
					streamErr = rawErr.Error()
					errAttrs = streamErrorLogAttrs(rawErr)
					if !cancelEventSent {
						writeModelRequestEvent(llm.ModelRequestEvent{
							State:   llm.ModelRequestCancelled,
							Outcome: llm.ModelRequestOutcomeTerminal,
							Message: rawErr.Error(),
						})
					}
					_ = writeEnvelope(streamFrame{Error: executionErrorFrom(rawErr)})
					return streamRetryNone
				}
				webSocketConnectionLimit := !sentEvents && webSocketConnectionLimitReached(rawErr)
				diagnostic := diagnosticFor(upstreamErrorStage(rawErr))
				diagnostic.UpstreamRequestID = upstreamRequestIDFromError(rawErr)
				diagnostic.MultimodalShape = requestShape
				diagnostic.Compatibility = classifyMultimodalToolResultRejection(rawErr, requestShape)
				finalDiagnostic = diagnostic
				err = redactImageBearingError(rawErr, semanticRequest)
				err = withAPIErrorDiagnostic(err, diagnostic)
				if webSocketConnectionLimit && request.PreviousResponseID != "" {
					// The proxy only has the continuation suffix, so it cannot safely
					// replay this request on a fresh socket. Convert the provider's
					// lifetime-limit error into the structured signal that makes the
					// client reset its anchor and resend full context.
					err = webSocketContinuationUnavailableError(err)
					continuationResult = "unavailable"
				} else if !sentEvents && request.PreviousResponseID != "" && previousResponseRejected(err) {
					continuationResult = "rejected_upstream"
				}
				streamErr = err.Error()
				errAttrs = streamErrorLogAttrs(err)

				retryKind := streamRetryNone
				if webSocketConnectionLimit && request.PreviousResponseID == "" && !webSocketConnectionLimitRetried {
					// This request already carries full context. The Responses provider
					// closes the expired socket before returning, so one unchanged retry
					// opens a fresh connection without risking duplicate model output.
					retryKind = streamRetryWebSocketReconnect
				}
				if retryKind == streamRetryNone && !sentEvents && len(request.ServerTools) > 0 && serverToolRejected(err) {
					retryKind = streamRetryServerTools
				}
				if retryKind == streamRetryNone && !sentEvents {
					if floor, ok := outputTokenFloorRejected(err); ok && floor > request.MaxTokens {
						retryMinOutputTokens = floor
						h.logger.Warn("retrying model request with higher output token floor",
							"request_id", requestID,
							"target_id", targetID,
							"provider", providerID,
							"api_type", apiType,
							"model", model,
							"configured_min_output_tokens", target.pc.MinOutputTokens,
							"inferred_min_output_tokens", floor,
							"original_max_tokens", request.MaxTokens,
							"retry_max_tokens", floor,
							"err", redactImageBearingError(err, semanticRequest).Error(),
						)
						retryKind = streamRetryMinOutputTokens
					}
				}

				issue := modelRequestEventFromError(err, llm.ModelRequestFailed, llm.ModelRequestOutcomeTerminal, time.Since(attemptStart))
				if pendingTerminalEvent != nil {
					issue = mergeModelRequestFailure(*pendingTerminalEvent, issue)
				}
				if retryKind != streamRetryNone && !sentEvents {
					issue.State = llm.ModelRequestUpstreamAttemptFailed
					issue.Outcome = llm.ModelRequestOutcomeRetrying
					if !writeModelRequestEvent(issue) {
						return streamRetryNone
					}
					retryEvent := issue
					retryEvent.State = llm.ModelRequestRetryScheduled
					retryEvent.Outcome = ""
					retryEvent.Attempt++
					retryEvent.AttemptDurationMS = 0
					if !writeModelRequestEvent(retryEvent) {
						return streamRetryNone
					}
					// These proxy-owned transitions have no backoff. Observe the
					// zero-delay wait separately from a subsequent physical start;
					// cancellation here must not invent another upstream request.
					reason, _ := llm.ClassifyAttemptError(rawErr, 0)
					_ = llm.ObserveRetryWait(attemptCtx, 0, llm.RetryLayerProxy, reason, attemptCtx.Err)
					return retryKind
				}
				if !writeModelRequestEvent(issue) {
					return streamRetryNone
				}
				_ = writeEnvelope(streamFrame{Error: executionErrorFrom(err)})
				return streamRetryNone
			}
			if ev.Kind == llm.EventModelRequest && ev.ModelRequest != nil {
				status := redactModelRequestEvent(*ev.ModelRequest, semanticRequest)
				if status.State == llm.ModelRequestCancelled {
					cancelEventSent = true
				}
				if status.State == llm.ModelRequestUpstreamAttemptFailed && status.Outcome == llm.ModelRequestOutcomeTerminal {
					pendingTerminalEvent = &status
					continue
				}
				if !writeModelRequestEvent(status) {
					return streamRetryNone
				}
				continue
			}
			sentEvents = true
			events++
			if ev.Usage != nil || ev.Kind == llm.EventDone {
				if ev.Usage != nil {
					responseUsage = mergeUsage(responseUsage, *ev.Usage)
				}
				responseUsage = h.priceUsage(targetID, request, responseUsage)
				usage = addSteeredUsage(carriedUsage, responseUsage)
				snapshot := responseUsage
				ev.Usage = &snapshot
			}
			switch ev.Kind {
			case llm.EventToolCallDone:
				toolCalls++
				attemptToolCalls++
			case llm.EventDone:
				if attemptToolCalls > 0 {
					ev.StopReason = llm.StopToolUse
				}
				stop = ev.StopReason
			case llm.EventLiveSteer:
				if ev.LiveSteer != nil && ev.LiveSteer.Boundary {
					carriedUsage = usage
					responseUsage = llm.Usage{}
					attemptToolCalls = 0
				}
			}
			event := ev
			if err := writeEnvelope(streamFrame{Event: &event}); err != nil {
				streamErr = err.Error()
				return streamRetryNone
			}
		}
		if pendingTerminalEvent != nil {
			if !writeModelRequestEvent(*pendingTerminalEvent) {
				return streamRetryNone
			}
		}
		return streamRetryNone
	}
	trackedStreamAttempt := func(request, semanticRequest llm.Request) streamRetry {
		ctx, attempts := llm.TrackAttempts(attemptCtx)
		retry := streamAttempt(ctx, request, semanticRequest)
		// A retry is returned only when !sentEvents proves the entire
		// subgroup's usage was omitted. Wait until streamAttempt returns so
		// provider iterator defers have emitted their final physical facts.
		if retry != streamRetryNone {
			attempts.Discard(llm.AttemptDiscardProxyRetry)
		}
		return retry
	}
	attemptRequest := req.Request
	retry := trackedStreamAttempt(attemptRequest, semanticRequest)
	for retry != streamRetryNone {
		switch retry {
		case streamRetryServerTools:
			attemptRequest.ServerTools = nil
			semanticRequest.ServerTools = nil
		case streamRetryMinOutputTokens:
			attemptRequest.MaxTokens = retryMinOutputTokens
			semanticRequest.MaxTokens = retryMinOutputTokens
		case streamRetryWebSocketReconnect:
			webSocketConnectionLimitRetried = true
		default:
			retry = streamRetryNone
			continue
		}
		attemptCtx = llm.WithAttemptCause(attemptCtx, llm.AttemptRetry, llm.RetryLayerProxy)
		retry = trackedStreamAttempt(attemptRequest, semanticRequest)
	}
	if streamErr == "" {
		if req.Request.PreviousResponseID != "" {
			continuationResult = "served"
		}
		writeModelRequestEvent(llm.ModelRequestEvent{
			State:     llm.ModelRequestCompleted,
			ElapsedMS: time.Since(start).Milliseconds(),
		})
	}
}

type streamRequest struct {
	TargetID         string
	Request          llm.Request
	ReasoningProfile string
	CallerAttempt    *modelproxy.CallerAttempt
}
