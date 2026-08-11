package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/skills"
	"harness/internal/tools"
)

func TestCompleteSkillReadPinsInstructionsAndStoresReceipt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	body := "---\nname: focused-review\ndescription: Review carefully\n---\n\nFOLLOW THIS SKILL BODY"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"path":%q,"paths":["removed/SKILL.md"],"files":["removed/SKILL.md"]}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{toolDone(0, "skill-read", "read", input)},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	sink := &archiveSink{archive: ToolResultArchive{
		DisplayPath: "artifacts/tool-results/skill-read.txt",
		ModelPath:   "/session/artifacts/tool-results/skill-read.txt",
	}}
	// Skills always read via the full catalog, independent of the narrowed auto Default.
	a := newAgent(fp, tools.Catalog(), Options{})

	if err := a.RunPrompt(context.Background(), "use the skill", sink); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}
	mustValid(t, a.Transcript())
	if len(fp.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(fp.Requests))
	}
	if got := strings.Join(fp.Requests[0].RequestContext, "\n"); strings.Contains(got, "FOLLOW THIS SKILL BODY") {
		t.Fatalf("skill was active before its read completed: %q", got)
	}
	contextText := strings.Join(fp.Requests[1].RequestContext, "\n")
	for _, want := range []string{
		"[active skill instructions]",
		"source: " + path,
		"FOLLOW THIS SKILL BODY",
		"[end active skill instructions]",
	} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("next request context missing %q:\n%s", want, contextText)
		}
	}
	result := toolResultText(fp.Requests[1].Messages, "skill-read")
	for _, want := range []string{
		"[skill activation receipt]",
		"status: activated",
		"source: " + path,
		"instructions: pinned in request context for this prompt",
		"/session/artifacts/tool-results/skill-read.txt",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("receipt missing %q:\n%s", want, result)
		}
	}
	if strings.Contains(result, "FOLLOW THIS SKILL BODY") {
		t.Fatalf("receipt retained full skill body:\n%s", result)
	}
	if len(sink.archived) != 1 {
		t.Fatalf("archived results = %+v", sink.archived)
	}
	if len(sink.activations) != 1 ||
		sink.activations[0] != (SkillActivationEvent{Source: "read", Status: "activated"}) {
		t.Fatalf("activation events = %+v", sink.activations)
	}
	archived := sink.archived[0]
	if !archived.Truncated || !strings.Contains(archived.OriginalText, "6\tFOLLOW THIS SKILL BODY") ||
		archived.OriginalBytes != len(archived.OriginalText) {
		t.Fatalf("archived exact result = %+v", archived)
	}
}

func TestRepeatedSkillReadKeepsOnePinnedContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(path, []byte("REPEAT BODY"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"path":%q}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "read-1", "read", input)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "read-2", "read", input)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Catalog(), Options{})
	sink := &archiveSink{archive: ToolResultArchive{ModelPath: "/session/full.txt"}}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatal(err)
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("requests = %d", len(fp.Requests))
	}
	for _, request := range fp.Requests[1:] {
		contextText := strings.Join(request.RequestContext, "\n")
		if count := strings.Count(contextText, "REPEAT BODY"); count != 1 {
			t.Fatalf("active skill body count = %d, want 1:\n%s", count, contextText)
		}
	}
	if result := toolResultText(fp.Requests[2].Messages, "read-2"); !strings.Contains(result, "status: already active") {
		t.Fatalf("second result is not an already-active receipt: %q", result)
	}
	if len(sink.activations) != 2 || sink.activations[1].Status != "already_active" {
		t.Fatalf("activation events = %+v", sink.activations)
	}
}

func TestSkillActivationKeepsFullResultWhenArtifactWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(path, []byte("DURABLE BODY"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"path":%q}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "skill-read", "read", input)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Catalog(), Options{})
	sink := &archiveSink{archiveErr: errors.New("disk unavailable")}

	if err := a.RunPrompt(context.Background(), "go", sink); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fp.Requests[1].RequestContext, "\n"); !strings.Contains(got, "DURABLE BODY") {
		t.Fatalf("skill was not activated after archive failure: %q", got)
	}
	result := toolResultText(fp.Requests[1].Messages, "skill-read")
	if !strings.Contains(result, "1\tDURABLE BODY") || strings.Contains(result, "[skill activation receipt]") {
		t.Fatalf("exact read result was not retained after archive failure: %q", result)
	}
}

func TestExplicitSkillContextReportsActivationWithoutToolRound(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn})
	a := newAgent(fp, tools.Catalog(), Options{})
	sink := &archiveSink{}
	contextText := skills.ActiveContext("commit", "/skills/commit/SKILL.md", "DIRECT BODY")

	if err := a.RunPromptContentWithContext(context.Background(), "go", nil, []string{contextText}, 1, sink); err != nil {
		t.Fatal(err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("requests = %d, want one direct model request", len(fp.Requests))
	}
	if got := strings.Join(fp.Requests[0].RequestContext, "\n"); !strings.Contains(got, "DIRECT BODY") {
		t.Fatalf("direct body missing from request context: %q", got)
	}
	if len(sink.activations) != 1 ||
		sink.activations[0] != (SkillActivationEvent{Source: "explicit", Status: "activated"}) {
		t.Fatalf("activation events = %+v", sink.activations)
	}
}

func TestCompleteSkillReadAcceptsNormalizedSkillPaths(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Catalog(), Options{})
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "skill scheme", path: "skill://focused/SKILL.md"},
		{name: "case insensitive basename", path: filepath.Join(t.TempDir(), "skill.md")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := llm.ToolCall{Name: "read", Input: []byte(fmt.Sprintf(`{"path":%q}`, tc.path))}
			path, body, ok := a.completeSkillRead(call, "1\tfull body")
			if !ok || body != "full body" {
				t.Fatalf("completeSkillRead = path %q body %q ok %v", path, body, ok)
			}
			if strings.HasPrefix(path, "skill://") {
				t.Fatalf("normalized path retained skill scheme: %q", path)
			}
		})
	}
}

func TestPartialSkillReadDoesNotActivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	var body strings.Builder
	for i := 0; i < 501; i++ {
		fmt.Fprintf(&body, "instruction %d\n", i)
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"path":%q}`, path)
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "partial", "read", input)}, Stop: llm.StopToolUse},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("done")}, Stop: llm.StopEndTurn},
	)
	a := newAgent(fp, tools.Catalog(), Options{})

	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fp.Requests[1].RequestContext, "\n"); strings.Contains(got, "[active skill instructions]") {
		t.Fatalf("partial skill read activated:\n%s", got)
	}
	result := toolResultText(fp.Requests[1].Messages, "partial")
	if !strings.Contains(result, "[file truncated") || strings.Contains(result, "[skill activation receipt]") {
		t.Fatalf("partial result = %q", result)
	}
}

func toolResultText(messages []llm.Message, id string) string {
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockToolResult && block.ResultForID == id {
				return block.ResultText
			}
		}
	}
	return ""
}
