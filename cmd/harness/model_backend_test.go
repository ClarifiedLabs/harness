package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy"
	"harness/internal/session"
	"harness/internal/tracing"
	"harness/internal/ui"
)

func modelBackendWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Keep these fixtures independent of the remote-proxy and ACP test helpers:
// local tests must exercise the in-process runtime, not a fake proxy listener.
func modelBackendEnvironment(t *testing.T, args ...string) (environment, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := filepath.Join(dir, "harness.json")
	modelBackendWrite(t, cfg, `{"mcp":{"enable":false,"local":{"enable":false}},"lsp":{"enable":false},"no_env":true}`)
	out, errout := new(bytes.Buffer), new(bytes.Buffer)
	return environment{
		args:  append([]string{"-config", cfg}, args...),
		stdin: strings.NewReader(""), stdout: out, stderr: errout,
		getenv: func(key string) string {
			switch key {
			case "HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME":
				return dir
			}
			return ""
		},
		now: time.Now, agentSleep: func(time.Duration) {},
	}, out, errout, dir
}

func modelBackendLocalConfig(t *testing.T, home, upstream string) string {
	t.Helper()
	path := filepath.Join(home, ".config", "harness-model-proxy", "config.json")
	modelBackendWrite(t, path, `{"provider_configs":["provider.json"],"models_dev_cache_ttl":0,"provider_models_cache_ttl":0,"subscription_poll_interval":0,"api_keys_file":"nonexistent-keys.json","metrics":{"enabled":true,"listen":"not a valid listen address"}}`)
	modelBackendWrite(t, filepath.Join(filepath.Dir(path), "provider.json"), fmt.Sprintf(`{"name":"fake","api_type":"openai","base_url":%q,"api_key":"local-secret","models":[{"name":"model","context_window":8192}]}`, upstream))
	return path
}

func modelBackendEvents(t *testing.T, output string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("non-JSON output %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestLocalModelBackendInformational(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "informational commands must not call upstream", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	for _, command := range []string{"-check-model-proxy", "-models"} {
		for _, format := range []string{"text", "json"} {
			t.Run(command+"/"+format, func(t *testing.T) {
				env, out, errout, home := modelBackendEnvironment(t, "-model-backend", "local", command, "-format", format)
				// Empty ModelConfig discovers the conventional model-proxy config.
				modelBackendLocalConfig(t, home, upstream.URL)
				if code := run(env); code != ui.ExitOK {
					t.Fatalf("exit=%d stderr=%s stdout=%s", code, errout, out)
				}
				if strings.Contains(out.String(), "http://") || strings.Contains(out.String(), "model_proxy_url") {
					t.Errorf("local metadata leaked a proxy URL: %s", out)
				}
				if format == "json" {
					var info infoOutput
					if err := json.Unmarshal(out.Bytes(), &info); err != nil {
						t.Fatal(err)
					}
					if info.ModelCount != 1 {
						t.Errorf("model count=%d: %s", info.ModelCount, out)
					}
					if info.ModelBackend != "local" {
						t.Errorf("model_backend=%q, want local: %s", info.ModelBackend, out)
					}
				} else if command == "-check-model-proxy" && !strings.Contains(out.String(), "model backend ok: local (1 targets)") {
					t.Errorf("missing local status: %s", out)
				} else if command == "-models" && !strings.Contains(out.String(), "fake:model") {
					t.Errorf("missing model: %s", out)
				}
				if _, err := os.Stat(filepath.Join(home, "harness", "sessions")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("informational command created sessions: %v", err)
				}
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("informational upstream requests=%d", requests.Load())
	}
}

func TestLocalModelBackendOneShot(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") || r.Header.Get("Authorization") != "Bearer local-secret" {
					t.Errorf("unexpected upstream request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
				}
				var request struct {
					Model  string `json:"model"`
					Stream bool   `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "model" || !request.Stream {
					t.Errorf("upstream body=%+v error=%v", request, err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"id\":\"local-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"local answer\"}}]}\n\ndata: {\"id\":\"local-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			env, out, errout, home := modelBackendEnvironment(t, "-model-backend", "local", "-model", "fake:model", "-p", "hello", "-format", format)
			path := modelBackendLocalConfig(t, home, upstream.URL)
			env.args = append(env.args, "-model-config", path)
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("exit=%d stderr=%s stdout=%s", code, errout, out)
			}
			if requests.Load() != 1 || !strings.Contains(out.String(), "local answer") {
				t.Fatalf("requests=%d stdout=%s", requests.Load(), out)
			}
			if format == "json" {
				events := modelBackendEvents(t, out.String())
				if events[0]["type"] != "run_start" || events[len(events)-1]["type"] != "run_end" || events[len(events)-1]["exit_code"] != float64(0) || errout.Len() != 0 {
					t.Fatalf("invalid run stream: %v stderr=%s", events, errout)
				}
			}
		})
	}
}

func TestModelBackendOfflineCommands(t *testing.T) {
	for _, args := range [][]string{{"-agents"}, {"-agents", "-format", "json"}, {"-help"}, {"config", "list"}, {"config", "show"}, {"config", "check"}, {"session", "ls"}, {"session", "stats"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			env, out, errout, home := modelBackendEnvironment(t)
			cfg := env.args[1]
			if args[0] == "session" || args[0] == "config" {
				env.args = append([]string{}, args...)
				if args[0] == "config" && args[1] != "list" {
					env.args = append(env.args, "-config", cfg)
				}
				if args[0] == "session" && args[1] == "stats" {
					path := filepath.Join(home, "recorded")
					state := session.Session{Provider: "fake", Model: "fake:model", Created: time.Now(), Updated: time.Now()}
					if err := state.Save(path); err != nil {
						t.Fatal(err)
					}
					modelBackendWrite(t, filepath.Join(path, "raw.ndjson"), "")
					env.args = append(env.args, path)
				}
			} else {
				env.args = append(env.args, args...)
				env.args = append(env.args, "-model-backend", "local", "-model-config", filepath.Join(home, "missing.json"))
			}
			env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
				t.Error("offline command opened a model backend")
				return nil, errors.New("unexpected backend open")
			}
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("exit=%d stderr=%s stdout=%s", code, errout, out)
			}
		})
	}
}

func TestModelBackendRemoteDoesNotReadLocalConfigOrFallback(t *testing.T) {
	for _, mode := range []string{"default", "remote"} {
		for _, available := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/available=%v", mode, available), func(t *testing.T) {
				var requests atomic.Int32
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if !available {
						http.Error(w, "remote unavailable", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(modelproxy.Catalog{Targets: []modelproxy.Target{{ID: "remote:model", APIType: "openai"}}})
				}))
				defer remote.Close()
				env, out, errout, home := modelBackendEnvironment(t, "-check-model-proxy", "-format", "json", "-model-proxy-url", remote.URL)
				if mode == "remote" {
					env.args = append(env.args, "-model-backend", "remote")
				}
				path := modelBackendLocalConfig(t, home, "http://127.0.0.1:1")
				if available {
					// Success proves malformed ModelConfig is never read in remote mode.
					modelBackendWrite(t, path, "not JSON")
				}
				env.args = append(env.args, "-model-config", path)
				code := run(env)
				want := ui.ExitRuntime
				if available {
					want = ui.ExitOK
				}
				if code != want || requests.Load() != 1 {
					t.Fatalf("exit=%d want=%d requests=%d stderr=%s stdout=%s", code, want, requests.Load(), errout, out)
				}
				if available {
					var info infoOutput
					if err := json.Unmarshal(out.Bytes(), &info); err != nil {
						t.Fatal(err)
					}
					if info.ModelBackend != "remote" || info.ModelProxyURL != remote.URL {
						t.Fatalf("remote metadata=%+v", info)
					}
				} else if strings.Contains(out.String(), "local") || !strings.Contains(errout.String(), "remote unavailable") {
					t.Fatalf("remote failure was hidden: stdout=%s stderr=%s", out, errout)
				}
			})
		}
	}
}

type modelBackendFake struct {
	provider     llm.Provider
	catalogError error
}

func (b modelBackendFake) Catalog(context.Context) (modelproxy.Catalog, error) {
	return modelproxy.Catalog{Targets: []modelproxy.Target{{ID: "fake:model", APIType: "openai", ContextWindow: 8192}}}, b.catalogError
}
func (b modelBackendFake) Provider(string) llm.Provider { return b.provider }
func (b modelBackendFake) Limits(context.Context, string) (modelproxy.LimitsReport, error) {
	return modelproxy.LimitsReport{}, nil
}
func (b modelBackendFake) ResetCredits(context.Context, string) (modelproxy.ResetCredits, error) {
	return modelproxy.ResetCredits{}, nil
}
func (b modelBackendFake) ResetLimits(context.Context, modelproxy.ResetRequest) (modelproxy.ResetResult, error) {
	return modelproxy.ResetResult{}, nil
}

func TestModelBackendClosedOnce(t *testing.T) {
	for _, failure := range []string{"success", "catalog", "selection"} {
		t.Run(failure, func(t *testing.T) {
			env, out, errout, _ := modelBackendEnvironment(t, "-model", "fake:model", "-p", "hello")
			fake := modelBackendFake{provider: llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})}
			if failure == "catalog" {
				fake.catalogError = errors.New("catalog unavailable")
			}
			if failure == "selection" {
				env.args = append(env.args, "-model", "fake:missing")
			}
			var opened, closed atomic.Int32
			var owned *openedModelBackend
			env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
				opened.Add(1)
				owned = &openedModelBackend{Backend: fake, Accounts: fake, Mode: "local", close: func() error { closed.Add(1); return nil }}
				return owned, nil
			}
			code := run(env)
			want := map[string]int{"success": ui.ExitOK, "catalog": ui.ExitRuntime, "selection": ui.ExitUsage}[failure]
			if code != want || opened.Load() != 1 || closed.Load() != 1 {
				t.Fatalf("exit=%d want=%d opened=%d closed=%d stderr=%s stdout=%s", code, want, opened.Load(), closed.Load(), errout, out)
			}
			if err := owned.Close(); err != nil || closed.Load() != 1 {
				t.Fatalf("repeated close: count=%d error=%v", closed.Load(), err)
			}
		})
	}
}

func TestModelBackendTurnInterruptionKeepsBackendUsable(t *testing.T) {
	env, out, errout, _ := modelBackendEnvironment(t, "-model", "fake:model", "-format", "json")
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	env.stdin, env.stdinPiped = input, true
	started := make(chan struct{})
	finished := make(chan struct{}, 2)
	var closed, opened atomic.Int32
	provider := llmtest.New("fake", llmtest.Step{Block: func(ctx context.Context) { close(started); <-ctx.Done() }}, llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "next turn works"}}, Stop: llm.StopEndTurn,
	})
	env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
		opened.Add(1)
		return &openedModelBackend{Backend: modelBackendFake{provider: provider}, Accounts: modelBackendFake{}, Mode: "local", close: func() error { closed.Add(1); return nil }}, nil
	}
	env.promptFinished = func() { finished <- struct{}{} }
	done := make(chan int, 1)
	go func() { done <- run(env) }()
	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(writer, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"type":"prompt","id":"first","text":"wait"}`)
	<-started
	write(`{"type":"interrupt"}`)
	<-finished
	if closed.Load() != 0 {
		t.Fatal("turn interruption closed application backend")
	}
	write(`{"type":"prompt","id":"second","text":"continue"}`)
	<-finished
	if closed.Load() != 0 {
		t.Fatal("second turn closed application backend")
	}
	write(`{"type":"shutdown"}`)
	if code := <-done; code != ui.ExitOK || opened.Load() != 1 || closed.Load() != 1 || provider.RequestCount() != 2 {
		t.Fatalf("exit=%d opened=%d closed=%d calls=%d stderr=%s stdout=%s", code, opened.Load(), closed.Load(), provider.RequestCount(), errout, out)
	}
	var ends []map[string]any
	for _, event := range modelBackendEvents(t, out.String()) {
		if event["type"] == "prompt_end" {
			ends = append(ends, event)
		}
	}
	if len(ends) != 2 || ends[0]["exit_code"] != float64(ui.ExitInterrupt) || ends[1]["final_text"] != "next turn works" || ends[1]["exit_code"] != float64(0) {
		t.Fatalf("prompt ends=%v", ends)
	}
}
