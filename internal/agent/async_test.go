package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestAsyncReadStartsBeforeResponseEndsAndRunsOnce(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	read := &recordTool{name: "read", readOnly: true, run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		runs.Add(1)
		close(started)
		select {
		case <-release:
			return "evidence", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	reg := tools.Default()
	reg.Register(read)
	input := json.RawMessage(`{"path":"file","_stage":1}`)
	fp := llmtest.New("astra", llmtest.Step{Events: []llm.StreamEvent{
		{Kind: llm.EventToolCallStart, ToolID: "r", ToolName: "read"},
		{Kind: llm.EventToolCallReady, ToolID: "r", ToolName: "read", ToolInput: input, ToolAsync: true},
		{Kind: llm.EventToolCallDone, ToolID: "r", ToolName: "read", ToolInput: input, ToolAsync: true},
	}, Stop: llm.StopToolUse, Block: func(ctx context.Context) {
		select {
		case <-started:
			close(release)
		case <-ctx.Done():
			t.Error("read did not start during generation")
		}
	}}, summaryStep("done", 10, 1))
	a := newAgent(fp, reg, Options{Model: "astra", Registry: llm.NewRegistry(map[string]llm.ModelInfo{"astra": {AsyncTools: true, ContextWindow: 100000}}), ExperimentalAsyncTools: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.RunPrompt(ctx, "read it", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 1 {
		t.Fatalf("read executed %d times", runs.Load())
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(fp.Requests))
	}
	results := 0
	for _, message := range fp.Requests[1].Messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockToolResult && block.ResultForID == "r" {
				results++
				if block.ResultText != "evidence" || block.ResultError {
					t.Fatalf("next request did not contain the completed read: %+v", block)
				}
			}
		}
	}
	if results != 1 {
		t.Fatalf("read results in next request = %d, want 1", results)
	}
	if err := llm.ValidateTranscript(a.Transcript()); err != nil {
		t.Fatal(err)
	}
	if !a.Transcript()[1].Content[0].ToolAsync {
		t.Fatal("async call lost replay metadata")
	}
}

func TestAsyncReadKeepsBudgetedContinuationAndArchive(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(fmt.Sprintf("archive=%t", archive), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "large.txt")
			if err := os.WriteFile(path, []byte(strings.Repeat(strings.Repeat("x", 80)+"\n", 1500)), 0600); err != nil {
				t.Fatal(err)
			}
			input, err := json.Marshal(map[string]any{"path": path, "limit": 1500})
			if err != nil {
				t.Fatal(err)
			}
			fp := llmtest.New("astra", llmtest.Step{Events: []llm.StreamEvent{
				{Kind: llm.EventToolCallReady, ToolID: "r", ToolName: "read", ToolInput: input, ToolAsync: true},
				{Kind: llm.EventToolCallDone, ToolID: "r", ToolName: "read", ToolInput: input, ToolAsync: true},
			}, Stop: llm.StopToolUse, Usage: llm.Usage{InputTokens: 23000, OutputTokens: 100}}, summaryStep("done", 10, 1))
			a := newAgent(fp, tools.Default(), Options{Model: "astra", Registry: llm.NewRegistry(map[string]llm.ModelInfo{"astra": {AsyncTools: true, ContextWindow: 50000}}), ExperimentalAsyncTools: true})
			recorded := &recordSink{}
			var sink EventSink = recorded
			archived := &archiveSink{archive: ToolResultArchive{DisplayPath: "artifact", ModelPath: "/tmp/read-artifact.txt"}}
			if archive {
				sink, recorded = archived, &archived.recordSink
			}
			if err := a.RunPrompt(context.Background(), "inspect", sink); err != nil {
				t.Fatal(err)
			}
			if len(recorded.results) != 1 {
				t.Fatalf("results = %d, want 1", len(recorded.results))
			}
			result := recorded.results[0]
			if result.IsError || !result.Truncated || result.OriginalBytes <= len(result.Text) || !strings.Contains(result.Text, "continue with offset=") {
				t.Fatalf("lost truncation/recovery: error=%v truncated=%v original=%d shown=%d tail=%q", result.IsError, result.Truncated, result.OriginalBytes, len(result.Text), result.Text[max(0, len(result.Text)-200):])
			}
			if len(result.Text) > a.readResultBatchByteBudget(23000) {
				t.Fatal("async result exceeded the shared context budget")
			}
			if archive && (len(archived.archived) != 1 || !strings.Contains(archived.archived[0].OriginalText, "1500\t") || !strings.Contains(result.Text, "full output archived at")) {
				t.Fatal("async result lost original output or archive hint")
			}
			if err := llm.ValidateTranscript(a.Transcript()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAsyncReadSharesFinalBudgetWithoutRereading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changing.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("original evidence\n", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"path": path, "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	laterInput, err := json.Marshal(map[string]any{"path": path, "offset": 2, "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	a := newAgent(llmtest.New("astra"), tools.Default(), Options{ExperimentalAsyncTools: true})
	batch := a.newAsyncReads(context.Background(), llm.Request{Tools: []llm.ToolSchema{{Name: "read", Async: true}}})
	batch.start(llm.StreamEvent{ToolName: "read", ToolID: "early", ToolAsync: true, ToolInput: input})
	cached := batch.finish(false)
	if len(cached) != 1 {
		t.Fatal("read was not prefetched")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("updated evidence\n", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := withReadResultBatchBudget(context.Background(), 1024, false, false)
	ctx = context.WithValue(ctx, asyncResultsKey{}, cached)
	results, _, _ := a.dispatchCalls(ctx, []llm.ToolCall{
		{ID: "early", Name: "read", Input: input, Async: true},
		{ID: "later", Name: "read", Input: laterInput},
	}, 1, 1, &recordSink{})
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for i, result := range results {
		want := "original evidence"
		if i == 1 {
			want = "updated evidence"
		}
		if result.ResultError || len(result.ResultText) > 512 || !strings.Contains(result.ResultText, want) || !strings.Contains(result.ResultText, "continue with offset=") {
			t.Fatalf("read %d lost its snapshot or shared allowance: %+v", i, result)
		}
	}
}

type asyncBackgroundStarter struct{ starts atomic.Int32 }

func (s *asyncBackgroundStarter) StartBackgroundJob(tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	return tools.BackgroundJobInfo{ID: fmt.Sprintf("bg_%d", s.starts.Add(1))}, nil
}

func TestAsyncBackgroundFetchUsesOrdinaryDispatch(t *testing.T) {
	starter := &asyncBackgroundStarter{}
	a := newAgent(llmtest.New("astra"), tools.DefaultWithOptions(tools.Options{Background: starter}), Options{ExperimentalAsyncTools: true})
	input := json.RawMessage(`{"url":"https://example.test/","background":true}`)
	req := llm.Request{Tools: []llm.ToolSchema{{Name: "web_fetch", Async: true}, {Name: "read", Async: true}}}
	for _, failed := range []bool{true, false} {
		batch := a.newAsyncReads(context.Background(), req)
		batch.start(llm.StreamEvent{ToolName: "web_fetch", ToolID: "fetch", ToolAsync: true, ToolInput: input})
		batch.start(llm.StreamEvent{ToolName: "read", ToolID: "later", ToolAsync: true, ToolInput: json.RawMessage(`{"path":"unused"}`)})
		started := len(batch.jobs)
		batch.finish(failed)
		if started != 0 || starter.starts.Load() != 0 {
			t.Fatalf("speculated across a background launch: jobs=%d launches=%d", started, starter.starts.Load())
		}
	}
	results, _, _ := a.dispatchCalls(context.Background(), []llm.ToolCall{{ID: "fetch", Name: "web_fetch", Input: input, Async: true}}, 1, 1, &recordSink{})
	if starter.starts.Load() != 1 || len(results) != 1 || results[0].ResultError || results[0].ResultText != "background job bg_1 started" {
		t.Fatalf("ordinary dispatch lost job receipt: launches=%d results=%+v", starter.starts.Load(), results)
	}
}

func TestAsyncReadsRespectBarriersAndDeduplicate(t *testing.T) {
	var runs atomic.Int32
	reg := tools.Default()
	reg.Register(&recordTool{name: "read", readOnly: true, run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }})
	a := newAgent(llmtest.New("astra"), reg, Options{ExperimentalAsyncTools: true})
	req := llm.Request{Tools: []llm.ToolSchema{{Name: "read", Async: true}}}
	for _, barrier := range []bool{false, true} {
		batch := a.newAsyncReads(context.Background(), req)
		if barrier {
			batch.observeStart(llm.StreamEvent{ToolName: "write"})
		}
		batch.start(llm.StreamEvent{ToolName: "read", ToolID: "a", ToolAsync: true, ToolInput: json.RawMessage(`{"path":"same","_stage":1}`)})
		batch.start(llm.StreamEvent{ToolName: "read", ToolID: "b", ToolAsync: true, ToolInput: json.RawMessage(`{"path":"same","_stage":1}`)})
		batch.start(llm.StreamEvent{ToolName: "read", ToolID: "c", ToolAsync: true, ToolInput: json.RawMessage(`{"path":"later","_stage":2}`)})
		results := batch.finish(false)
		want := 1
		if barrier {
			want = 0
		}
		if len(results) != want {
			t.Fatalf("prefetched %d, want %d", len(results), want)
		}
	}
	if runs.Load() != 1 {
		t.Fatalf("runs=%d", runs.Load())
	}
}

func TestFailedAsyncAttemptCancelsReadsWaitingForAnExecutionSlot(t *testing.T) {
	a := newAgent(llmtest.New("astra"), tools.Default(), Options{ExperimentalAsyncTools: true})
	// Occupy the execution slot so a failed stream must cancel its queued read
	// without waiting for unrelated work to release the slot.
	a.toolRunSem = make(chan struct{}, 1)
	a.toolRunSem <- struct{}{}
	batch := a.newAsyncReads(context.Background(), llm.Request{Tools: []llm.ToolSchema{{Name: "read", Async: true}}})
	batch.start(llm.StreamEvent{ToolName: "read", ToolID: "queued", ToolAsync: true, ToolInput: json.RawMessage(`{"path":"unused"}`)})
	finished := make(chan struct{})
	go func() {
		batch.finish(true)
		close(finished)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-finished:
		<-a.toolRunSem
	case <-ctx.Done():
		<-a.toolRunSem
		<-finished
		t.Fatal("failed attempt waited for an unrelated task's execution slot")
	}
}
