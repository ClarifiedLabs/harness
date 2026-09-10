package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"harness/internal/execution"
	"harness/internal/llm"
)

func testLeaseConflict() *BackgroundLeaseConflictError {
	return &BackgroundLeaseConflictError{
		BlockingJobID: "bg_owner", BlockingAgent: "review", BlockingStatus: "completed",
		ResourceKey: "/repo", RequestedAccess: BackgroundAccessExclusive, ActiveAccess: BackgroundAccessReadOnly,
		Guidance: "Wait for unfinished descendants to return before retrying.",
	}
}

func TestBackgroundLeaseConflictErrorWrapping(t *testing.T) {
	base := testLeaseConflict()
	wrapped := fmt.Errorf("launch: %w", WithKind(fmt.Errorf("admission: %w", base), llm.ToolErrorLeaseConflict))
	var conflict *BackgroundLeaseConflictError
	if !errors.As(wrapped, &conflict) || conflict != base || !errors.Is(wrapped, base) {
		t.Fatalf("typed conflict lost through wrappers: %v", wrapped)
	}
	if KindOf(wrapped) != llm.ToolErrorLeaseConflict {
		t.Fatalf("kind = %q", KindOf(wrapped))
	}
	for _, want := range []string{base.BlockingJobID, base.BlockingAgent, base.BlockingStatus, base.ResourceKey, base.RequestedAccess, base.ActiveAccess, base.Guidance} {
		if !strings.Contains(wrapped.Error(), want) {
			t.Errorf("error %q omits %q", wrapped.Error(), want)
		}
	}
	want := llm.LeaseConflictDetails(*base)
	if got := DetailsOf(wrapped); got == nil || got.LeaseConflict == nil || *got.LeaseConflict != want {
		t.Fatalf("details = %+v, want %+v", got, want)
	}
	got := base.ToolErrorDetails()
	got.LeaseConflict.BlockingAgent = "changed"
	if base.BlockingAgent != "review" {
		t.Fatal("typed error details alias error")
	}
}

// This distinct producer verifies DetailsOf uses an interface, not the lease
// error's concrete type, and copies even when the producer returns shared data.
type diagnosticFixtureError struct{ details *llm.ToolErrorDetails }

func (*diagnosticFixtureError) Error() string                             { return "fixture error" }
func (e *diagnosticFixtureError) ToolErrorDetails() *llm.ToolErrorDetails { return e.details }

func TestDetailsOfInterfaceAndOrdinaryErrors(t *testing.T) {
	plain := errors.New("ordinary failure")
	for _, err := range []error{nil, plain, fmt.Errorf("outer: %w", plain), WithKind(plain, llm.ToolErrorBlocked), &diagnosticFixtureError{}} {
		if got := DetailsOf(err); got != nil {
			t.Fatalf("DetailsOf(%v) = %+v, want nil", err, got)
		}
	}
	base := &diagnosticFixtureError{details: testLeaseConflict().ToolErrorDetails()}
	got := DetailsOf(fmt.Errorf("outer: %w", base))
	if !reflect.DeepEqual(got, base.details) || got == base.details || got.LeaseConflict == base.details.LeaseConflict {
		t.Fatalf("DetailsOf did not copy interface details: %+v", got)
	}
	got.LeaseConflict.Guidance = "changed"
	if base.details.LeaseConflict.Guidance == "changed" {
		t.Fatal("DetailsOf mutation changed producer")
	}
}

func TestDispatchLeaseConflictDetails(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprint(typed), func(t *testing.T) {
			var err error = errors.New("ordinary failure")
			var want *llm.ToolErrorDetails
			var producer *diagnosticFixtureError
			if typed {
				producer = &diagnosticFixtureError{details: testLeaseConflict().ToolErrorDetails()}
				want = llm.CloneToolErrorDetails(producer.details)
				err = WithKind(fmt.Errorf("launch: %w", producer), llm.ToolErrorLeaseConflict)
			}
			registry := &Registry{}
			registry.Register(fakeTool{name: "lease_fixture", schema: `{"type":"object"}`, run: func(context.Context, json.RawMessage) (string, error) {
				return "ignored output", err
			}})
			recorder := &workRecorder{}
			result, done := registry.DispatchWithCompletion(workContext(recorder), llm.ToolCall{ID: "call", Name: "lease_fixture", Input: json.RawMessage(`{}`)})
			<-done
			if !result.IsError || result.ForID != "call" || result.Text != err.Error() || !reflect.DeepEqual(result.ErrorDetails, want) {
				t.Fatalf("dispatch = %+v, want text %q, details %+v", result, err.Error(), want)
			}
			wantKind := llm.ToolErrorKind("")
			if typed {
				wantKind = llm.ToolErrorLeaseConflict
				producer.details.LeaseConflict.BlockingAgent = "changed"
				if result.ErrorDetails.LeaseConflict.BlockingAgent != "review" {
					t.Fatal("dispatch result aliases producer details")
				}
			}
			if result.ErrorKind != wantKind {
				t.Fatalf("kind = %q, want %q", result.ErrorKind, wantKind)
			}
			for _, event := range recorder.snapshot() {
				if event.Phase == execution.WorkResult && typed && event.ErrorKind != string(llm.ToolErrorLeaseConflict) {
					t.Fatalf("execution lost lease_conflict kind: %+v", event)
				}
				if strings.Contains(fmt.Sprint(event), "bg_owner") || strings.Contains(fmt.Sprint(event), "/repo") {
					t.Fatalf("lease details leaked into execution event: %+v", event)
				}
			}
		})
	}
}
