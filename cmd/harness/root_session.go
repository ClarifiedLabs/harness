package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"harness/internal/config"
	"harness/internal/session"
	"harness/internal/taskcontext"
)

type rootSessionStartup struct {
	resumed   *session.Session
	created   time.Time
	path      string
	cloned    bool
	cloneFrom string
	cloneTo   string
}

// prepareRootSession loads, recovers, and optionally clones the startup session.
// The caller owns locks and keeps them until shutdown, including on error.
func prepareRootSession(runOptions config.RunOptions, getenv func(string) string, now func() time.Time, stderr io.Writer, locks *rootSessionLocks) (rootSessionStartup, error) {
	// Load a resumed session up front: its saved agent selects the tool set and
	// any agent-specific model target when no -agent flag overrides it.
	var resumed *session.Session
	if runOptions.Resume != "" {
		if err := locks.switchTo(runOptions.Resume); err != nil {
			return rootSessionStartup{}, fmt.Errorf("resume %s: %w", runOptions.Resume, err)
		}
		s, err := session.Load(runOptions.Resume)
		if err != nil {
			return rootSessionStartup{}, fmt.Errorf("resume %s: %w", runOptions.Resume, err)
		}
		resumed = &s
		if s.Recovery != nil {
			fmt.Fprintf(
				stderr,
				"[recovered active session boundary: %s, prompt %d, turn %d]\n",
				s.Recovery.Phase,
				s.Recovery.Prompt,
				s.Recovery.Turn,
			)
		}
		if s.RecoveryWarning != "" {
			fmt.Fprintf(stderr, "[ignored unreadable active-turn checkpoint: %s]\n", s.RecoveryWarning)
		}
	}
	startedAt := now()
	created := startedAt
	if resumed != nil && !resumed.Created.IsZero() {
		created = resumed.Created
	}
	if resumed != nil {
		abandoned, skipped, err := session.AbandonRunningChildren(runOptions.Resume, startedAt)
		if err != nil {
			return rootSessionStartup{}, fmt.Errorf("resume child sessions: %w", err)
		}
		if abandoned > 0 {
			fmt.Fprintf(stderr, "[marked %d interrupted child session(s) abandoned]\n", abandoned)
		}
		if skipped > 0 {
			fmt.Fprintf(stderr, "[skipped %d unreadable child session(s)]\n", skipped)
		}
	}
	sessionPath := runOptions.Session
	if sessionPath == "" {
		if runOptions.Resume != "" {
			sessionPath = runOptions.Resume
		} else {
			sessionPath = session.DefaultPath(stateDir(getenv), created)
		}
	}
	// Debug requests do not persist a new session. A debug request with -resume
	// still holds the source lock because loading it may perform recovery and child
	// cleanup. All ordinary runs lock their write destination before setup proceeds.
	if !runOptions.DebugRequest && (runOptions.Resume == "" || filepath.Clean(sessionPath) != filepath.Clean(runOptions.Resume)) {
		if err := locks.switchTo(sessionPath); err != nil {
			return rootSessionStartup{}, fmt.Errorf("session %s: %w", sessionPath, err)
		}
	}
	resumeCloned := false
	resumeCloneFrom := ""
	resumeCloneTo := ""
	if resumed != nil && runOptions.Session != "" && filepath.Clean(runOptions.Session) != filepath.Clean(runOptions.Resume) {
		clone, err := cloneSessionForResume(resumed, now)
		if err != nil {
			return rootSessionStartup{}, fmt.Errorf("clone resumed session: %w", err)
		}
		// The destination is locked above for ordinary runs. Debug requests only
		// clone in memory and must not create notes or other destination files.
		if !runOptions.DebugRequest {
			if err := taskcontext.CopyNotes(context.Background(), runOptions.Resume, sessionPath); err != nil {
				return rootSessionStartup{}, fmt.Errorf("clone resumed session: copy task notes: %w", err)
			}
		}
		created = clone.Created
		resumeCloneFrom = clone.From
		resumeCloneTo = clone.To
		resumeCloned = true
	}
	return rootSessionStartup{
		resumed: resumed, created: created, path: sessionPath,
		cloned: resumeCloned, cloneFrom: resumeCloneFrom, cloneTo: resumeCloneTo,
	}, nil
}
