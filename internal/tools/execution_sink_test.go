package tools_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"harness/internal/buildinfo"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/otel"
	"harness/internal/tools"
)

type sinkBlockingTool struct {
	started chan struct{}
	release chan struct{}
}

func (*sinkBlockingTool) Name() string                  { return "read" }
func (*sinkBlockingTool) Description() string           { return "test worker" }
func (*sinkBlockingTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (*sinkBlockingTool) ReadOnly(json.RawMessage) bool { return true }
func (t *sinkBlockingTool) Run(context.Context, json.RawMessage) (string, error) {
	close(t.started)
	<-t.release
	return "late result", nil
}

type sinkMetricPayload struct {
	ResourceMetrics []struct {
		ScopeMetrics []struct {
			Metrics []struct {
				Name string
				Sum  struct{ DataPoints []sinkMetricPoint }
			}
		}
	}
}

type sinkMetricPoint struct {
	AsInt      string
	Attributes []struct {
		Key   string
		Value struct{ StringValue string }
	}
}

func TestRegistrySinkLogicalCancellationAndTimeoutLabels(t *testing.T) {
	for _, outcome := range []string{"cancelled", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			payloads := make(chan sinkMetricPayload, 16)
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload sinkMetricPayload
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode metrics: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				payloads <- payload
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer collector.Close()
			exporter, err := otel.NewExporter(otel.Config{Enabled: true, Endpoint: collector.URL}, buildinfo.Metadata{}, "", "provider", "model", "root", nil)
			if err != nil {
				t.Fatal(err)
			}
			registry := &tools.Registry{}
			worker := &sinkBlockingTool{started: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(worker.release) }) }
			defer release()
			registry.Register(worker)
			if outcome == "timeout" {
				registry.SetDispatchTimeout(20 * time.Millisecond)
			}
			sink := otel.NewSink(exporter, registry, "provider", "model", "root", false)
			ctx, cancel := context.WithCancel(execution.WithScope(context.Background(), sink.Scope()))
			defer cancel()
			var result llm.ToolResult
			var completed <-chan struct{}
			returned := make(chan struct{})
			go func() {
				result, completed = registry.DispatchWithCompletion(ctx, llm.ToolCall{Name: "read"})
				close(returned)
			}()
			<-worker.started
			if outcome == "cancelled" {
				cancel()
			}
			<-returned
			if !result.IsError || string(result.ErrorKind) != outcome {
				t.Fatalf("logical result: %+v", result)
			}
			select {
			case <-completed:
				t.Fatal("worker finished before release")
			default:
			}
			// Export while the actual worker remains alive: only logical-result
			// accounting can supply these outcome/error_kind labels.
			if err := exporter.Export(context.Background()); err != nil {
				t.Fatal(err)
			}
			close(payloads)
			counts := map[string]int64{}
			for payload := range payloads {
				for _, resource := range payload.ResourceMetrics {
					for _, scope := range resource.ScopeMetrics {
						for _, metric := range scope.Metrics {
							if metric.Name != "harness.tool.calls" && metric.Name != "harness.tool.errors" {
								continue
							}
							for _, point := range metric.Sum.DataPoints {
								labels := map[string]string{}
								for _, attr := range point.Attributes {
									labels[attr.Key] = attr.Value.StringValue
								}
								if labels["outcome"] != outcome || labels["tool"] != "read" {
									t.Fatalf("wrong logical labels: %v", labels)
								}
								if metric.Name == "harness.tool.errors" && labels["error_kind"] != outcome {
									t.Fatalf("wrong error kind: %v", labels)
								}
								value, err := strconv.ParseInt(point.AsInt, 10, 64)
								if err != nil {
									t.Fatal(err)
								}
								counts[metric.Name] += value
							}
						}
					}
				}
			}
			if counts["harness.tool.calls"] != 1 || counts["harness.tool.errors"] != 1 {
				t.Fatalf("logical counters: %v", counts)
			}
			release()
			<-completed
		})
	}
}
