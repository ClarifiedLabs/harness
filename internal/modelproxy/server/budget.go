package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

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
