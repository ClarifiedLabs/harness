package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/config"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/otel"
	"harness/internal/tools"
	"harness/internal/ui"
)

type constructionTestRoot struct{ closed chan struct{} }

func (*constructionTestRoot) Prompt(context.Context, string, acpagent.UpdateSink) (acp.StopReason, error) {
	return acp.StopReasonEndTurn, nil
}
func (r *constructionTestRoot) Close(context.Context) error { close(r.closed); return nil }

func TestACPRootFactoryCloseDoesNotWaitOnConstructorMutex(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			workspace := t.TempDir()
			t.Chdir(workspace)
			collector, exports := collectRootTelemetry(t)
			cfgPath := filepath.Join(workspace, "config.json")
			data, _ := json.Marshal(map[string]any{"otel": map[string]any{"enabled": enabled, "endpoint": collector.URL}})
			if err := os.WriteFile(cfgPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			invocation, err := commandCatalog(environment{}).Parse([]string{"acp", "serve", "--config", cfgPath})
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			root := &constructionTestRoot{closed: make(chan struct{})}
			factory := &acpRootFactory{launchCWD: workspace, flags: invocation.Flags,
				build: func(ctx context.Context, _ environment, _ acpagent.SessionConfig, _ *slog.Logger, _ config.Result) (acpagent.RootSession, error) {
					close(entered)
					<-release // deliberately ignores cancellation during construction
					if !errors.Is(ctx.Err(), context.Canceled) {
						t.Error("constructor was not cancelled")
					}
					return root, nil
				},
			}
			constructed := make(chan error, 1)
			go func() {
				got, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: workspace})
				if got != nil {
					t.Error("factory admitted a root returned after shutdown")
				}
				constructed <- err
			}()
			<-entered
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			closed := make(chan struct{})
			go func() { factory.close(ctx); close(closed) }()
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				close(release)
				<-constructed
				<-closed
				t.Fatal("factory close blocked behind constructor despite an expired context")
			}
			close(release)
			if err := <-constructed; err == nil {
				t.Fatal("late construction unexpectedly succeeded")
			}
			<-root.closed
			factory.mu.Lock()
			pending := len(factory.pending)
			telemetry := factory.telemetry
			factory.mu.Unlock()
			if pending != 0 {
				t.Fatalf("completed construction still tracked: %d", pending)
			}
			if enabled {
				_ = nextRootExport(t, exports)
				if err := telemetry.exporter.Export(context.Background()); !errors.Is(err, otel.ErrExporterShutdown) {
					t.Fatalf("late exporter remained active: %v", err)
				}
			} else if telemetry != nil {
				t.Fatal("disabled telemetry allocated an exporter")
			}
			if _, err := factory.telemetryFor(config.Config{}); err == nil {
				t.Fatal("closed factory admitted telemetry initialization")
			}
			select {
			case <-exports:
				t.Fatal("late constructor caused another export")
			default:
			}
		})
	}
}

func TestACPRootFactoryConcurrentCloseRespectsCallerBudget(t *testing.T) {
	root := &lateACPRoot{closeStarted: make(chan struct{}), release: make(chan struct{}), finish: func() {}}
	tracked := trackACPRoot(root)
	factory := &acpRootFactory{roots: []*acpTrackedRoot{tracked}}
	waiting := &observedSettleContext{Context: context.Background(), waiting: make(chan struct{})}
	first := make(chan struct{})
	go func() { factory.close(waiting); close(first) }()
	<-waiting.waiting
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := make(chan struct{})
	go func() { factory.close(ctx); close(second) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		close(root.release)
		<-first
		<-second
		t.Fatal("concurrent close waited on the first owner's unbounded cleanup")
	}
	close(root.release)
	<-first
}

type lateTelemetryTool struct{ started, release chan struct{} }

func (*lateTelemetryTool) ReadOnly(json.RawMessage) bool { return true }
func (*lateTelemetryTool) Name() string                  { return "read" }
func (*lateTelemetryTool) Description() string           { return "late foreground worker" }
func (*lateTelemetryTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (t *lateTelemetryTool) Run(context.Context, json.RawMessage) (string, error) {
	close(t.started)
	<-t.release
	return "late result", nil
}

func TestRootTelemetryFinalizeJoinsActualForegroundWorker(t *testing.T) {
	for _, acpFactory := range []bool{false, true} {
		t.Run(fmt.Sprintf("acp=%v", acpFactory), func(t *testing.T) {
			testRootTelemetryForegroundWorker(t, acpFactory)
		})
	}
}

func testRootTelemetryForegroundWorker(t *testing.T, acpFactory bool) {
	collector, exports := collectRootTelemetry(t)
	cfg := config.Config{}
	cfg.OTel.Enabled, cfg.OTel.Endpoint = true, collector.URL
	telemetry, err := newRootTelemetry(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := &tools.Registry{}
	tool := &lateTelemetryTool{started: make(chan struct{}), release: make(chan struct{})}
	registry.Register(tool)
	sink := telemetry.NewSink(registry, "provider", "model", "auto")
	second := telemetry.NewSink(registry, "provider", "child-model", "worker")
	if sink.Scope().Group != &telemetry.group || second.Scope().Group != sink.Scope().Group {
		t.Fatal("root sinks did not share process work group")
	}
	ctx, cancel := context.WithCancel(execution.WithScope(context.Background(), sink.Scope()))
	defer cancel()
	logical := make(chan llm.ToolResult, 1)
	go func() { logical <- registry.Dispatch(ctx, llm.ToolCall{Name: "read", Input: json.RawMessage(`{}`)}) }()
	<-tool.started
	cancel()
	if result := <-logical; !result.IsError {
		t.Fatal("expected logical cancellation before actual worker completion")
	}
	waiting := &observedSettleContext{Context: context.Background(), waiting: make(chan struct{})}
	finalize := func(ctx context.Context) { telemetry.Finalize(ctx, func() { sink.RecordSession(0, 0) }) }
	if acpFactory {
		// The logical ACP root is already closed. Its escaped foreground worker
		// still belongs to the shared group and must precede process export.
		sink.RecordSession(0, 0)
		tracked := trackACPRoot(&constructionTestRoot{closed: make(chan struct{})})
		if err := tracked.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		factory := &acpRootFactory{telemetry: telemetry, roots: []*acpTrackedRoot{tracked}}
		finalize = factory.close
	}
	done := make(chan struct{})
	go func() { finalize(waiting); close(done) }()
	select {
	case <-waiting.waiting:
	case <-exports:
		t.Fatal("terminal export overtook foreground worker")
	}
	close(tool.release)
	<-done
	payload := nextRootExport(t, exports)
	if got := payload.sum("harness.work.finished", map[string]string{"kind": "tool"}); got != 1 {
		t.Fatalf("late physical completion=%v, want 1", got)
	}
	if got := payload.sum("harness.session.total", nil); got != 1 {
		t.Fatalf("settled session snapshot=%v, want 1", got)
	}
}

func TestRootTelemetryFinalizeTimeoutSkipsMutableAppSnapshot(t *testing.T) {
	collector, exports := collectRootTelemetry(t)
	var logs bytes.Buffer
	cfg := config.Config{}
	cfg.OTel.Enabled, cfg.OTel.Endpoint = true, collector.URL
	telemetry, err := newRootTelemetry(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	app := &ui.App{SessionPath: "initial"}
	app.SetOTel(telemetry.NewSink(&tools.Registry{}, "provider", "model", "auto"))
	finish := app.TrackOTel() // same registration used before the real UI owners
	started, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer finish()
		close(started)
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			app.SessionPath = strconv.Itoa(n) // unsafe for any concurrent aggregate read
		}
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.WaitOTel(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("active App owner not tracked: %v", err)
	}
	called := false
	telemetry.Finalize(ctx, func() { called = true; app.RecordOTelSession() })
	close(stop)
	<-done
	if called {
		t.Fatal("timeout path read mutable App session state")
	}
	if !strings.Contains(logs.String(), "skipping session snapshot") {
		t.Fatalf("missing nonfatal settle warning: %s", logs.String())
	}
	if got := nextRootExport(t, exports).sum("harness.session.total", nil); got != 0 {
		t.Fatalf("unsettled session snapshot exported: %v", got)
	}
}
