package runtime

import (
	"context"
	"harness/internal/modelproxy"
	modelconfig "harness/internal/modelproxy/config"
	"harness/internal/modelproxy/server/httpadapter"
	"net/http"
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

type Handler struct{ *Runtime }

func NewHandler(opts Options) (*Handler, error) {
	r, e := New(opts)
	if e != nil {
		return nil, e
	}
	return &Handler{r}, nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpadapter.New(h.Runtime, h.configDir).ServeHTTP(w, r)
}
func (h *Handler) Catalog() modelproxy.Catalog {
	c, _ := h.Runtime.Catalog(context.Background())
	return c
}
func ObserveAuth(h *Handler, store interface{ Authorize(*http.Request) bool }, next http.Handler) http.Handler {
	return httpadapter.ObserveAuth(httpadapter.New(h.Runtime, h.configDir), store, next)
}
func ValidateConfigReferences(dir string, cfg modelconfig.Config, getenv func(string) string, warn func(string)) error {
	return modelconfig.ValidateConfigReferences(dir, cfg, getenv, warn)
}

const streamPath = "/v1/stream"
const compactPath = "/v1/compact"
