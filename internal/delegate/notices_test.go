package delegate

import (
	"reflect"
	"testing"

	"harness/internal/agent"
)

// TestSafeFixedNoticesMatchAgentConstants guards the production coupling: the
// delegate activity allowlist is built from agent notice constants, so the set
// must stay exactly the fixed notices the agent emits.
func TestSafeFixedNoticesMatchAgentConstants(t *testing.T) {
	want := map[string]bool{
		agent.NoticeCancelled:                      true,
		agent.NoticeStoppedMaxTokens:               true,
		agent.NoticeStoppedStopSequence:            true,
		agent.NoticeContextOverflowCompacting:      true,
		agent.NoticeResponsesStateDisabledRejected: true,
		agent.NoticeResponsesStateResetUnavailable: true,
		agent.NoticeCompactNothingToShrink:         true,
	}
	if !reflect.DeepEqual(safeFixedNotices, want) {
		t.Fatalf("safeFixedNotices = %v, want %v", safeFixedNotices, want)
	}
}
