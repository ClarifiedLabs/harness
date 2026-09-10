package ui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"harness/internal/agent"
	"harness/internal/llm"
)

func TestRendererRejectedBatchAfterStreamedCalls(t *testing.T) {
	for _, opts := range []RenderOptions{{Verbose: true}, {ToolStream: true}} {
		t.Run(fmt.Sprint(opts.Verbose, opts.ToolStream), func(t *testing.T) {
			var out, errw bytes.Buffer
			r := NewRenderer(&out, &errw, opts)
			call := llm.ToolCall{ID: "id\x1b]52;c;YQ==\x07", Name: "read\n\x1b[31m\u009b\u202e"}
			r.ToolUseStart(call)
			r.TurnAttemptComplete(agent.TurnAttemptUsage{Usage: llm.Usage{CostKnown: true}})
			const summary = "[tool batch rejected: 1 calls not executed; invalid tool stage plan: invalid stage]"
			r.Notice(summary)
			call.Stage = &llm.ToolStage{EmissionIndex: 1, BatchRejected: true}
			r.ToolStart(call)
			r.ToolResult(llm.ToolResult{ForID: call.ID, IsError: true, Text: "duplicate error"})
			got := errw.String()
			if strings.ContainsAny(got, "\x1b\x07\u009b\u202e") || strings.Count(got, "\n") != 2 || strings.Count(got, "[tool-call:") != 1 || strings.Count(got, summary) != 1 || strings.Contains(got, "duplicate error") {
				t.Fatalf("unsafe or repeated rejection output: %q", got)
			}
			if len(r.pending) != 0 || len(r.pendingToolUses) != 0 {
				t.Fatal("rejected batch left pending calls")
			}
		})
	}
}

func TestRendererStreamedToolIdentityEscapedInLiveStatus(t *testing.T) {
	var out, errw bytes.Buffer
	r := NewRenderer(&out, &errw, RenderOptions{LiveStatus: true})
	defer r.StopProgress()
	r.ToolUseStart(llm.ToolCall{ID: "call", Name: "read\x1b]52;c;YQ==\x07\n"})
	r.renderMu.Lock()
	label := r.statusLabel
	r.renderMu.Unlock()
	if strings.ContainsAny(label, "\x1b\x07\n") || !strings.Contains(label, `read\x1b`) {
		t.Fatalf("unsafe status label: %q", label)
	}
}

func TestRendererRejectedBatchShowsOnlySummary(t *testing.T) {
	for _, opts := range []RenderOptions{{}, {Verbose: true}, {ToolStream: true}, {ConciseReads: true, ConciseShell: true}} {
		t.Run(fmt.Sprint(opts.Verbose, opts.ToolStream, opts.ConciseReads), func(t *testing.T) {
			var out, errw bytes.Buffer
			r := NewRenderer(&out, &errw, opts)
			const summary = "[tool batch rejected: 3 calls not executed; invalid tool stage plan: invalid stage]"
			r.Notice(summary)
			for i, name := range []string{"read", "shell", "delegate"} {
				id := fmt.Sprint(i)
				r.ToolStart(llm.ToolCall{ID: id, Name: name, Stage: &llm.ToolStage{EmissionIndex: i + 1, BatchRejected: true}})
				r.ToolResult(llm.ToolResult{ForID: id, IsError: true, Text: "duplicate error"})
			}
			if got := strings.TrimSpace(errw.String()); got != summary || out.Len() != 0 {
				t.Fatalf("batch display = %q, stdout=%q", got, out.String())
			}
			if len(r.pending) != 0 || r.statusActive {
				t.Fatalf("rejected batch left pending/status: %+v / %v", r.pending, r.statusActive)
			}
			// A later ordinary failure remains visible.
			r.ToolStart(llm.ToolCall{ID: "next", Name: "read", Stage: &llm.ToolStage{EmissionIndex: 1, Resolved: 1}})
			r.ToolResult(llm.ToolResult{ForID: "next", IsError: true, Text: "ordinary failure"})
			if !strings.Contains(errw.String(), "ordinary failure") {
				t.Fatal("suppressed ordinary failure after batch rejection")
			}
		})
	}
}
