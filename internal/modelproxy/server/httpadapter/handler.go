// Package httpadapter adapts a transport-neutral backend to the model HTTP API.
package httpadapter

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"harness/internal/apikey"
	"harness/internal/llm"
	"harness/internal/modelproxy"
	"harness/internal/modelproxy/protocol"
	"harness/internal/tracing"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxStreamRequestBytes = 64 << 20

// Core deliberately does not mention the runtime implementation, so transport
// and execution regression tests can exercise this same adapter independently.
type Core interface {
	modelproxy.Backend
	modelproxy.Accounts
	Usage(context.Context) modelproxy.UsageReport
	RecordRejected(context.Context, bool)
	InstanceID() string
	Logger() *slog.Logger
}
type Handler struct {
	Core      Core
	ConfigDir string
}

func New(core Core, configDir string) *Handler { return &Handler{core, configDir} }
func (h *Handler) RequestContext(r *http.Request) context.Context {
	ctx := r.Context()
	if tc, ok := tracing.TraceFromHeaders(r.Header); ok {
		ctx = tracing.ContextWithTrace(ctx, tc)
	}
	e := modelproxy.Execution{Requester: requesterName(r), RemoteAddr: r.RemoteAddr}
	if entry, ok := apikey.AuthorizedEntry(r); ok {
		e.Authenticated = true
		e.Name = entry.Name
		e.Principal = entry.Hash
		if b := entry.CostBudget; b != nil {
			e.CostBudget = &modelproxy.CostBudget{LimitUSD: b.LimitUSD, Period: time.Duration(b.PeriodSeconds) * time.Second, RejectUnpriced: b.RejectUnpriced}
			hash := hex.EncodeToString(entry.Hash)
			if len(hash) > 16 {
				hash = hash[:16]
			}
			name := entry.Name
			if name == "" {
				name = "key"
			}
			if hash != "" {
				name += "-" + hash
			}
			e.BudgetStatePath = filepath.Join(h.ConfigDir, "state", "api_key_budgets", name+".json")
		}
	}
	return modelproxy.WithExecution(ctx, e)
}
func requesterName(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Harness-Requester")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.UserAgent()); v != "" {
		return v
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, e *protocol.Error) {
	if e.StatusCode == 0 {
		e.StatusCode = status
	}
	writeRetryAfter(w, e)
	writeJSON(w, status, e)
}
func writeRetryAfter(w http.ResponseWriter, e *protocol.Error) {
	if e != nil && e.RetryAfterMS > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(float64(e.RetryAfterMS)/1000))))
	}
}
func apiStatus(err error) int {
	var a *llm.APIError
	if errors.As(err, &a) && a.StatusCode >= 400 {
		return a.StatusCode
	}
	return http.StatusBadRequest
}
func decode(w http.ResponseWriter, r *http.Request, v any, operation string) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStreamRequestBytes))
	if err != nil {
		writeError(w, 413, &protocol.Error{StatusCode: 413, Message: "request body too large"})
		return false
	}
	if json.Unmarshal(body, v) != nil {
		writeError(w, 400, &protocol.Error{StatusCode: 400, Message: "malformed " + operation + " request"})
		return false
	}
	return true
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cw := &countingResponseWriter{ResponseWriter: w}
	w = cw
	ctx := h.RequestContext(r)
	e := modelproxy.ExecutionFromContext(ctx)
	e.RequestBytes = int(r.ContentLength)
	e.ResponseBytes = cw.bytesWritten
	ctx = modelproxy.WithExecution(ctx, e)
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/limits"):
		h.serveLimits(w, r.WithContext(ctx))
	case r.Method == "GET" && r.URL.Path == "/v1/models":
		c, err := h.Core.Catalog(ctx)
		if err != nil {
			writeError(w, apiStatus(err), protocol.ErrorFrom(err))
			return
		}
		if tc, ok := tracing.TraceFromContext(ctx); ok {
			h.Core.Logger().Info("model proxy request completed", "requester", requesterName(r), "remote_addr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "status", 200, "trace_id", tc.TraceID, "span_id", tc.SpanID, "trace_sampled", tc.Sampled)
		}
		writeJSON(w, 200, c)
	case r.Method == "GET" && r.URL.Path == "/v1/usage":
		writeJSON(w, 200, h.Core.Usage(ctx))
	case r.Method == "POST" && r.URL.Path == "/v1/stream":
		h.stream(w, r.WithContext(ctx))
	case r.Method == "POST" && r.URL.Path == "/v1/input_tokens":
		var req protocol.TokenCountRequest
		if !decode(w, r, &req, "input token") {
			return
		}
		counter, ok := h.Core.Provider(req.TargetID).(llm.InputTokenCounter)
		if !ok {
			writeError(w, 501, &protocol.Error{StatusCode: 501, Code: "input_token_count_unsupported", Message: llm.ErrInputTokenCountUnsupported.Error()})
			return
		}
		count, err := counter.CountInputTokens(ctx, req.Request)
		if errors.Is(err, llm.ErrInputTokenCountUnsupported) {
			writeError(w, 501, &protocol.Error{StatusCode: 501, Code: "input_token_count_unsupported", Message: err.Error()})
			return
		}
		if err != nil {
			writeError(w, apiStatus(err), protocol.ErrorFrom(err))
			return
		}
		writeJSON(w, 200, protocol.TokenCountResponse{InputTokens: count.InputTokens, Source: count.Source, Scope: count.Scope})
	case r.Method == "POST" && r.URL.Path == "/v1/compact":
		var req protocol.CompactRequest
		if !decode(w, r, &req, "compact") {
			h.Core.RecordRejected(ctx, false)
			return
		}
		if req.CallerAttempt != nil {
			ctx = llm.WithAttemptCause(ctx, req.CallerAttempt.Cause, req.CallerAttempt.RetryLayer)
		}
		var attempts []llm.AttemptEvent
		var mu sync.Mutex
		ctx = llm.WithAttemptObserver(ctx, llm.AttemptObserverFunc(func(e llm.AttemptEvent) { mu.Lock(); attempts = append(attempts, e); mu.Unlock() }))
		compactor, ok := h.Core.Provider(req.TargetID).(llm.ContextCompactor)
		var result llm.CompactedContext
		var err error
		if ok {
			result, err = compactor.CompactContext(ctx, req.Request)
		} else {
			err = llm.ErrContextCompactionUnsupported
		}
		status := 200
		pe := protocol.ErrorFrom(err)
		if err != nil {
			status = apiStatus(err)
			var upstream *modelproxy.UpstreamError
			if errors.As(err, &upstream) {
				status = 502
			}
			if errors.Is(err, llm.ErrContextCompactionUnsupported) {
				status = 501
				pe = &protocol.Error{StatusCode: 501, Code: "context_compaction_unsupported", Message: err.Error()}
			} else if result.Usage != (llm.Usage{}) || len(attempts) > 0 {
				status = 502
			}
		}
		if errors.Is(err, llm.ErrContextCompactionUnsupported) {
			writeError(w, status, pe)
			return
		}
		writeRetryAfter(w, pe)
		writeJSON(w, status, protocol.CompactResponse{Context: result, Attempts: attempts, Error: pe})
	case r.Method == "POST" && r.URL.Path == "/v1/steer":
		var req protocol.SteerRequest
		if !decode(w, r, &req, "steering") {
			return
		}
		steerer, ok := h.Core.Provider(req.TargetID).(llm.LiveSteerer)
		err := llm.ErrSteeringUnavailable
		if ok {
			err = steerer.Steer(ctx, req.Submission)
		}
		if err != nil {
			status := apiStatus(err)
			if errors.Is(err, llm.ErrSteeringUnavailable) {
				status = 409
			}
			writeError(w, status, protocol.ErrorFrom(err))
			return
		}
		w.WriteHeader(202)
	default:
		http.NotFound(w, r)
	}
}
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	var req protocol.StreamRequest
	if !decode(w, r, &req, "stream") {
		h.Core.RecordRejected(r.Context(), true)
		return
	}
	ctx := r.Context()
	if req.CallerAttempt != nil {
		ctx = llm.WithAttemptCause(ctx, req.CallerAttempt.Cause, req.CallerAttempt.RetryLayer)
	}
	if req.ReasoningProfile != "" {
		req.Request.Reasoning.Profile = req.ReasoningProfile
	}
	started := false
	enc := json.NewEncoder(w)
	var writeErr error
	var mu sync.Mutex
	send := func(env protocol.StreamEnvelope) {
		mu.Lock()
		defer mu.Unlock()
		if writeErr != nil {
			return
		}
		if !started {
			started = true
			w.Header().Set("Content-Type", protocol.ContentTypeNDJSON)
			w.WriteHeader(200)
		}
		writeErr = enc.Encode(env)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	ctx = llm.WithAttemptObserver(ctx, llm.AttemptObserverFunc(func(a llm.AttemptEvent) { send(protocol.StreamEnvelope{Attempt: &a}) }))
	for ev, err := range h.Core.Provider(req.TargetID).Stream(ctx, req.Request) {
		if err != nil {
			if !started {
				writeError(w, apiStatus(err), protocol.ErrorFrom(err))
			} else {
				send(protocol.StreamEnvelope{Error: protocol.ErrorFrom(err)})
			}
			return
		}
		send(protocol.StreamEnvelope{Event: &ev})
		if writeErr != nil {
			return
		}
	}
}
func (h *Handler) RecordRejectedStream(r *http.Request) {
	h.Core.RecordRejected(h.RequestContext(r), true)
}

type authAuthorizer interface{ Authorize(*http.Request) bool }

func ObserveAuth(h *Handler, store authAuthorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store != nil && r.Method == "POST" && (r.URL.Path == "/v1/stream" || r.URL.Path == "/v1/compact") && !store.Authorize(r) {
			h.Core.RecordRejected(h.RequestContext(r), r.URL.Path == "/v1/stream")
		}
		next.ServeHTTP(w, r)
	})
}
