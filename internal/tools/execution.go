package tools

import (
	"context"
	"errors"

	"harness/internal/llm"
)

func executionOutcome(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if err != nil {
		return "failed"
	}
	return "completed"
}

func executionErrorKind(kind llm.ToolErrorKind) string {
	switch kind {
	case llm.ToolErrorUnknownTool, llm.ToolErrorInvalidArgs, llm.ToolErrorTimeout,
		llm.ToolErrorCancelled, llm.ToolErrorPanic, llm.ToolErrorPathNotFound,
		llm.ToolErrorEditOldTextNotFound, llm.ToolErrorEditOldTextAmbiguous,
		llm.ToolErrorStaleFile, llm.ToolErrorHookBlocked, llm.ToolErrorBlocked,
		llm.ToolErrorUnsupportedModality, llm.ToolErrorInvalidResult,
		llm.ToolErrorRegexInvalid, llm.ToolErrorBatchFailed,
		llm.ToolErrorProviderInternalError, llm.ToolErrorProviderAuth,
		llm.ToolErrorProviderRequest, llm.ToolErrorProvider5xx,
		llm.ToolErrorRateLimited, llm.ToolErrorProviderOverloaded, llm.ToolErrorProviderError:
		return string(kind)
	default:
		return string(llm.ToolErrorOther)
	}
}

// ExecutionProcessMetrics copies only the numeric process diagnostic contract.
// Unknown keys may carry arbitrary content and are not execution observations.
func ExecutionProcessMetrics(metrics map[string]int) map[string]int {
	var out map[string]int
	for _, key := range []string{
		CommandMetricOutcomeAvailable, CommandMetricSucceeded, CommandMetricFailed,
		CommandMetricCancelled, CommandMetricTimedOut, CommandMetricExitCode,
		CommandMetricWaitComplete, CommandMetricStepsTotal, CommandMetricStepsExecuted,
		CommandMetricStepsFailed, CommandMetricStepsCancelled, CommandMetricStepsTimedOut,
		CommandMetricStepsSkipped,
	} {
		if value, ok := metrics[key]; ok {
			if out == nil {
				out = make(map[string]int)
			}
			out[key] = value
		}
	}
	return out
}
