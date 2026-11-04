// Package modelproxy defines transport-neutral model service contracts.
package modelproxy

import (
	"context"
	"harness/internal/llm"
	"time"
)

type Backend interface {
	Catalog(context.Context) (Catalog, error)
	Provider(string) llm.Provider
}
type Accounts interface {
	Limits(context.Context, string) (LimitsReport, error)
	ResetCredits(context.Context, string) (ResetCredits, error)
	ResetLimits(context.Context, ResetRequest) (ResetResult, error)
}

const (
	ErrCodePreviousResponseUnavailable = "previous_response_unavailable"
	ErrCodeContinuationUnsupported     = "continuation_unsupported"
)

// Execution binds caller identity independently of transport. Local callers may
// supply only Principal; budget admission is deliberately opt-in.
type Execution struct {
	RequestBytes  int
	ResponseBytes func() int

	Principal       []byte
	Name            string
	Authenticated   bool
	Requester       string
	RemoteAddr      string
	CostBudget      *CostBudget
	BudgetStatePath string
}
type CostBudget struct {
	LimitUSD       float64
	Period         time.Duration
	RejectUnpriced bool
}

func (b *CostBudget) Enabled() bool { return b != nil && b.LimitUSD > 0 && b.Period > 0 }

type executionKey struct{}

func WithExecution(ctx context.Context, e Execution) context.Context {
	e.Principal = append([]byte(nil), e.Principal...)
	if e.CostBudget != nil {
		b := *e.CostBudget
		e.CostBudget = &b
	}
	return context.WithValue(ctx, executionKey{}, e)
}
func ExecutionFromContext(ctx context.Context) Execution {
	if ctx == nil {
		return Execution{}
	}
	e, _ := ctx.Value(executionKey{}).(Execution)
	e.Principal = append([]byte(nil), e.Principal...)
	if e.CostBudget != nil {
		b := *e.CostBudget
		e.CostBudget = &b
	}
	return e
}

func (e Execution) ResponseSize() int {
	if e.ResponseBytes == nil {
		return 0
	}
	return e.ResponseBytes()
}
