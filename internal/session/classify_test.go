package session

import (
	"testing"

	"harness/internal/llm"
)

func TestClassifyToolError(t *testing.T) {
	cases := []struct {
		name       string
		display    string
		excerpt    string
		want       llm.ToolErrorKind
		confidence string
	}{
		{
			name:       "unknown tool",
			display:    `[frobnicate] → error: unknown tool "frobnicate"`,
			want:       llm.ToolErrorUnknownTool,
			confidence: "high",
		},
		{
			name:       "invalid arguments",
			display:    `[read_file path=x] → error: invalid arguments: path is required`,
			want:       llm.ToolErrorInvalidArgs,
			confidence: "high",
		},
		{
			name:       "timeout",
			display:    `[run_command command="sleep 9"] → error: tool timed out after 5s`,
			want:       llm.ToolErrorTimeout,
			confidence: "high",
		},
		{
			name:       "panic",
			display:    `[glob] → error: tool panicked: boom`,
			want:       llm.ToolErrorPanic,
			confidence: "high",
		},
		{
			name:       "path not found from display",
			display:    `[read_file path=/missing] → error: stat /missing: no such file or directory`,
			want:       llm.ToolErrorPathNotFound,
			confidence: "medium",
		},
		{
			name:       "path not found via excerpt fallback",
			display:    `[read_file path=/very/long/path/that/got/clipped/before/the/marker] → error: stat /very/long/path/that/got/clipped/before/t…`,
			excerpt:    "stat /very/long/path/that/got/clipped/before/the/marker: no such file or directory",
			want:       llm.ToolErrorPathNotFound,
			confidence: "medium",
		},
		{
			name:       "edit oldText not found",
			display:    `[edit path=a.go edits=1] → error: could not find oldText in a.go; oldText must match exactly inclu…`,
			want:       llm.ToolErrorEditOldTextNotFound,
			confidence: "high",
		},
		{
			name:       "edit oldText not found multi-edit form",
			display:    `[edit path=a.go edits=2] → error: could not find edits[1].oldText in a.go; oldText must match exac…`,
			want:       llm.ToolErrorEditOldTextNotFound,
			confidence: "high",
		},
		{
			name:       "edit oldText ambiguous",
			display:    `[edit path=a.go edits=1] → error: found 3 occurrences of oldText in a.go; provide more context to…`,
			want:       llm.ToolErrorEditOldTextAmbiguous,
			confidence: "high",
		},
		{
			name:       "regexp error",
			display:    `[search pattern=(] → error: compile pattern: error parsing regexp: missing closing ): ` + "`(`",
			want:       llm.ToolErrorRegexInvalid,
			confidence: "low",
		},
		{
			name:       "blocked by loop guard",
			display:    `[run_command command="go build ./..."] → error: [loop guard] blocked: this exact call (go build ./...) already failed …`,
			want:       llm.ToolErrorBlocked,
			confidence: "high",
		},
		{
			name:       "blocked by hook",
			display:    `[write_file path=x] → error: blocked by PreToolUse hook: no writes`,
			want:       llm.ToolErrorHookBlocked,
			confidence: "high",
		},
		{
			name:       "unsupported modality",
			display:    `[read_image path=x.png] → error: tool "read_image" requires image input, but the current model does not adver…`,
			want:       llm.ToolErrorUnsupportedModality,
			confidence: "high",
		},
		{
			name:       "invalid rich result",
			display:    `[view_image path=x.png] → error: invalid rich tool result: image data is empty`,
			want:       llm.ToolErrorInvalidResult,
			confidence: "high",
		},
		{
			name:       "unrecognized stays other",
			display:    `[web_fetch url=x] → error: connection refused`,
			want:       llm.ToolErrorOther,
			confidence: "low",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyToolError(tc.display, tc.excerpt)
			if got.Kind != tc.want || got.Confidence != tc.confidence {
				t.Fatalf("ClassifyToolError(%q, %q) = %+v, want kind %q confidence %q",
					tc.display, tc.excerpt, got, tc.want, tc.confidence)
			}
		})
	}
}

func TestClassifyModelRequestFailure(t *testing.T) {
	cases := []struct {
		name string
		ev   *llm.ModelRequestEvent
		want llm.ToolErrorKind
	}{
		{"status 429", &llm.ModelRequestEvent{StatusCode: 429}, llm.ToolErrorRateLimited},
		{"rate limit code", &llm.ModelRequestEvent{StatusCode: 400, Code: "rate_limit_exceeded"}, llm.ToolErrorRateLimited},
		{"overloaded code", &llm.ModelRequestEvent{StatusCode: 529, Code: "overloaded_error"}, llm.ToolErrorProviderOverloaded},
		{"api error code", &llm.ModelRequestEvent{StatusCode: 500, Code: "api_error"}, llm.ToolErrorProviderInternalError},
		{"internal code", &llm.ModelRequestEvent{Code: "internal_error"}, llm.ToolErrorProviderInternalError},
		{"auth code", &llm.ModelRequestEvent{StatusCode: 401, Code: "authentication_error"}, llm.ToolErrorProviderAuth},
		{"status 403 no code", &llm.ModelRequestEvent{StatusCode: 403}, llm.ToolErrorProviderAuth},
		{"invalid request", &llm.ModelRequestEvent{StatusCode: 400, Code: "invalid_request_error"}, llm.ToolErrorProviderRequest},
		{"status 400 no code", &llm.ModelRequestEvent{StatusCode: 400}, llm.ToolErrorProviderRequest},
		{"status 500 no code", &llm.ModelRequestEvent{StatusCode: 500}, llm.ToolErrorProvider5xx},
		{"empty", &llm.ModelRequestEvent{}, llm.ToolErrorProviderError},
		{"nil", nil, llm.ToolErrorProviderError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyModelRequestFailure(tc.ev); got != tc.want {
				t.Fatalf("ClassifyModelRequestFailure(%+v) = %q, want %q", tc.ev, got, tc.want)
			}
		})
	}
}
