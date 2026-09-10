package background

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"harness/internal/llm"
	"harness/internal/tools"
)

func holdDiagnosticLease(t *testing.T, m *Manager, resource, access string) heldLease {
	t.Helper()
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	info, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Agent: " review ", ResourceKey: resource, Access: access,
		Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			started <- ctx
			<-release
			return tools.BackgroundJobResult{}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	done := m.jobs[info.ID].done
	m.mu.Unlock()
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }); <-done }
	t.Cleanup(finish)
	return heldLease{info: info, ctx: <-started, finish: finish}
}

func requireLeaseDiagnostic(t *testing.T, m *Manager, owner heldLease, requestedAccess, status string) {
	t.Helper()
	before := len(m.List())
	_, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		Agent: "requester", ResourceKey: owner.info.ResourceKey + "/", Access: requestedAccess,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, nil
		},
	})
	var conflict *tools.BackgroundLeaseConflictError
	if !errors.As(fmt.Errorf("launch: %w", err), &conflict) {
		t.Fatalf("conflict = %v, want typed lease error", err)
	}
	if tools.KindOf(err) != llm.ToolErrorLeaseConflict {
		t.Fatalf("kind = %q", tools.KindOf(err))
	}
	if conflict.BlockingJobID != owner.info.ID || conflict.BlockingAgent != "review" ||
		conflict.BlockingStatus != status || conflict.ResourceKey != owner.info.ResourceKey ||
		conflict.RequestedAccess != requestedAccess || conflict.ActiveAccess != owner.info.Access {
		t.Fatalf("wrong conflict fields: %+v", conflict)
	}
	for _, want := range []string{"background_jobs", "descendants", "cancellation cleanup", "waiting for a completed owner alone will not release it"} {
		if !strings.Contains(conflict.Guidance, want) || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing safe guidance %q in %v", want, err)
		}
	}
	for _, forbidden := range []string{"change resource", "different resource", "switch to read_only", "downgrade"} {
		if strings.Contains(strings.ToLower(conflict.Guidance), forbidden) {
			t.Fatalf("unsafe bypass guidance: %s", conflict.Guidance)
		}
	}
	if details := tools.DetailsOf(err); details == nil || details.LeaseConflict == nil || *details.LeaseConflict != llm.LeaseConflictDetails(*conflict) {
		t.Fatalf("missing/mismatched error details: %+v", details)
	}
	if after := len(m.List()); after != before {
		t.Fatalf("rejected job registered: before %d after %d", before, after)
	}
}

func TestManagerLeaseConflictDiagnostics(t *testing.T) {
	for _, active := range []string{tools.BackgroundAccessExclusive, tools.BackgroundAccessReadOnly} {
		t.Run(active, func(t *testing.T) {
			m := NewManager(Options{})
			t.Cleanup(m.Shutdown)
			resource := t.TempDir()
			owner := holdDiagnosticLease(t, m, resource, active)
			if active == tools.BackgroundAccessReadOnly {
				// Preserve deterministic first-owner selection among active readers.
				holdLease(t, m, nil, resource, tools.BackgroundAccessReadOnly)
			} else {
				requireLeaseDiagnostic(t, m, owner, tools.BackgroundAccessReadOnly, StatusRunning)
			}
			requireLeaseDiagnostic(t, m, owner, tools.BackgroundAccessExclusive, StatusRunning)
		})
	}
}

func TestManagerLeaseConflictReservationCleanupDiagnostics(t *testing.T) {
	for _, scenario := range []string{"completed ancestor", "canceled owner", "cleared ancestor", "abandoned ancestor"} {
		t.Run(scenario, func(t *testing.T) {
			m := NewManager(Options{})
			t.Cleanup(m.Shutdown)
			resource := t.TempDir()
			owner := holdDiagnosticLease(t, m, resource, tools.BackgroundAccessExclusive)
			child := holdLease(t, m, owner.ctx, resource, tools.BackgroundAccessReadOnly)
			status := StatusCanceled
			switch scenario {
			case "completed ancestor":
				owner.finish()
				status = StatusCompleted
			case "canceled owner":
				m.Cancel(owner.info.ID)
				<-owner.ctx.Done()
			case "cleared ancestor":
				m.Clear()
				status = StatusAbandoned
			case "abandoned ancestor":
				m.Shutdown()
				m.Clear()
				status = StatusAbandoned
			}
			requireLeaseDiagnostic(t, m, owner, tools.BackgroundAccessReadOnly, status)
			owner.finish()
			// The first reservation owner remains selected after it returns;
			// selecting only the live reader would wrongly admit another reader.
			requireLeaseDiagnostic(t, m, owner, tools.BackgroundAccessReadOnly, status)
			child.finish()
			after := holdLease(t, m, nil, resource, tools.BackgroundAccessExclusive)
			after.finish()
		})
	}
}
