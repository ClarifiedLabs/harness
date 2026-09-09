package server

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestHandler writes a provider config and constructs a handler expected to
// succeed. Only missing config wiring is defaulted; scenario options stay with
// the caller. Tests own HTTP servers and middleware, so their deferred closes
// run before handler cleanup and removal of the temporary config directory.
func newTestHandler(t *testing.T, filename, providerJSON string, opts Options) *Handler {
	t.Helper()
	if opts.ConfigDir == "" {
		opts.ConfigDir = t.TempDir()
	}
	if err := os.WriteFile(filepath.Join(opts.ConfigDir, filename), []byte(providerJSON), 0o600); err != nil {
		t.Fatalf("write provider config: %v", err)
	}
	if opts.Config.ProviderConfigs == nil {
		opts.Config.ProviderConfigs = []string{filename}
	}
	handler, err := NewHandler(opts)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() {
		if err := handler.Close(); err != nil {
			t.Errorf("close handler: %v", err)
		}
	})
	return handler
}
