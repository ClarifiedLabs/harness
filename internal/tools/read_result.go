package tools

import (
	"context"
	"encoding/json"
)

const readResultBytesPerRequestedLine = 512

type dispatchResultLimitsContextKey struct{}

// RunResult applies read's input-aware allowance while the file is being read,
// not only after a potentially huge requested window has been constructed.
func (r readFile) RunResult(ctx context.Context, input json.RawMessage) (RunResult, error) {
	args, err := decodeReadFileArgs(input)
	if err != nil {
		return RunResult{}, err
	}
	requestedLines := r.requestedLines(args)
	limits := readExecutionResultLimits(ctx, requestedLines)
	out, err := r.runDecoded(ctx, args, requestedLines, limits.maxBytes)
	if err != nil {
		return RunResult{}, err
	}
	visible, info := truncateToolResult("read", out, limits)
	if !info.truncated {
		return RunResult{Text: out}, nil
	}
	return RunResult{Text: visible, OriginalText: out}, nil
}

func readExecutionResultLimits(ctx context.Context, requestedLines int) resultLimits {
	limits := resultLimits{maxBytes: defaultReadResultHardBytes, maxLines: defaultReadResultLines}
	if dispatched, ok := ctx.Value(dispatchResultLimitsContextKey{}).(resultLimits); ok {
		limits = dispatched
	}
	if scaled := scaledReadResultBytes(requestedLines); scaled < limits.maxBytes {
		limits.maxBytes = scaled
	}
	return limits
}

func scaledReadResultBytes(requestedLines int) int {
	if requestedLines <= defaultReadResultBytes/readResultBytesPerRequestedLine {
		return defaultReadResultBytes
	}
	maxInt := int(^uint(0) >> 1)
	if requestedLines > maxInt/readResultBytesPerRequestedLine {
		return maxInt
	}
	return requestedLines * readResultBytesPerRequestedLine
}
