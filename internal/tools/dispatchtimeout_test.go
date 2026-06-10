package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
)

// ctxTool honors ctx like a well-behaved tool: it blocks until cancelled.
type ctxTool struct{}

func (ctxTool) Name() string                  { return "ctx_tool" }
func (ctxTool) Description() string           { return "blocks until ctx is done" }
func (ctxTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (ctxTool) ReadOnly(json.RawMessage) bool { return false }
func (ctxTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// stuckTool ignores ctx entirely — the case the ceiling exists for.
type stuckTool struct{ release chan struct{} }

func (s *stuckTool) Name() string                  { return "stuck_tool" }
func (s *stuckTool) Description() string           { return "ignores ctx" }
func (s *stuckTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (s *stuckTool) ReadOnly(json.RawMessage) bool { return false }
func (s *stuckTool) Run(_ context.Context, _ json.RawMessage) (string, error) {
	<-s.release
	return "released", nil
}

// internalDeadlineTool returns context.DeadlineExceeded from its own internal
// limit (e.g. http.Client.Timeout) without the dispatch ceiling having fired.
type internalDeadlineTool struct{}

func (internalDeadlineTool) Name() string                  { return "internal_deadline_tool" }
func (internalDeadlineTool) Description() string           { return "hits its own deadline" }
func (internalDeadlineTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (internalDeadlineTool) ReadOnly(json.RawMessage) bool { return false }
func (internalDeadlineTool) Run(_ context.Context, _ json.RawMessage) (string, error) {
	return "", context.DeadlineExceeded
}

type deadlineProbeTool struct{}

func (deadlineProbeTool) Name() string                  { return "deadline_probe_tool" }
func (deadlineProbeTool) Description() string           { return "reports whether ctx has a deadline" }
func (deadlineProbeTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (deadlineProbeTool) ReadOnly(json.RawMessage) bool { return false }
func (deadlineProbeTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	if _, ok := ctx.Deadline(); ok {
		return "deadline", nil
	}
	return "no deadline", nil
}

func TestDispatchHasNoDefaultTimeoutCeiling(t *testing.T) {
	r := &Registry{}
	r.Register(deadlineProbeTool{})

	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "deadline_probe_tool", Input: json.RawMessage(`{}`)})
	if res.IsError || res.Text != "no deadline" {
		t.Fatalf("dispatch result = %+v, want no deadline", res)
	}
}

func TestDispatchTimeoutCeiling(t *testing.T) {
	r := &Registry{}
	r.Register(ctxTool{})
	r.SetDispatchTimeout(20 * time.Millisecond)

	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "ctx_tool", Input: json.RawMessage(`{}`)})
	if !res.IsError || !strings.Contains(res.Text, "timed out after 20ms") {
		t.Errorf("want timeout is_error result, got %+v", res)
	}
}

func TestDispatchTimeoutUnblocksCtxIgnoringTool(t *testing.T) {
	stuck := &stuckTool{release: make(chan struct{})}
	defer close(stuck.release) // let the abandoned goroutine finish
	r := &Registry{}
	r.Register(stuck)
	r.SetDispatchTimeout(20 * time.Millisecond)

	done := make(chan llm.ToolResult, 1)
	go func() {
		done <- r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "stuck_tool", Input: json.RawMessage(`{}`)})
	}()
	select {
	case res := <-done:
		if !res.IsError || !strings.Contains(res.Text, "timed out") {
			t.Errorf("want timeout is_error result, got %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch did not return: ceiling failed to unblock a ctx-ignoring tool")
	}
}

func TestDispatchOuterCancellationIsNotATimeout(t *testing.T) {
	r := &Registry{}
	r.Register(ctxTool{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	res := r.Dispatch(ctx, llm.ToolCall{ID: "1", Name: "ctx_tool", Input: json.RawMessage(`{}`)})
	if !res.IsError {
		t.Fatalf("want is_error result, got %+v", res)
	}
	if strings.Contains(res.Text, "timed out") {
		t.Errorf("outer cancellation must not be reported as a timeout: %q", res.Text)
	}
	if !strings.Contains(res.Text, context.Canceled.Error()) {
		t.Errorf("want cancellation error in result, got %q", res.Text)
	}
}

// A tool's own internal deadline (e.g. http.Client.Timeout, which yields an
// error satisfying errors.Is(err, context.DeadlineExceeded)) must surface as a
// plain tool error, not as a dispatch-ceiling timeout — the ceiling never
// fired, so reporting "timed out after 1m0s" would be wrong semantics and the
// wrong duration (spec §6: only the ceiling's own expiry becomes a timeout).
func TestDispatchInternalDeadlineIsNotDispatchTimeout(t *testing.T) {
	r := &Registry{}
	r.Register(internalDeadlineTool{})
	r.SetDispatchTimeout(time.Minute) // generous; the ceiling must not fire

	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "internal_deadline_tool", Input: json.RawMessage(`{}`)})
	if !res.IsError {
		t.Fatalf("want is_error result, got %+v", res)
	}
	if strings.Contains(res.Text, "timed out after") {
		t.Errorf("tool-internal deadline must not be reported as a dispatch timeout: %q", res.Text)
	}
	if !strings.Contains(res.Text, context.DeadlineExceeded.Error()) {
		t.Errorf("want the tool's deadline error in result, got %q", res.Text)
	}
}
