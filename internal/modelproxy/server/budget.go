package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"harness/internal/apikey"
	"harness/internal/llm"
	"harness/internal/modelproxy/pricing"
	"harness/internal/modelproxy/protocol"
)

type costBudgetTracker struct {
	mu        sync.Mutex
	cfg       CostBudgetConfig
	statePath string
	now       func() time.Time

	windowStart time.Time
	spentUSD    float64
}

func validateCostBudget(cfg CostBudgetConfig) error {
	if cfg.LimitUSD < 0 || math.IsNaN(cfg.LimitUSD) || math.IsInf(cfg.LimitUSD, 0) {
		return fmt.Errorf("cost_budget.limit_usd must be finite and non-negative")
	}
	if cfg.Period.Set && cfg.Period.Duration < 0 {
		return fmt.Errorf("cost_budget.period must be non-negative")
	}
	limitSet := cfg.LimitUSD > 0
	periodPositive := cfg.Period.Set && cfg.Period.Duration > 0
	if !limitSet && !periodPositive {
		return nil
	}
	if !limitSet && periodPositive {
		return fmt.Errorf("cost_budget.limit_usd must be positive when period is set")
	}
	if limitSet && !periodPositive {
		return fmt.Errorf("cost_budget.period must be positive when limit_usd is set")
	}
	return nil
}

func newCostBudgetTrackerAtPath(cfg CostBudgetConfig, statePath string, now func() time.Time) (*costBudgetTracker, error) {
	if err := validateCostBudget(cfg); err != nil {
		return nil, err
	}
	if !budgetEnabled(cfg) {
		return nil, nil
	}
	if now == nil {
		now = time.Now
	}
	b := &costBudgetTracker{
		cfg:       cfg,
		statePath: statePath,
		now:       now,
	}
	if err := b.load(); err != nil {
		return nil, err
	}
	return b, nil
}

func budgetEnabled(cfg CostBudgetConfig) bool {
	return cfg.LimitUSD > 0 && cfg.Period.Set && cfg.Period.Duration > 0
}

type costBudgetState struct {
	LimitUSD      float64   `json:"limit_usd"`
	PeriodSeconds int64     `json:"period_seconds"`
	WindowStart   time.Time `json:"window_start"`
	SpentUSD      float64   `json:"spent_usd"`
}

func (b *costBudgetTracker) load() error {
	now := b.now()
	fresh := func() {
		b.windowStart = now
		b.spentUSD = 0
	}
	data, err := os.ReadFile(b.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fresh()
			return nil
		}
		return fmt.Errorf("read cost budget state: %w", err)
	}
	var st costBudgetState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("parse cost budget state: %w", err)
	}
	if st.LimitUSD != b.cfg.LimitUSD || st.PeriodSeconds != int64(b.cfg.Period.Duration/time.Second) || st.WindowStart.IsZero() || st.SpentUSD < 0 || math.IsNaN(st.SpentUSD) || math.IsInf(st.SpentUSD, 0) {
		fresh()
		return nil
	}
	b.windowStart = st.WindowStart
	b.spentUSD = st.SpentUSD
	b.resetIfNeededLocked(now)
	return nil
}

func (b *costBudgetTracker) RejectUnpriced() bool {
	return b != nil && b.cfg.RejectUnpriced
}

func (b *costBudgetTracker) Check() (bool, time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.resetIfNeededLocked(now)
	if b.spentUSD >= b.cfg.LimitUSD {
		return false, b.windowStart.Add(b.cfg.Period.Duration).Sub(now)
	}
	return true, 0
}

func (b *costBudgetTracker) Add(cost float64) error {
	if b == nil || cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNeededLocked(b.now())
	b.spentUSD += cost
	return b.persistLocked()
}

func (b *costBudgetTracker) Report() *protocol.BudgetReport {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.resetIfNeededLocked(now)
	end := b.windowStart.Add(b.cfg.Period.Duration)
	remaining := b.cfg.LimitUSD - b.spentUSD
	if remaining < 0 {
		remaining = 0
	}
	return &protocol.BudgetReport{
		LimitUSD:      b.cfg.LimitUSD,
		PeriodSeconds: int64(b.cfg.Period.Duration / time.Second),
		WindowStart:   b.windowStart,
		WindowEnd:     end,
		SpentUSD:      b.spentUSD,
		RemainingUSD:  remaining,
	}
}

func (b *costBudgetTracker) resetIfNeededLocked(now time.Time) {
	if b.windowStart.IsZero() || !now.Before(b.windowStart.Add(b.cfg.Period.Duration)) {
		b.windowStart = now
		b.spentUSD = 0
	}
}

func (b *costBudgetTracker) persistLocked() error {
	if b.statePath == "" {
		return nil
	}
	st := costBudgetState{
		LimitUSD:      b.cfg.LimitUSD,
		PeriodSeconds: int64(b.cfg.Period.Duration / time.Second),
		WindowStart:   b.windowStart,
		SpentUSD:      b.spentUSD,
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cost budget state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(b.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cost budget state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(b.statePath)+".*")
	if err != nil {
		return fmt.Errorf("create temp cost budget state: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp cost budget state: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp cost budget state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp cost budget state: %w", err)
	}
	if err := os.Rename(tmpName, b.statePath); err != nil {
		return fmt.Errorf("rename temp cost budget state: %w", err)
	}
	cleanup = false
	return nil
}

// recordMetrics stamps one model request into the metrics registry. It is called
// once per /v1/stream or /v1/compact (including bounded retries), regardless of whether the
// target resolved (empty provider/model labels are omitted) or the model is
// priced, so free models and pre-resolution failures still get counters. Cost is
// recorded only when usage.CostKnown. failed is true when the stream errored or
// returned a 4xx/5xx status. Purpose is normalized to the bounded llm contract;
// key is the authorizing API key's name ("anonymous" when auth is disabled or
// absent).
func (h *Handler) recordMetrics(r *http.Request, providerID, model string, purpose llm.RequestPurpose, usage llm.Usage, duration time.Duration, failed bool) {
	if h.metrics == nil || h.metricFams == nil {
		return
	}
	key := "anonymous"
	if name, ok := apikey.AuthorizedName(r); ok {
		key = name
	}
	labels := map[string]string{
		"provider": providerID,
		"model":    model,
		"purpose":  string(llm.NormalizeRequestPurpose(purpose)),
		"key":      key,
	}
	h.metricFams.requests.Inc(labels)
	h.metricFams.duration.Add(duration.Seconds(), labels)
	if failed {
		h.metricFams.errors.Inc(labels)
	}
	if usage.InputTokens != 0 {
		h.metricFams.input.Add(float64(usage.InputTokens), labels)
	}
	if usage.OutputTokens != 0 {
		h.metricFams.output.Add(float64(usage.OutputTokens), labels)
	}
	if usage.CacheReadTokens != 0 {
		h.metricFams.cacheRead.Add(float64(usage.CacheReadTokens), labels)
	}
	if promptInput := llm.PromptInputTokens(usage); promptInput != 0 {
		h.metricFams.promptInput.Add(float64(promptInput), labels)
	}
	if usage.CacheWriteTokens != 0 {
		h.metricFams.cacheWrite.Add(float64(usage.CacheWriteTokens), labels)
	}
	if usage.CacheWrite1hTokens != 0 {
		h.metricFams.cacheWrite1h.Add(float64(usage.CacheWrite1hTokens), labels)
	}
	if usage.ReasoningTokens != 0 {
		h.metricFams.reasoning.Add(float64(usage.ReasoningTokens), labels)
	}
	if usage.CostKnown {
		h.metricFams.cost.Add(usage.CostUSD, labels)
	}
}

// recordUsage accumulates one priced request into the per-model usage map. It is
// called only for requests with known cost, so every bucket has a meaningful
// CostUSD.
func (h *Handler) recordUsage(targetID string, u llm.Usage, cost float64) {
	key := usageKey{targetID: targetID}
	h.usageMu.Lock()
	defer h.usageMu.Unlock()
	acc := h.usage[key]
	if acc == nil {
		acc = &protocol.ModelUsage{TargetID: targetID}
		h.usage[key] = acc
	}
	acc.Requests++
	acc.InputTokens += int64(u.InputTokens)
	acc.OutputTokens += int64(u.OutputTokens)
	acc.CacheReadTokens += int64(u.CacheReadTokens)
	acc.CacheWriteTokens += int64(u.CacheWriteTokens)
	acc.CacheWrite1hTokens += int64(u.CacheWrite1hTokens)
	acc.ReasoningTokens += int64(u.ReasoningTokens)
	acc.CostUSD += cost
}

func (h *Handler) checkCostBudget(w http.ResponseWriter, r *http.Request, target resolvedTarget, request llm.Request, diagnostic *llm.APIErrorDiagnostic) (*costBudgetTracker, bool, string) {
	budget, err := h.requestCostBudget(r)
	if err != nil {
		msg := err.Error()
		writeError(w, http.StatusInternalServerError, &protocol.Error{StatusCode: http.StatusInternalServerError, Code: "cost_budget_state_error", Message: msg, Diagnostic: diagnostic})
		return nil, false, msg
	}
	if budget == nil {
		return nil, true, ""
	}
	snapshot := h.snapshot.Load()
	if snapshot == nil || snapshot.pricer == nil {
		msg := "api key cost budget is enabled but pricing is unavailable"
		if budget.RejectUnpriced() {
			writeError(w, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Code: "cost_budget_unpriced_target", Message: msg, Diagnostic: diagnostic})
			return budget, false, msg
		}
		return budget, true, ""
	}
	price := snapshot.pricer.PriceUsage(pricing.Input{
		TargetID: target.targetID,
		Provider: target.pc,
		Model:    target.entry,
		Request:  request,
	})
	if !price.Known {
		msg := fmt.Sprintf("api key cost budget is enabled but target %q has no known price", target.targetID)
		if budget.RejectUnpriced() {
			writeError(w, http.StatusBadRequest, &protocol.Error{StatusCode: http.StatusBadRequest, Code: "cost_budget_unpriced_target", Message: msg, Diagnostic: diagnostic})
			return budget, false, msg
		}
		return budget, true, ""
	}
	if ok, retryAfter := budget.Check(); !ok {
		if retryAfter < 0 {
			retryAfter = 0
		}
		retrySeconds := int(math.Ceil(retryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		msg := fmt.Sprintf("api key cost budget exhausted; retry after %s", retryAfter.Round(time.Second))
		writeError(w, http.StatusTooManyRequests, &protocol.Error{
			StatusCode:   http.StatusTooManyRequests,
			Code:         "cost_budget_exceeded",
			Message:      msg,
			Retryable:    true,
			RetryAfterMS: retryAfter.Milliseconds(),
			Diagnostic:   diagnostic,
		})
		return budget, false, msg
	}
	return budget, true, ""
}

func (h *Handler) requestCostBudget(r *http.Request) (*costBudgetTracker, error) {
	entry, ok := apikey.AuthorizedEntry(r)
	if !ok || entry.CostBudget == nil || !entry.CostBudget.Enabled() {
		return nil, nil
	}
	cfg := CostBudgetConfig{
		LimitUSD:       entry.CostBudget.LimitUSD,
		Period:         Duration{Duration: time.Duration(entry.CostBudget.PeriodSeconds) * time.Second, Set: true},
		RejectUnpriced: entry.CostBudget.RejectUnpriced,
	}
	statePath := h.keyBudgetStatePath(entry)
	cacheKey := strings.Join([]string{
		statePath,
		strconv.FormatFloat(cfg.LimitUSD, 'g', -1, 64),
		strconv.FormatInt(entry.CostBudget.PeriodSeconds, 10),
		strconv.FormatBool(cfg.RejectUnpriced),
	}, "|")
	h.keyBudgetMu.Lock()
	defer h.keyBudgetMu.Unlock()
	if h.keyBudgets == nil {
		h.keyBudgets = map[string]*costBudgetTracker{}
	}
	if budget := h.keyBudgets[cacheKey]; budget != nil {
		return budget, nil
	}
	budget, err := newCostBudgetTrackerAtPath(cfg, statePath, h.now)
	if err != nil {
		return nil, err
	}
	h.keyBudgets[cacheKey] = budget
	return budget, nil
}

func (h *Handler) priceUsage(targetID string, request llm.Request, usage llm.Usage) llm.Usage {
	snapshot := h.snapshot.Load()
	if snapshot == nil || snapshot.pricer == nil {
		return usage
	}
	target, ok := snapshot.targets[targetID]
	if !ok {
		return usage
	}
	res := snapshot.pricer.PriceUsage(pricing.Input{
		TargetID: targetID,
		Provider: target.pc,
		Model:    target.entry,
		Request:  request,
		Usage:    usage,
	})
	if !res.Known {
		return usage
	}
	usage.CostUSD = res.CostUSD
	usage.CostKnown = true
	return usage
}
