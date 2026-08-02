package sessionrec

import (
	"reflect"
	"testing"

	"harness/internal/agent"
	"harness/internal/session"
)

func TestWorkflowStatusSnapshotDistinguishesAvailabilityAndOutcomes(t *testing.T) {
	remaining := 3
	tests := []struct {
		name   string
		status agent.WorkflowStatus
		want   *session.WorkflowStatusSnapshot
	}{
		{name: "absent", status: agent.WorkflowStatus{}, want: nil},
		{name: "complete", status: agent.WorkflowStatus{Available: true, Outcome: agent.WorkflowOutcomeComplete}, want: &session.WorkflowStatusSnapshot{Outcome: "complete"}},
		{name: "blocked", status: agent.WorkflowStatus{Available: true, Outcome: agent.WorkflowOutcomeBlocked, RemainingRequirements: &remaining}, want: &session.WorkflowStatusSnapshot{Outcome: "blocked", RemainingRequirements: &remaining}},
		{name: "expected wait", status: agent.WorkflowStatus{Available: true, Outcome: agent.WorkflowOutcomeWaiting, ExpectedWait: true}, want: &session.WorkflowStatusSnapshot{Outcome: "waiting", ExpectedWait: true}},
		{name: "explicit unknown", status: agent.WorkflowStatus{Available: true, Outcome: agent.WorkflowOutcomeUnknown}, want: &session.WorkflowStatusSnapshot{Outcome: "unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WorkflowStatusSnapshot(tt.status); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("WorkflowStatusSnapshot() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRetentionSnapshotPreservesCausalFields(t *testing.T) {
	event := agent.RetentionEvent{
		Policy: "pressure_epoch", Trigger: "context_pressure", BlocksTrimmed: 2,
		BytesBefore: 100, BytesAfter: 40, BytesRemoved: 60,
		ContextTokensBefore: 90, ContextTokensAfter: 20,
		DecisionContextTokens: 90, DecisionContextSource: agent.ContextEstimateSourceResponseUsageDelta,
		LocalEstimateTokensBefore: 50, LocalEstimateTokensAfter: 20, EstimatedTokensRemoved: 30,
		MeasurementAnchorReset: true, ContinuationStatePresent: true, ContinuationStateReset: true,
		PreviousRequestMode: agent.RetentionRequestModeStatefulSuffix, NextRequestMode: agent.RetentionRequestModeFull,
	}
	want := &session.RetentionSnapshot{
		Policy: "pressure_epoch", Trigger: "context_pressure", BlocksTrimmed: 2,
		BytesBefore: 100, BytesAfter: 40, BytesRemoved: 60,
		ContextTokensBefore: 90, ContextTokensAfter: 20,
		DecisionContextTokens: 90, DecisionContextSource: agent.ContextEstimateSourceResponseUsageDelta,
		LocalEstimateTokensBefore: 50, LocalEstimateTokensAfter: 20, EstimatedTokensRemoved: 30,
		MeasurementAnchorReset: true, ContinuationStatePresent: true, ContinuationStateReset: true,
		PreviousRequestMode: "stateful_suffix", NextRequestMode: "full",
	}
	if got := RetentionSnapshot(event); !reflect.DeepEqual(got, want) {
		t.Fatalf("RetentionSnapshot() = %+v, want %+v", got, want)
	}
}
