package session

import (
	"strings"

	"harness/internal/llm"
)

// ClassifiedFailure is the result of classifying one failed tool result or
// model request. Confidence is "high" when the class came from structured
// data or an exact message pattern and "low"/"medium" for heuristic text
// matches on legacy logs.
type ClassifiedFailure struct {
	Kind       llm.ToolErrorKind
	Confidence string
}

// ClassifyToolError assigns an error kind to a legacy tool_result event that
// predates structured ErrorKind recording, using the recorded one-line Display
// (the ErrorExcerpt is a fallback for classes whose marker the 80-byte display
// clip may have cut). Patterns are anchored against the current Display
// formats ("[tool args] → error: <first line, clipped>"); rules are ordered,
// first match wins.
func ClassifyToolError(display, excerpt string) ClassifiedFailure {
	switch {
	case strings.Contains(display, "→ error: unknown tool "):
		return ClassifiedFailure{Kind: llm.ToolErrorUnknownTool, Confidence: "high"}
	case strings.Contains(display, "→ error: invalid arguments: "):
		return ClassifiedFailure{Kind: llm.ToolErrorInvalidArgs, Confidence: "high"}
	case strings.Contains(display, "→ error: tool timed out after "):
		return ClassifiedFailure{Kind: llm.ToolErrorTimeout, Confidence: "high"}
	case strings.Contains(display, "→ error: tool panicked: "):
		return ClassifiedFailure{Kind: llm.ToolErrorPanic, Confidence: "high"}
	case strings.Contains(display, "→ error: ") &&
		(strings.Contains(display, "no such file or directory") || strings.Contains(excerpt, "no such file or directory")):
		return ClassifiedFailure{Kind: llm.ToolErrorPathNotFound, Confidence: "medium"}
	case strings.Contains(display, "→ error: could not find ") && strings.Contains(display, "oldText"):
		return ClassifiedFailure{Kind: llm.ToolErrorEditOldTextNotFound, Confidence: "high"}
	case strings.Contains(display, "→ error: found ") &&
		strings.Contains(display, " occurrences of ") && strings.Contains(display, "oldText"):
		return ClassifiedFailure{Kind: llm.ToolErrorEditOldTextAmbiguous, Confidence: "high"}
	case strings.Contains(display, "error parsing regexp"):
		return ClassifiedFailure{Kind: llm.ToolErrorRegexInvalid, Confidence: "low"}
	case strings.Contains(display, "→ error: [loop guard] blocked: "):
		return ClassifiedFailure{Kind: llm.ToolErrorBlocked, Confidence: "high"}
	case strings.Contains(display, "→ error: blocked by ") && strings.Contains(display, " hook"):
		return ClassifiedFailure{Kind: llm.ToolErrorHookBlocked, Confidence: "high"}
	case strings.Contains(display, "→ error: tool ") && strings.Contains(display, " requires ") && strings.Contains(display, " input, but the current model"),
		strings.Contains(display, "→ error: tool result includes images"),
		strings.Contains(display, "→ error: tool result images rejected"):
		return ClassifiedFailure{Kind: llm.ToolErrorUnsupportedModality, Confidence: "high"}
	case strings.Contains(display, "→ error: invalid rich tool result: "):
		return ClassifiedFailure{Kind: llm.ToolErrorInvalidResult, Confidence: "high"}
	default:
		return ClassifiedFailure{Kind: llm.ToolErrorOther, Confidence: "low"}
	}
}

// ClassifyModelRequestFailure maps a failed model_request event's structured
// status/code onto an error kind. The mapping is deterministic, so the
// confidence is always high; provider_error is the fallback for unmapped
// failures.
func ClassifyModelRequestFailure(ev *llm.ModelRequestEvent) llm.ToolErrorKind {
	if ev == nil {
		return llm.ToolErrorProviderError
	}
	code := strings.ToLower(strings.TrimSpace(ev.Code))
	switch {
	case ev.StatusCode == 429 || strings.Contains(code, "rate_limit"):
		return llm.ToolErrorRateLimited
	case strings.Contains(code, "overloaded"):
		return llm.ToolErrorProviderOverloaded
	case strings.Contains(code, "api_error") || strings.Contains(code, "internal"):
		return llm.ToolErrorProviderInternalError
	case strings.Contains(code, "auth") || ev.StatusCode == 401 || ev.StatusCode == 403:
		return llm.ToolErrorProviderAuth
	case strings.Contains(code, "invalid_request") || ev.StatusCode == 400:
		return llm.ToolErrorProviderRequest
	case ev.StatusCode >= 500:
		return llm.ToolErrorProvider5xx
	default:
		return llm.ToolErrorProviderError
	}
}
