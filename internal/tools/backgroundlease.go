package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"harness/internal/llm"
)

const (
	BackgroundAccessReadOnly  = "read_only"
	BackgroundAccessExclusive = "exclusive"
)

// BackgroundLeaseConflictError identifies the reservation that rejected a
// background launch. BlockingJobID is the reservation owner, not necessarily a
// running worker: descendants may retain a completed owner's reservation.
// Manager admission wraps this error with ToolErrorLeaseConflict.
type BackgroundLeaseConflictError llm.LeaseConflictDetails

func (e *BackgroundLeaseConflictError) Error() string {
	return fmt.Sprintf(
		"background resource %q access %q conflicts with reservation owned by job %s (agent %q, status %q, access %q). %s",
		e.ResourceKey, e.RequestedAccess, e.BlockingJobID, e.BlockingAgent, e.BlockingStatus, e.ActiveAccess, e.Guidance,
	)
}

// ToolErrorDetails returns an independently owned diagnostics snapshot.
func (e *BackgroundLeaseConflictError) ToolErrorDetails() *llm.ToolErrorDetails {
	if e == nil {
		return nil
	}
	details := llm.LeaseConflictDetails(*e)
	return &llm.ToolErrorDetails{LeaseConflict: &details}
}

// DefaultBackgroundResource returns the canonical working directory used when
// a local background tool does not name a narrower resource explicitly.
func DefaultBackgroundResource(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve background resource cwd: %w", err)
		}
	}
	return CanonicalBackgroundResource(cwd)
}

// ResolveBackgroundLease applies caller-specific defaults and returns a
// canonical resource/access pair. Empty defaults preserve an unleased job.
func ResolveBackgroundLease(resourceKey, access, defaultResourceKey, defaultAccess string) (string, string, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	access = strings.TrimSpace(access)
	if resourceKey == "" {
		resourceKey = strings.TrimSpace(defaultResourceKey)
	}
	if access == "" {
		access = strings.TrimSpace(defaultAccess)
	}
	return NormalizeBackgroundLease(resourceKey, access)
}

// NormalizeBackgroundLease validates and canonicalizes a background job lease.
// A fully empty pair is valid for work without a local shared resource.
func NormalizeBackgroundLease(resourceKey, access string) (string, string, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	access = strings.TrimSpace(access)
	if resourceKey == "" && access == "" {
		return "", "", nil
	}
	if resourceKey == "" {
		return "", "", fmt.Errorf("resource_key is required when access is set")
	}
	if access == "" {
		return "", "", fmt.Errorf("access is required when resource_key is set")
	}
	switch access {
	case BackgroundAccessReadOnly, BackgroundAccessExclusive:
	default:
		return "", "", fmt.Errorf(
			"access must be %q or %q",
			BackgroundAccessReadOnly,
			BackgroundAccessExclusive,
		)
	}
	canonical, err := CanonicalBackgroundResource(resourceKey)
	if err != nil {
		return "", "", err
	}
	return canonical, access, nil
}

// CanonicalBackgroundResource converts a path-like resource key to a stable
// absolute path. It resolves symlinks through the longest existing prefix so a
// not-yet-created child below a symlinked worktree still shares the same key.
func CanonicalBackgroundResource(resourceKey string) (string, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return "", fmt.Errorf("resource_key is required")
	}
	absolute, err := filepath.Abs(resourceKey)
	if err != nil {
		return "", fmt.Errorf("canonicalize resource_key %q: %w", resourceKey, err)
	}
	absolute = filepath.Clean(absolute)

	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("canonicalize resource_key %q: %w", resourceKey, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
