package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

func TestCallerAttemptPayloadIsOptionalNarrowAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta llm.AttemptMetadata
		want *protocol.CallerAttempt
	}{
		{name: "legacy-default"},
		{name: "explicit-default", meta: llm.AttemptMetadata{Cause: llm.AttemptInitial, RetryLayer: llm.RetryLayerNone}},
		{name: "unknown-values", meta: llm.AttemptMetadata{Cause: "unbounded-cause", RetryLayer: "unbounded-layer"}},
		{name: "agent-retry", meta: llm.AttemptMetadata{Provider: "private-provider", API: "private-api", Model: "private-model", Purpose: "private-purpose", Cause: llm.AttemptRetry, RetryLayer: llm.RetryLayerAgent}, want: &protocol.CallerAttempt{Cause: llm.AttemptRetry, RetryLayer: llm.RetryLayerAgent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := callerAttempt(llm.WithAttemptMetadata(context.Background(), tc.meta))
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("caller attempt=%+v want=%+v", got, tc.want)
			}
			for _, request := range []any{
				protocol.StreamRequest{CallerAttempt: got},
				protocol.CompactRequest{CallerAttempt: got},
			} {
				body, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), "private-") || strings.Contains(string(body), "unbounded-") {
					t.Fatalf("narrow payload leaked other metadata: %s", body)
				}
				if strings.Contains(string(body), `"caller_attempt"`) != (got != nil) {
					t.Fatalf("optional payload changed legacy shape: %s", body)
				}
			}
		})
	}
}
