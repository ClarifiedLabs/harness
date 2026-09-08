package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/buildinfo"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/otel"
	"harness/internal/session"
	"harness/internal/tools"
)

func testUIExporter(t *testing.T, app *App) *otel.Exporter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(server.Close)
	exp, err := otel.NewExporter(otel.Config{Enabled: true, Endpoint: server.URL, Timeout: time.Second}, buildinfo.Metadata{Version: "test"}, "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := otel.NewSink(exp, nil, app.Provider, app.Model, app.AgentName, false)
	sink.SetWorkGroup(&execution.Group{})
	app.SetOTel(sink)
	return exp
}

type uiMetricPoint struct {
	Attributes []struct {
		Key   string
		Value struct{ StringValue string }
	}
	AsInt    string
	AsDouble float64
	Sum      float64
	Count    string
}

func (p uiMetricPoint) attr(key string) string {
	for _, a := range p.Attributes {
		if a.Key == key {
			return a.Value.StringValue
		}
	}
	return ""
}
func uiMetricPoints(t *testing.T, exp *otel.Exporter, name string) []uiMetricPoint {
	t.Helper()
	raw, err := exp.BuildPayloadForTest()
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []struct {
					Name      string
					Sum       struct{ DataPoints []uiMetricPoint }
					Gauge     struct{ DataPoints []uiMetricPoint }
					Histogram struct{ DataPoints []uiMetricPoint }
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	var points []uiMetricPoint
	for _, r := range payload.ResourceMetrics {
		for _, s := range r.ScopeMetrics {
			for _, m := range s.Metrics {
				if m.Name == name {
					points = append(points, m.Sum.DataPoints...)
					points = append(points, m.Gauge.DataPoints...)
					points = append(points, m.Histogram.DataPoints...)
				}
			}
		}
	}
	return points
}
func uiMetricTotal(t *testing.T, exp *otel.Exporter, name string) float64 {
	t.Helper()
	var total float64
	for _, p := range uiMetricPoints(t, exp, name) {
		if p.AsInt != "" {
			n, err := strconv.ParseFloat(p.AsInt, 64)
			if err != nil {
				t.Fatal(err)
			}
			total += n
		} else {
			total += p.AsDouble + p.Sum
		}
	}
	return total
}

func sourceUsage(u llm.Usage) func(context.Context) {
	return func(ctx context.Context) {
		source := llm.StartAttempt(ctx)
		source.Usage(u)
		source.Finish(llm.AttemptSucceeded, nil)
	}
}

func TestOTelPromptSourceAndToolsCountOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reuse     bool
		toolCalls float64
	}{
		{name: "distinct_reads", toolCalls: 2},
		{name: "reused_read", reuse: true, toolCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			dir := t.TempDir()
			input, err := json.Marshal(map[string]string{"path": filepath.Join(dir, "missing-1")})
			if err != nil {
				t.Fatal(err)
			}
			other, err := json.Marshal(map[string]string{"path": filepath.Join(dir, "missing-2")})
			if err != nil {
				t.Fatal(err)
			}
			if tc.reuse {
				other = input
			}
			first := llm.Usage{InputTokens: 10, OutputTokens: 3, CostKnown: true, CostUSD: .1}
			second := llm.Usage{InputTokens: 7, OutputTokens: 2, CostKnown: true, CostUSD: .2}
			fp := llmtest.New("anthropic", llmtest.Step{Events: []llm.StreamEvent{
				{Kind: llm.EventToolCallDone, ToolID: "read-1", ToolName: "read", ToolInput: input},
				{Kind: llm.EventToolCallDone, ToolID: "read-2", ToolName: "read", ToolInput: other},
			}, Stop: llm.StopToolUse, Usage: first, Block: sourceUsage(first)},
				llmtest.Step{Events: []llm.StreamEvent{textDelta("answer")}, Stop: llm.StopEndTurn, Usage: second, Block: sourceUsage(second)})
			app := newTestApp(t, &out, &errw, fp)
			exp := testUIExporter(t, app)
			if code := OneShot(app, "inspect both"); code != ExitOK {
				t.Fatalf("exit %d: %s", code, errw.String())
			}
			for name, want := range map[string]float64{"harness.prompt.total": 1, "harness.model.requests": 2, "harness.tokens.input": 17, "harness.tokens.output": 5, "harness.tool.calls": tc.toolCalls} {
				if got := uiMetricTotal(t, exp, name); got != want {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
			}
			if got := uiMetricTotal(t, exp, "harness.cost.usd"); got < .2999 || got > .3001 {
				t.Errorf("cost = %v", got)
			}
			if app.usage.InputTokens != 17 || app.usageByModel[app.usageKey()].OutputTokens != 5 {
				t.Fatalf("UI usage changed: %+v / %+v", app.usage, app.usageByModel)
			}
			if err := llm.ValidateTranscript(app.Agent.Transcript()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOTelTurnProgressAndGuardCountOnce(t *testing.T) {
	var out, errw bytes.Buffer
	dir := t.TempDir()
	var steps []llmtest.Step
	for i := range 3 {
		path := filepath.Join(dir, "lookup-"+strconv.Itoa(i))
		if err := os.WriteFile(path, []byte("lookup result\n"), 0600); err != nil {
			t.Fatal(err)
		}
		input, err := json.Marshal(map[string]string{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "read-" + strconv.Itoa(i), ToolName: "read", ToolInput: input}},
			Stop:   llm.StopToolUse,
		})
	}
	steps = append(steps, llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic", steps...))
	exp := testUIExporter(t, app)
	if code := OneShot(app, "inspect the three files"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	for name, want := range map[string]float64{
		"harness.tools_per_turn":       3,
		"harness.operations_per_turn":  3,
		"harness.single_lookup_turns":  3,
		"harness.single_inspect_turns": 3,
		"harness.guard.steers":         1,
	} {
		if got := uiMetricTotal(t, exp, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	points := uiMetricPoints(t, exp, "harness.guard.steers")
	if len(points) != 1 || points[0].attr("reason") != "batching" {
		t.Errorf("guard points = %+v, want one batching steer", points)
	}
	// Shared telemetry must not replace or duplicate the canonical recorder.
	raw, err := os.ReadFile(filepath.Join(app.SessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(raw, []byte(`"type":"turn_progress"`)); got != 3 {
		t.Errorf("recorded turn progress = %d, want 3", got)
	}
	if err := llm.ValidateTranscript(app.Agent.Transcript()); err != nil {
		t.Fatal(err)
	}
}

func TestOTelMaintenanceKeepsCapturedScopeAcrossModelSwitch(t *testing.T) {
	var out, errw bytes.Buffer
	oldUsage := llm.Usage{InputTokens: 11, OutputTokens: 1}
	old := llmtest.New("anthropic", llmtest.Step{Stop: llm.StopEndTurn, Usage: oldUsage, Block: sourceUsage(oldUsage)})
	app := newTestApp(t, &out, &errw, old)
	exp := testUIExporter(t, app)
	oldKey, oldModel := app.usageKey(), app.Model
	prewarm, ok := app.Agent.PrewarmFunc()
	if !ok {
		t.Fatal("no prewarm")
	}
	next := llmtest.New("next-provider", llmtest.Step{Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 23}})
	app.SwitchModel = func(string, llm.ReasoningConfig) (ModelSelection, error) {
		return ModelSelection{Provider: "next-provider", Model: "next-model", RegistryModel: "next-provider:next-model", Runtime: next}, nil
	}
	if !app.switchModel("next-model", llm.ReasoningConfig{}) {
		t.Fatal(errw.String())
	}
	result := prewarm(context.Background())
	app.QueuePrewarmResultForModel(oldKey, result)
	app.drainMaintenanceUsage()
	if code := OneShot(app, "next prompt"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	totals := map[string]float64{}
	for _, p := range uiMetricPoints(t, exp, "harness.tokens.input") {
		n, _ := strconv.ParseFloat(p.AsInt, 64)
		totals[p.attr("model")] += n
	}
	if totals[oldModel] != 11 || totals["next-model"] != 23 {
		t.Fatalf("captured model totals = %v", totals)
	}
	if app.usageByModel[oldKey].InputTokens != 11 || app.usageByModel[app.usageKey()].InputTokens != 23 {
		t.Fatalf("UI per-model totals = %+v", app.usageByModel)
	}
}

func TestOTelClearSettlesBeforeRootAggregateAndRotates(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic"))
	exp := testUIExporter(t, app)
	original := app.SessionPath
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 10}})
	app.QueueMaintenanceUsageForModel(app.usageKey(), agent.MaintenanceUsage{Purpose: "prewarm", Usage: llm.Usage{InputTokens: 20}})
	app.settleIdleCompaction = func() {
		app.QueueMaintenanceUsageForModel(app.usageKey(), agent.MaintenanceUsage{Purpose: "idle_compaction", Usage: llm.Usage{InputTokens: 30}})
	}
	app.AgentSessions = &testAgentSessionLifecycle{reset: func(context.Context) error {
		if got := uiMetricTotal(t, exp, "harness.session.total"); got != 0 {
			t.Fatalf("session emitted before shutdown: %v", got)
		}
		app.QueueMaintenanceUsageForModel(app.usageKey(), agent.MaintenanceUsage{Purpose: "prewarm", Usage: llm.Usage{InputTokens: 40}})
		return nil
	}}
	app.clear()
	if app.SessionPath == original || app.usage.InputTokens != 0 {
		t.Fatalf("clear did not rotate/reset: %s %+v", app.SessionPath, app.usage)
	}
	saved, err := session.Load(original)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Usage.InputTokens != 100 {
		t.Fatalf("saved old usage = %+v", saved.Usage)
	}
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 7}})
	app.RecordOTelSession()
	app.RecordOTelSession()
	if got := uiMetricTotal(t, exp, "harness.session.total"); got != 2 {
		t.Fatalf("sessions = %v, want 2", got)
	}
	if got := uiMetricTotal(t, exp, "harness.session.tokens"); got != 107 {
		t.Fatalf("session tokens = %v", got)
	}
	for _, p := range uiMetricPoints(t, exp, "harness.session.tokens") {
		if p.attr("scope") != "root_session_inclusive" || p.attr("model") != "" || p.attr("provider") != "" || p.attr("agent") != "" {
			t.Fatalf("inclusive aggregate mislabeled: %+v", p)
		}
	}
	payload, err := exp.BuildPayloadForTest()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), `"session_id"`) || strings.Contains(string(payload), filepath.Base(app.SessionPath)) {
		t.Fatalf("session identity leaked: %s", payload)
	}
}

func TestOTelCanBeDisabledBeforeNextPrompt(t *testing.T) {
	var out, errw bytes.Buffer
	usage := llm.Usage{InputTokens: 5}
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic", llmtest.Step{Stop: llm.StopEndTurn, Usage: usage, Block: sourceUsage(usage)}))
	exp := testUIExporter(t, app)
	app.SetOTel(nil)
	if code := OneShot(app, "unobserved prompt"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if got := uiMetricTotal(t, exp, "harness.prompt.total"); got != 0 {
		t.Fatalf("disabled telemetry emitted %v prompts", got)
	}
	if got := uiMetricTotal(t, exp, "harness.tokens.input"); got != 0 {
		t.Fatalf("disabled telemetry emitted %v tokens", got)
	}
	if app.usage.InputTokens != 5 {
		t.Fatalf("disabling telemetry changed usage: %+v", app.usage)
	}
}

func TestOTelSessionDoesNotRebuildChildMetadata(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("anthropic"))
	exp := testUIExporter(t, app)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"agent":"private-child","status":"completed","usage":{"input_tokens":999}}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager := background.NewManager(background.Options{})
	app.Background = manager
	_, err := manager.StartBackgroundJob(tools.BackgroundJobRequest{Kind: "delegate", Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
		return tools.BackgroundJobResult{TranscriptPath: dir, Usage: llm.Usage{InputTokens: 999}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	manager.ShutdownAndWait(time.Second)
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 10}})
	app.RecordOTelSession()
	for _, name := range []string{"harness.delegate.sessions", "harness.delegate.tokens", "harness.tokens.input", "harness.context.messages"} {
		if len(uiMetricPoints(t, exp, name)) != 0 {
			t.Errorf("shutdown rebuilt %s", name)
		}
	}
	if got := uiMetricTotal(t, exp, "harness.session.tokens"); got != 10 {
		t.Fatalf("root tokens = %v", got)
	}
}
