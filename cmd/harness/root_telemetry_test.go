package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"harness/internal/acp"
	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/mcp/jsonrpc"
	"harness/internal/otel"
)

// Decode actual OTLP wire data so the test covers configuration, both root
// lifecycles, exclusive core observations, and the synchronous terminal export.
type rootMetricPoint struct {
	Attributes []struct {
		Key   string `json:"key"`
		Value struct {
			String string `json:"stringValue"`
		} `json:"value"`
	} `json:"attributes"`
	Int    string  `json:"asInt"`
	Double float64 `json:"asDouble"`
	Count  string  `json:"count"`
}
type rootMetric struct {
	Name string `json:"name"`
	Sum  struct {
		Points []rootMetricPoint `json:"dataPoints"`
	} `json:"sum"`
	Histogram struct {
		Points []rootMetricPoint `json:"dataPoints"`
	} `json:"histogram"`
}
type rootPayload struct {
	Resources []struct {
		Resource struct {
			Attributes []struct {
				Key   string `json:"key"`
				Value struct {
					String string `json:"stringValue"`
				} `json:"value"`
			} `json:"attributes"`
		} `json:"resource"`
		Scopes []struct {
			Metrics []rootMetric `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

func TestACPOTelMultipleRootsShareProcessAndTerminalExport(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	if err := os.WriteFile(filepath.Join(workspace, "input.txt"), []byte("private tool contents"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "read_one", ToolName: "read", ToolInput: json.RawMessage(`{"path":"input.txt"}`)}}, Stop: llm.StopToolUse, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: .01, CostKnown: true}}
	answer := okStepWithUsage(7, 3)
	answer.Usage.CostUSD, answer.Usage.CostKnown = .02, true
	fp := llmtest.New("fake", tool, answer, tool, answer)
	var mu sync.Mutex
	var payloads []rootPayload
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" || r.Header.Get("X-Telemetry-Test") != "configured" {
			t.Errorf("collector request path/header not configured: %s", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		if strings.Contains(string(data), "private tool contents") || strings.Contains(string(data), "private prompt contents") {
			t.Error("content leaked to metrics")
		}
		var payload rootPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		payloads = append(payloads, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	cfgPath := filepath.Join(workspace, "config.json")
	data, _ := json.Marshal(map[string]any{"otel": map[string]any{"enabled": true, "endpoint": collector.URL, "hostname": "", "headers": map[string]string{"X-Telemetry-Test": "configured"}}, "mcp": map[string]any{"enable": false}, "lsp": map[string]any{"enable": false}})
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	env, _, stderr, _, _ := fakeProviderEnvWithProxy(t, []string{"acp", "serve", "--config", cfgPath, "--model", "claude-opus-4-8"}, fp, "")
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	env.stdin, env.stdout = serverConn, serverConn
	client := jsonrpc.NewPeer(clientConn, jsonrpc.PeerOptions{})
	defer client.Close()
	done := make(chan int, 1)
	go func() { done <- run(env) }()
	var initialized acp.InitializeResponse
	callACPCommand(t, client, acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1}, &initialized)
	for i := 0; i < 2; i++ {
		var created acp.NewSessionResponse
		callACPCommand(t, client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: workspace, MCPServers: []acp.MCPServer{}}, &created)
		var response acp.PromptResponse
		callACPCommand(t, client, acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "private prompt contents"}}}, &response)
		if response.StopReason != acp.StopReasonEndTurn {
			t.Fatalf("stop reason: %s", response.StopReason)
		}
		// Leave the last root open: process teardown must close it before export.
		if i == 0 {
			var closed acp.CloseSessionResponse
			callACPCommand(t, client, acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: created.SessionID}, &closed)
		}
	}
	_ = client.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 1 {
		t.Fatalf("exports=%d, want one terminal process snapshot", len(payloads))
	}
	values := map[string]float64{}
	counts := map[string]int{}
	instances := map[string]bool{}
	for _, resource := range payloads[0].Resources {
		for _, attr := range resource.Resource.Attributes {
			if attr.Key == "service.instance.id" {
				instances[attr.Value.String] = true
			}
			if attr.Key == "host.name" {
				t.Error("explicit empty hostname was ignored")
			}
		}
		for _, scope := range resource.Scopes {
			for _, metric := range scope.Metrics {
				for _, point := range metric.Sum.Points {
					n, _ := strconv.ParseFloat(point.Int, 64)
					values[metric.Name] += n + point.Double
				}
				for _, point := range metric.Histogram.Points {
					n, _ := strconv.Atoi(point.Count)
					counts[metric.Name] += n
					if strings.HasPrefix(metric.Name, "harness.session.") {
						for _, attr := range point.Attributes {
							if attr.Key == "model" || attr.Key == "provider" {
								t.Errorf("inclusive session attributed to last model: %s", metric.Name)
							}
						}
					}
				}
			}
		}
	}
	if len(instances) != 1 || instances[""] {
		t.Fatalf("process identities: %v", instances)
	}
	for name, want := range map[string]float64{"harness.tokens.total": 50, "harness.cost.usd": .06, "harness.model.requests": 4, "harness.prompt.total": 2, "harness.tool.calls": 2} {
		if math.Abs(values[name]-want) > 1e-9 {
			t.Errorf("%s=%v, want %v (values=%v)", name, values[name], want, values)
		}
	}
	if counts["harness.session.tokens"] != 2 {
		t.Errorf("session snapshots=%v, want two inclusive roots", counts)
	}
}

func TestRootTelemetryCloseWarnsAndJoins(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }))
	defer collector.Close()
	var logs bytes.Buffer
	cfg := config.Config{}
	cfg.OTel.Enabled, cfg.OTel.Endpoint = true, collector.URL
	telemetry, err := newRootTelemetry(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	telemetry.exporter.RecordSum(strings.Repeat("x", 257), "1", 1, nil)
	telemetry.Close()
	first := logs.String()
	if !strings.Contains(first, "final OTEL export failed") || !strings.Contains(first, "lost metric detail") {
		t.Fatalf("missing final diagnostics: %s", first)
	}
	if err := telemetry.exporter.Export(context.Background()); !errors.Is(err, otel.ErrExporterShutdown) {
		t.Fatalf("export after shutdown: %v", err)
	}
	telemetry.Close()
	if logs.String() != first {
		t.Fatal("duplicate close logged/exported twice")
	}
}
