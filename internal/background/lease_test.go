package background

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"harness/internal/tools"
)

type heldLease struct {
	info   tools.BackgroundJobInfo
	ctx    context.Context
	finish func()
}

// A held worker deliberately ignores cancellation until cleanup is released.
func holdLease(t *testing.T, m *Manager, admission context.Context, resource, access string) heldLease {
	t.Helper()
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	info, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		AdmissionContext: admission, ResourceKey: resource, Access: access,
		Run: func(ctx context.Context, _ string) (tools.BackgroundJobResult, error) {
			started <- ctx
			<-release
			return tools.BackgroundJobResult{}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("start %s lease: %v", access, err)
	}
	m.mu.Lock()
	done := m.jobs[info.ID].done
	m.mu.Unlock()
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }); <-done }
	t.Cleanup(finish)
	return heldLease{info: info, ctx: <-started, finish: finish}
}

func assertLeaseConflict(t *testing.T, m *Manager, admission context.Context, resource, access string) {
	t.Helper()
	_, err := m.StartBackgroundJob(tools.BackgroundJobRequest{
		AdmissionContext: admission, ResourceKey: resource, Access: access,
		Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
			return tools.BackgroundJobResult{}, nil
		},
	})
	if err == nil {
		t.Fatalf("%s lease unexpectedly admitted", access)
	}
}

func TestManagerAncestorLeaseReuse(t *testing.T) {
	for _, parentAccess := range []string{tools.BackgroundAccessExclusive, tools.BackgroundAccessReadOnly} {
		for _, childAccess := range []string{tools.BackgroundAccessExclusive, tools.BackgroundAccessReadOnly} {
			t.Run(parentAccess+"/"+childAccess, func(t *testing.T) {
				m := NewManager(Options{})
				t.Cleanup(m.Shutdown)
				resource := t.TempDir()
				parent := holdLease(t, m, nil, resource, parentAccess)
				child := holdLease(t, m, parent.ctx, resource+"/", childAccess)
				// Every genuine ancestor must be exempt, not only the direct parent.
				grandchild := holdLease(t, m, child.ctx, resource, tools.BackgroundAccessExclusive)
				for _, access := range []string{tools.BackgroundAccessExclusive, tools.BackgroundAccessReadOnly} {
					assertLeaseConflict(t, m, nil, resource, access)
					assertLeaseConflict(t, m, parent.ctx, resource, access) // uncle of EX grandchild
				}
				grandchild.finish()
				for _, siblingAccess := range []string{tools.BackgroundAccessExclusive, tools.BackgroundAccessReadOnly} {
					if childAccess == tools.BackgroundAccessReadOnly && siblingAccess == tools.BackgroundAccessReadOnly {
						sibling := holdLease(t, m, parent.ctx, resource, siblingAccess)
						sibling.finish()
					} else {
						assertLeaseConflict(t, m, parent.ctx, resource, siblingAccess)
					}
				}
				if parentAccess == tools.BackgroundAccessReadOnly && childAccess == tools.BackgroundAccessReadOnly {
					other := holdLease(t, m, nil, resource, tools.BackgroundAccessReadOnly)
					// An ancestor exemption cannot bypass an unrelated reader.
					assertLeaseConflict(t, m, child.ctx, resource, tools.BackgroundAccessExclusive)
					other.finish()
				}
				// Distinct canonical paths still have exact-key, not hierarchical locks.
				other := holdLease(t, m, nil, filepath.Join(resource, "subdir"), tools.BackgroundAccessExclusive)
				other.finish()
			})
		}
	}
}

func TestManagerAncestorLeaseOutlivesParent(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "cancellation"}[cancelParent], func(t *testing.T) {
			m := NewManager(Options{})
			t.Cleanup(m.Shutdown)
			resource := t.TempDir()
			parent := holdLease(t, m, nil, resource, tools.BackgroundAccessExclusive)
			child := holdLease(t, m, parent.ctx, resource, tools.BackgroundAccessReadOnly)
			if cancelParent {
				m.Cancel(parent.info.ID)
				<-parent.ctx.Done()
				// Removing cancellation does not revive an inactive ancestor's authority.
				assertLeaseConflict(t, m, context.WithoutCancel(parent.ctx), resource, tools.BackgroundAccessExclusive)
			}
			parent.finish()
			if err := child.ctx.Err(); err != nil {
				t.Fatalf("parent lifetime canceled child: %v", err)
			}
			// The EX ancestor's protection survives even when the child only reads.
			for _, access := range []string{tools.BackgroundAccessExclusive, tools.BackgroundAccessReadOnly} {
				assertLeaseConflict(t, m, nil, resource, access)
			}
			assertLeaseConflict(t, m, context.WithoutCancel(parent.ctx), resource, tools.BackgroundAccessReadOnly)
			grandchild := holdLease(t, m, child.ctx, resource, tools.BackgroundAccessReadOnly)
			m.Cancel(child.info.ID)
			<-child.ctx.Done()
			assertLeaseConflict(t, m, nil, resource, tools.BackgroundAccessReadOnly)
			child.finish()
			if err := grandchild.ctx.Err(); err != nil {
				t.Fatalf("child cancellation leaked: %v", err)
			}
			assertLeaseConflict(t, m, nil, resource, tools.BackgroundAccessReadOnly)
			m.Cancel(grandchild.info.ID)
			<-grandchild.ctx.Done()
			assertLeaseConflict(t, m, nil, resource, tools.BackgroundAccessReadOnly)
			grandchild.finish()
			after := holdLease(t, m, nil, resource, tools.BackgroundAccessExclusive)
			after.finish()
		})
	}
}

func TestManagerAncestorLeaseClearRetainsCleanup(t *testing.T) {
	m := NewManager(Options{})
	t.Cleanup(m.Shutdown)
	resource := t.TempDir()
	parent := holdLease(t, m, nil, resource, tools.BackgroundAccessExclusive)
	child := holdLease(t, m, parent.ctx, resource, tools.BackgroundAccessReadOnly)
	m.Clear()
	<-parent.ctx.Done()
	<-child.ctx.Done()
	if jobs := m.List(); len(jobs) != 0 {
		t.Fatalf("clear retained visible jobs: %+v", jobs)
	}
	assertLeaseConflict(t, m, nil, resource, tools.BackgroundAccessExclusive)
	assertLeaseConflict(t, m, context.WithoutCancel(child.ctx), resource, tools.BackgroundAccessReadOnly)
	parent.finish()
	assertLeaseConflict(t, m, nil, resource, tools.BackgroundAccessReadOnly)
	child.finish()
	after := holdLease(t, m, nil, resource, tools.BackgroundAccessExclusive)
	after.finish()
}

func TestManagerAncestorLeaseRejectsUntrustedLineage(t *testing.T) {
	m := NewManager(Options{})
	t.Cleanup(m.Shutdown)
	resource := t.TempDir()
	parent := holdLease(t, m, nil, resource, tools.BackgroundAccessExclusive)
	// Model-supplied IDs/paths do not carry the private manager capability.
	for _, key := range []string{"parent_id", "job_id", "resource_key"} {
		ctx := context.WithValue(context.Background(), key, parent.info.ID)
		assertLeaseConflict(t, m, ctx, resource, tools.BackgroundAccessExclusive)
	}
	// Even the right private key with a fabricated job fails pointer identity.
	forged := context.WithValue(context.Background(), jobContextKey{}, &Job{ID: parent.info.ID, Status: StatusRunning})
	assertLeaseConflict(t, m, forged, resource, tools.BackgroundAccessExclusive)
	otherManager := NewManager(Options{})
	t.Cleanup(otherManager.Shutdown)
	otherParent := holdLease(t, otherManager, nil, resource, tools.BackgroundAccessExclusive)
	assertLeaseConflict(t, m, otherParent.ctx, resource, tools.BackgroundAccessExclusive)
}
