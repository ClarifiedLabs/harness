package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/taskcontext"
)

func TestChildContextToolsUseChildPolicyAndSession(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "other_provider", true: "codex"}[enabled], func(t *testing.T) {
			fixture := newContinuationFixture(t, 100_000, false, llmtest.Step{Stop: llm.StopEndTurn})
			original := fixture.runner.resolve
			parentDir := t.TempDir()
			fixture.runner.resolve = func(runtime Runtime, name string) (Launch, error) {
				launch, err := original(runtime, name)
				if err != nil {
					return launch, err
				}
				taskcontext.New(func() string { return parentDir }).Register(launch.Tools)
				launch.ContextManagement = enabled
				return launch, nil
			}
			if _, err := fixture.runner.Run(context.Background(), RunRequest{Task: "inspect"}, nil); err != nil {
				t.Fatal(err)
			}
			if len(fixture.provider.Requests) == 0 {
				t.Fatal("child made no request")
			}
			req := fixture.provider.Requests[0]
			for _, name := range taskcontext.Names {
				found := false
				for _, spec := range req.Tools {
					if spec.Name == name {
						found = true
					}
				}
				if found != enabled {
					t.Fatalf("%s exposed=%v expected=%v", name, found, enabled)
				}
			}
			if strings.Contains(strings.Join(req.RequestContext, "\n"), "Experimental context management") != enabled {
				t.Fatal("child guidance used parent policy")
			}
		})
	}
}

func TestContextContinuationPreservesNotesHistoryAndChildPolicy(t *testing.T) {
	call := func(id, name, input string) llmtest.Step {
		return llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: id, ToolName: name, ToolInput: json.RawMessage(input)}}, Stop: llm.StopToolUse}
	}
	first := call("save", "task_notes", `{"action":"write","path":"details/tests.md","text":"First fix failed; inspect the original failure."}`)
	first.Events = append([]llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "Original failure: stale cache. " + strings.Repeat("diagnostic detail ", 2000)}}, first.Events...)
	fixture := newContinuationFixture(t, 100_000, false, first,
		call("reset", "new_context", `{}`), llmtest.Step{Stop: llm.StopEndTurn},
		call("recover", "task_notes", `{"path":"details/tests.md"}`),
		call("evidence", "history_search", `{"query":"Original failure: stale cache"}`), llmtest.Step{Stop: llm.StopEndTurn})
	resolve := fixture.runner.resolve
	parentEnabled := false
	parentDir := t.TempDir()
	fixture.runner.resolve = func(runtime Runtime, name string) (Launch, error) {
		launch, err := resolve(runtime, name)
		if err == nil {
			manager := taskcontext.New(func() string { return parentDir })
			manager.SetEnabled(func() bool { return parentEnabled })
			manager.Register(launch.Tools)
			launch.ContextManagement = true
		}
		return launch, err
	}
	if _, err := fixture.runner.Run(context.Background(), RunRequest{Task: "fix cache", ChildID: "source"}, nil); err != nil {
		t.Fatal(err)
	}
	sourceDir := session.ChildSessionDir(fixture.sessionPath, "source")
	sourceTree, err := os.ReadFile(filepath.Join(sourceDir, "tree.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	parentEnabled = true // The pinned child's continuation must remain compatible.
	if _, err := fixture.runner.Run(context.Background(), RunRequest{Task: "continue", ChildID: "continued", ContinueChildID: "source"}, nil); err != nil {
		t.Fatal(err)
	}
	requests := fixture.provider.Requests
	if len(requests) != 6 {
		t.Fatalf("model requests = %d, want 6 without a summarization call", len(requests))
	}
	last, _ := json.Marshal(requests[5].Messages)
	if !strings.Contains(string(last), "First fix failed") || !strings.Contains(string(last), "Original failure: stale cache") {
		t.Fatalf("continued child could not recover notes and original evidence: %s", last)
	}
	after, err := os.ReadFile(filepath.Join(sourceDir, "tree.ndjson"))
	if err != nil || !bytes.Equal(after, sourceTree) {
		t.Fatal("continuation modified source history")
	}
	continuedDir := session.ChildSessionDir(fixture.sessionPath, "continued")
	if err := os.WriteFile(filepath.Join(continuedDir, "notes", "details", "tests.md"), []byte("new findings"), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(sourceDir, "notes", "details", "tests.md"))
	if err != nil || !strings.Contains(string(text), "First fix failed") {
		t.Fatal("continued notes were not independent of source")
	}
}
