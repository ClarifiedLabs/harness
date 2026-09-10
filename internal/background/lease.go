package background

import "context"

// Only Manager.start installs this key, on the accepted worker's independently
// cancelable context. IDs, resource strings, and delegate metadata grant no trust.
type jobContextKey struct{}

func (m *Manager) ancestorsLocked(ctx context.Context) []*Job {
	if ctx == nil {
		return nil
	}
	parent, _ := ctx.Value(jobContextKey{}).(*Job)
	if parent == nil || m.jobs[parent.ID] != parent || parent.finished || parent.Status != StatusRunning {
		return nil
	}
	ancestors := make([]*Job, 0, len(parent.ancestors)+1)
	ancestors = append(ancestors, parent.ancestors...)
	return append(ancestors, parent)
}

func isAncestor(ancestors []*Job, job *Job) bool {
	for _, ancestor := range ancestors {
		if ancestor == job {
			return true
		}
	}
	return false
}

func (m *Manager) acquireLeaseLocked(job *Job) {
	if job.ResourceKey == "" {
		return
	}
	job.leaseUsers = 1
	m.leases = append(m.leases, job)
	for _, ancestor := range job.ancestors {
		// Retain only leases actually reused. Distinct canonical resources never
		// become hierarchical locks, even when their paths overlap.
		if ancestor.ResourceKey == job.ResourceKey && ancestor.leaseUsers > 0 {
			ancestor.leaseUsers++
		}
	}
}

func (m *Manager) releaseLeaseLocked(job *Job) {
	if job.ResourceKey == "" {
		return
	}
	job.leaseUsers--
	for _, ancestor := range job.ancestors {
		if ancestor.ResourceKey == job.ResourceKey && ancestor.leaseUsers > 0 {
			ancestor.leaseUsers--
		}
	}
	// Release on actual worker return, not logical cancellation/abandonment.
	// Clear resets the visible job table, but cannot release still-live leases.
	kept := m.leases[:0]
	for _, owner := range m.leases {
		if owner.leaseUsers > 0 {
			kept = append(kept, owner)
		}
	}
	clear(m.leases[len(kept):])
	m.leases = kept
}
