package execution

import (
	"context"
	"time"
)

type toolQueuedKey struct{}

// WithToolQueued records the scheduler admission boundary before dependency,
// stage, or semaphore waits. The timestamp is process-local, never serialized.
func WithToolQueued(ctx context.Context, queued time.Time) context.Context {
	return context.WithValue(ctx, toolQueuedKey{}, queued)
}

// ToolQueued returns the admission time when supplied by a scheduler. Direct
// registry callers without a scheduling boundary receive an unknown zero time.
func ToolQueued(ctx context.Context) time.Time {
	queued, _ := ctx.Value(toolQueuedKey{}).(time.Time)
	return queued
}
