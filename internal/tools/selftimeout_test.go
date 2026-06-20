package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"harness/internal/llm"
)

// selfTimeoutProbeTool reports the time remaining on its ctx deadline so a test
// can verify the dispatch ceiling was raised to the tool's own SelfTimeout
// rather than capped at the registry ceiling.
type selfTimeoutProbeTool struct{ self time.Duration }

func (selfTimeoutProbeTool) Name() string                  { return "self_timeout_probe_tool" }
func (selfTimeoutProbeTool) Description() string           { return "reports ctx deadline" }
func (selfTimeoutProbeTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (selfTimeoutProbeTool) ReadOnly(json.RawMessage) bool { return true }
func (s selfTimeoutProbeTool) SelfTimeout(json.RawMessage) (time.Duration, bool) {
	return s.self, true
}
func (selfTimeoutProbeTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		return "none", nil
	}
	return time.Until(dl).String(), nil
}

func dispatchRemaining(t *testing.T, r *Registry) (string, bool) {
	t.Helper()
	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "self_timeout_probe_tool", Input: json.RawMessage(`{}`)})
	if res.IsError {
		t.Fatalf("unexpected dispatch error: %+v", res)
	}
	if res.Text == "none" {
		return "", false
	}
	return res.Text, true
}

// A tool's own (longer) deadline must raise the ceiling so it is not cut early.
func TestDispatchCeilingRaisedBySelfTimeout(t *testing.T) {
	r := &Registry{}
	r.Register(selfTimeoutProbeTool{self: time.Hour})
	r.SetDispatchTimeout(20 * time.Millisecond)

	text, hasDeadline := dispatchRemaining(t, r)
	if !hasDeadline {
		t.Fatal("want a ceiling deadline raised to SelfTimeout, got none")
	}
	remaining, err := time.ParseDuration(text)
	if err != nil {
		t.Fatalf("parse remaining %q: %v", text, err)
	}
	if remaining < time.Minute {
		t.Errorf("ceiling not raised to SelfTimeout: remaining %s, want ~1h", remaining)
	}
}

// SelfTimeout only ever raises; a shorter self deadline must not lower the
// configured ceiling.
func TestDispatchSelfTimeoutNeverLowersCeiling(t *testing.T) {
	r := &Registry{}
	r.Register(selfTimeoutProbeTool{self: 5 * time.Millisecond})
	r.SetDispatchTimeout(time.Hour)

	text, hasDeadline := dispatchRemaining(t, r)
	if !hasDeadline {
		t.Fatal("want the configured 1h ceiling, got none")
	}
	remaining, err := time.ParseDuration(text)
	if err != nil {
		t.Fatalf("parse remaining %q: %v", text, err)
	}
	if remaining < time.Minute {
		t.Errorf("self timeout lowered the ceiling: remaining %s, want ~1h", remaining)
	}
}

// With the ceiling disabled (zero), SelfTimeout must not synthesize one.
func TestDispatchSelfTimeoutInertWhenCeilingDisabled(t *testing.T) {
	r := &Registry{}
	r.Register(selfTimeoutProbeTool{self: time.Hour})

	if _, hasDeadline := dispatchRemaining(t, r); hasDeadline {
		t.Error("SelfTimeout must not impose a ceiling when dispatch timeout is disabled")
	}
}

func TestDefaultWithOptionsWiresDispatchTimeout(t *testing.T) {
	r, _ := DefaultWithOptions(Options{DispatchTimeout: 42 * time.Second})
	if r.dispatchTimeout != 42*time.Second {
		t.Fatalf("DefaultWithOptions dispatchTimeout = %s, want 42s", r.dispatchTimeout)
	}
}

func TestSubsetCarriesDispatchTimeout(t *testing.T) {
	r := &Registry{}
	r.Register(deadlineProbeTool{})
	r.SetDispatchTimeout(30 * time.Second)

	sub, err := r.Subset([]string{"deadline_probe_tool"})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	if sub.dispatchTimeout != 30*time.Second {
		t.Fatalf("subset dispatchTimeout = %s, want 30s (Subset must carry the ceiling)", sub.dispatchTimeout)
	}
}

func TestRunCommandSelfTimeout(t *testing.T) {
	rc := runCommand{}
	want := time.Duration(runCommandDefaultTimeout) * time.Second
	if d, ok := rc.SelfTimeout(json.RawMessage(`{"command":"echo hi"}`)); !ok || d != want {
		t.Errorf("default SelfTimeout = (%s,%v), want (%s,true)", d, ok, want)
	}
	if d, ok := rc.SelfTimeout(json.RawMessage(`{"command":"sleep 1000","timeout_seconds":1800}`)); !ok || d != 1800*time.Second {
		t.Errorf("explicit SelfTimeout = (%s,%v), want (1800s,true)", d, ok)
	}
	if _, ok := rc.SelfTimeout(json.RawMessage(`{"command":"x","background":true}`)); ok {
		t.Error("background run_command should report no SelfTimeout")
	}
	if _, ok := rc.SelfTimeout(json.RawMessage(`{"command":"x","timeout_seconds":-1}`)); ok {
		t.Error("negative timeout_seconds should report no SelfTimeout")
	}
}
