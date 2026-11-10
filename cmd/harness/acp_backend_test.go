package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/agent"
	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy"
	"harness/internal/tools"
	"harness/internal/tracing"
)

type acpTestBackend struct{ provider llm.Provider }

func (b acpTestBackend) Catalog(context.Context) (modelproxy.Catalog, error) {
	return modelproxy.Catalog{Targets: []modelproxy.Target{{ID: "fake:model", APIType: "openai", ContextWindow: 8192}}}, nil
}
func (b acpTestBackend) Provider(string) llm.Provider { return b.provider }

func acpBackendTestFactory(t *testing.T, extra ...string) (*acpRootFactory, string) {
	t.Helper()
	launch, workspace := t.TempDir(), t.TempDir()
	t.Chdir(launch)
	path := filepath.Join(launch, "harness.json")
	if err := os.WriteFile(path, []byte(`{"no_env":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"acp", "serve", "--config", path, "--model", "fake:model"}, extra...)
	invocation, err := commandCatalog(environment{}).Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	factory := &acpRootFactory{flags: invocation.Flags, launchCWD: launch, logger: slog.New(slog.DiscardHandler), env: environment{getenv: func(key string) string {
		switch key {
		case "HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME":
			return launch
		}
		return ""
	}}}
	t.Cleanup(factory.Close)
	return factory, workspace
}

func TestACPSharedBackendSequentialAndConcurrentRoots(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "sequential", true: "concurrent"}[concurrent], func(t *testing.T) {
			factory, cwd := acpBackendTestFactory(t)
			var opened, closed atomic.Int32
			var lifetime context.Context
			provider := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn}, llmtest.Step{Stop: llm.StopEndTurn})
			factory.env.openModelBackend = func(ctx context.Context, _ config.Config, _ *slog.Logger, _ *tracing.Tracer) (*openedModelBackend, error) {
				opened.Add(1)
				lifetime = ctx
				return &openedModelBackend{Backend: acpTestBackend{provider}, Mode: "local", close: func() error { closed.Add(1); return nil }}, nil
			}
			roots := make([]acpagent.RootSession, 2)
			var wg sync.WaitGroup
			create := func(i int) {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var err error
				roots[i], err = factory.New(ctx, acpagent.SessionConfig{CWD: cwd})
				if err != nil {
					t.Errorf("New: %v", err)
				}
			}
			for i := range roots {
				wg.Add(1)
				if concurrent {
					go create(i)
				} else {
					create(i)
					if roots[i] != nil {
						if _, err := roots[i].Prompt(context.Background(), "hello", &recordACPUpdates{}); err != nil {
							t.Fatal(err)
						}
						if err := roots[i].Close(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			wg.Wait()
			if opened.Load() != 1 || closed.Load() != 0 {
				t.Fatalf("before factory close: opened=%d closed=%d", opened.Load(), closed.Load())
			}
			if err := lifetime.Err(); err != nil {
				t.Fatalf("construction context cancellation stopped shared runtime: %v", err)
			}
			if concurrent {
				for _, root := range roots {
					if root != nil {
						if _, err := root.Prompt(context.Background(), "hello", &recordACPUpdates{}); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			factory.Close()
			factory.Close()
			if closed.Load() != 1 || !errors.Is(lifetime.Err(), context.Canceled) {
				t.Fatalf("closed=%d lifetime error=%v", closed.Load(), lifetime.Err())
			}
		})
	}
}

func TestACPSharedBackendJoinsCleanupBeforeClose(t *testing.T) {
	root := newACPCleanupTestRoot(t)
	synctest.Test(t, func(t *testing.T) {
		started, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
		root.cleanups = []func(context.Context){func(context.Context) { close(started); <-release }}
		factory := &acpRootFactory{roots: []*acpTrackedRoot{trackACPRoot(root)}, backend: &openedModelBackend{close: func() error { close(closed); return nil }}}
		done := make(chan struct{})
		go func() { factory.Close(); close(done) }()
		<-started
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("backend closed before root-owned cleanup")
		default:
		}
		close(release)
		<-done
		<-closed
	})
}

func TestACPSharedBackendJoinsConstructionBeforeClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closed := make(chan struct{})
		factory := &acpRootFactory{backend: &openedModelBackend{close: func() error { close(closed); return nil }}}
		ctx, finish, err := factory.beginConstruction(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { factory.Close(); close(done) }()
		<-ctx.Done()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("backend closed before admitted construction cleanup")
		default:
		}
		finish()
		<-done
		<-closed
	})
}

func TestACPCustomBuilderDoesNotOpenBackend(t *testing.T) {
	factory, cwd := acpBackendTestFactory(t)
	factory.env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
		t.Fatal("custom builder must not open a model backend")
		return nil, nil
	}
	factory.build = func(context.Context, environment, acpagent.SessionConfig, *slog.Logger, config.Result) (acpagent.RootSession, error) {
		return stubACPRoot{}, nil
	}
	if _, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: cwd}); err != nil {
		t.Fatal(err)
	}
}

func TestACPBackendConfigurationCannotSwitch(t *testing.T) {
	factory := &acpRootFactory{}
	defer factory.Close()
	factory.env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
		return &openedModelBackend{Backend: acpTestBackend{}}, nil
	}
	cfg := config.Config{ModelBackend: "remote", ModelProxyURL: "http://localhost:9123", ModelProxyAPIKey: "secret"}
	if _, err := factory.backendFor(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []config.Config{
		{ModelBackend: "local"},
		{ModelBackend: "remote", ModelProxyURL: cfg.ModelProxyURL, ModelProxyAPIKey: "other-secret"},
		{ModelBackend: "remote", ModelProxyURL: "http://localhost:9124", ModelProxyAPIKey: cfg.ModelProxyAPIKey},
	} {
		if _, err := factory.backendFor(context.Background(), changed); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("configuration switch error=%v", err)
		}
	}
}

func TestACPLocalBackendUsesLaunchRelativeModelConfig(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("local provider authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"local-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"local answer\"}}]}\n\ndata: {\"id\":\"local-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	factory, cwd := acpBackendTestFactory(t, "--model-backend", "local", "--model-config", "models.json")
	for name, content := range map[string]string{
		"models.json":   `{"provider_configs":["provider.json"],"models_dev_cache_ttl":0,"provider_models_cache_ttl":0,"subscription_poll_interval":0}`,
		"provider.json": fmt.Sprintf(`{"name":"fake","api_type":"openai","base_url":%q,"api_key":"secret","models":[{"name":"model"}]}`, provider.URL),
	} {
		if err := os.WriteFile(filepath.Join(factory.launchCWD, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if factory.backend.Mode != "local" || factory.backend.Label() != "local" {
		t.Fatalf("backend mode=%q label=%q", factory.backend.Mode, factory.backend.Label())
	}
	updates := &recordACPUpdates{}
	if reason, err := root.Prompt(context.Background(), "hello", updates); err != nil || reason != acp.StopReasonEndTurn {
		t.Fatalf("local prompt reason=%s error=%v", reason, err)
	}
	if strings.Join(updates.text, "") != "local answer" || requests.Load() != 1 {
		t.Fatalf("local output=%v provider requests=%d", updates.text, requests.Load())
	}
	if err := root.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: cwd}); err != nil {
		t.Fatal(err)
	}
}

func TestACPModelConfigPathsStayBoundToTheirSource(t *testing.T) {
	for _, source := range []string{"flag", "environment", "file"} {
		t.Run(source, func(t *testing.T) {
			var extra []string
			if source == "flag" {
				extra = []string{"--model-config", "models.json"}
			}
			factory, cwd := acpBackendTestFactory(t, extra...)
			want := filepath.Join(factory.launchCWD, "models.json")
			if source == "environment" {
				previous := factory.env.getenv
				factory.env.getenv = func(key string) string {
					if key == "HARNESS_MODEL_CONFIG" {
						return "models.json"
					}
					return previous(key)
				}
			}
			if source == "file" {
				dir := filepath.Join(factory.launchCWD, "nested")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "harness.json")
				if err := os.WriteFile(path, []byte(`{"model_config":"models.json"}`), 0600); err != nil {
					t.Fatal(err)
				}
				invocation, err := commandCatalog(environment{}).Parse([]string{"acp", "serve", "--config", path})
				if err != nil {
					t.Fatal(err)
				}
				factory.flags = invocation.Flags
				want = filepath.Join(dir, "models.json")
			}
			factory.build = func(_ context.Context, _ environment, _ acpagent.SessionConfig, _ *slog.Logger, result config.Result) (acpagent.RootSession, error) {
				if result.Config.ModelConfig != want {
					t.Errorf("model config=%q want %q", result.Config.ModelConfig, want)
				}
				return stubACPRoot{}, nil
			}
			for range 2 {
				if _, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: cwd}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestACPSharedBackendWaitsForLateRootSettlement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closed := make(chan struct{})
		// Channel waits inside the bubble are durable. A blocked Mutex.Lock
		// would prevent virtual time reaching the factory settlement deadline.
		root := &lateACPRoot{closeStarted: make(chan struct{}), release: make(chan struct{}), finish: func() {}}
		factory := &acpRootFactory{roots: []*acpTrackedRoot{trackACPRoot(root)}, backend: &openedModelBackend{close: func() error { close(closed); return nil }}}
		done := make(chan struct{})
		go func() { factory.Close(); close(done) }()
		<-root.closeStarted
		<-done // The generic settlement deadline is still bounded.
		select {
		case <-closed:
			t.Error("backend closed while the root could still use it")
		default:
		}
		close(root.release)
		<-closed
	})
}

func TestACPSharedBackendWaitsForRealRootPrompt(t *testing.T) {
	root := newACPCleanupTestRoot(t)
	started, canceled, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	provider := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn, Block: func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release // Model work can outlive cancellation until it cooperates.
		select {
		case <-closed:
			t.Error("backend closed underneath active model work")
		default:
		}
	}})
	root.cfg = config.Config{Provider: "fake", Model: "fake:model"}
	root.registryModel = "fake:model"
	root.registry = llm.NewRegistry(map[string]llm.ModelInfo{"fake:model": {ContextWindow: 8192}})
	root.agent = agent.New(provider, tools.Default(), agent.Options{Model: root.registryModel, Registry: root.registry})
	tracked := trackACPRoot(root)
	factory := &acpRootFactory{roots: []*acpTrackedRoot{tracked}, backend: &openedModelBackend{close: func() error { close(closed); return nil }}}
	promptDone := make(chan acp.StopReason, 1)
	go func() {
		reason, err := tracked.Prompt(context.Background(), "hello", &recordACPUpdates{})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("prompt error=%v", err)
		}
		promptDone <- reason
	}()
	<-started
	// Deplete settlement explicitly: this real prompt holds its root mutex,
	// whose waiter is intentionally not a synctest durable block.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	factory.close(ctx)
	<-canceled
	select {
	case <-closed:
		t.Error("backend closed before the real root prompt settled")
	default:
	}
	close(release)
	if reason := <-promptDone; reason != acp.StopReasonCancelled {
		t.Errorf("prompt stop=%s, want cancellation", reason)
	}
	<-closed
	<-tracked.closeDone
}

func TestACPDirectRootOwnsInjectedBackend(t *testing.T) {
	factory, cwd := acpBackendTestFactory(t)
	var closed atomic.Int32
	factory.env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
		return &openedModelBackend{Backend: acpTestBackend{llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})}, close: func() error { closed.Add(1); return nil }}, nil
	}
	result, err := config.LoadParsed(harnessLoadOptions(factory.env, nil), factory.flags)
	if err != nil {
		t.Fatal(err)
	}
	root, err := newACPRootSession(context.Background(), factory.env, acpagent.SessionConfig{CWD: cwd}, factory.logger, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := root.Prompt(context.Background(), "hello", &recordACPUpdates{}); err != nil || reason != acp.StopReasonEndTurn {
		t.Fatalf("reason=%s error=%v", reason, err)
	}
	if err := root.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(context.Background()); err != nil || closed.Load() != 1 {
		t.Fatalf("closed=%d error=%v", closed.Load(), err)
	}
	result.Config.Model = "fake:missing"
	if failed, err := newACPRootSession(context.Background(), factory.env, acpagent.SessionConfig{CWD: cwd}, factory.logger, result, nil); err == nil || failed != nil {
		t.Fatalf("unknown-model root=%v error=%v", failed, err)
	}
	if closed.Load() != 2 {
		t.Fatalf("failed direct construction leaked its backend: closed=%d", closed.Load())
	}
}
