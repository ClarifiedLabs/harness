//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the actual shipped executable with neither proxy available on PATH.
// The only HTTP server is the upstream provider, not a harness model proxy.
func TestSmokeLocalBackendWithoutProxyBinaries(t *testing.T) {
	built := buildBinary(t)
	binDir := t.TempDir()
	binary := filepath.Join(binDir, "harness")
	data, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") || r.Header.Get("Authorization") != "Bearer local-secret" {
					t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"local-smoke\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"consolidated answer\"}}]}\n\ndata: {\"id\":\"local-smoke\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			home := t.TempDir()
			modelConfig := modelBackendLocalConfig(t, home, provider.URL)
			harnessConfig := filepath.Join(home, "harness.json")
			modelBackendWrite(t, harnessConfig, `{"mcp":{"enable":false,"local":{"enable":false}},"lsp":{"enable":false},"no_env":true}`)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-config", harnessConfig, "-model-backend", "local", "-model-config", modelConfig, "-model", "fake:model", "-p", "hello", "-format", format, "-q")
			cmd.Dir = home
			cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "PATH=" + binDir, "NO_COLOR=1"}
			var out, errout bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &errout
			if err := cmd.Run(); err != nil {
				t.Fatalf("harness: %v\nstdout: %s\nstderr: %s", err, &out, &errout)
			}
			if calls.Load() != 1 || !strings.Contains(out.String(), "consolidated answer") {
				t.Fatalf("calls=%d stdout=%s stderr=%s", calls.Load(), &out, &errout)
			}
			if format == "json" {
				events := modelBackendEvents(t, out.String())
				if events[0]["type"] != "run_start" || events[len(events)-1]["type"] != "run_end" || events[len(events)-1]["exit_code"] != float64(0) {
					t.Fatalf("run stream=%v", events)
				}
			}
		})
	}
}
