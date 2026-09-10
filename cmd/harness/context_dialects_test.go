package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/session"
	"harness/internal/taskcontext"
	"harness/internal/tools"
)

// The manager is deliberately enabled independently of CLI provider policy. This
// exercises the real dialect encoders/decoders without putting factory in agent.
func TestContextNotesRecoveryAcrossDialects(t *testing.T) {
	const (
		system   = "SYSTEM_SENTINEL: preserve the user's compatibility constraints."
		original = "Fix the cache invalidation race without changing the public API."
		prompt   = "Continue that task; use this screenshot and recover the failed test."
		evidence = "STALE_GENERATION_17: TestInvalidation failed"
		note     = "Goal: fix cache; first attempt failed; next: inspect the generation guard."
		final    = "Recovered the notes and failed test; continue with the generation guard."
		png      = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII="
	)
	for _, dialect := range []struct{ name, model, path, previous string }{
		{"anthropic", "claude-test", "/messages", ""},
		{"interactions", "gemini-test", "/interactions", "previous_interaction_id"},
		{"openai", "chat-test", "/chat/completions", ""},
		{"responses", "responses-test", "/responses", "previous_response_id"},
	} {
		t.Run(dialect.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method != http.MethodPost {
					t.Errorf("method = %s", r.Method)
					http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
					return
				}
				// Token counting is not a summarization/model round.
				if r.URL.Path == "/messages/count_tokens" || r.URL.Path == "/responses/input_tokens" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"input_tokens":1000}`)
					return
				}
				if r.URL.Path != dialect.path {
					t.Errorf("unexpected model endpoint (including native summary): %s", r.URL.Path)
					http.Error(w, "unexpected endpoint", http.StatusBadRequest)
					return
				}
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode request: %v", err)
					http.Error(w, "bad JSON", http.StatusBadRequest)
					return
				}
				round := len(requests)
				requests = append(requests, req)
				var calls []contextDialectCall
				switch round {
				case 0:
					calls = []contextDialectCall{{"save-notes", "task_notes", map[string]any{"text": note}}}
				case 1:
					calls = []contextDialectCall{{"reset-window", "new_context", map[string]any{}}}
				case 2:
					calls = []contextDialectCall{
						{"recover-notes", "task_notes", map[string]any{"action": "read"}},
						{"find-evidence", "history_search", map[string]any{"query": evidence}},
					}
				case 3:
					var result any
					if err := json.Unmarshal([]byte(contextDialectResult(req, "find-evidence")), &result); err != nil {
						t.Errorf("decode history tool result: %v", err)
					}
					id := contextDialectHistoryID(result)
					if id == "" {
						t.Error("history_search returned no stable entry ID to the model")
					}
					calls = []contextDialectCall{{"recover-evidence", "history_read", map[string]any{"id": id}}}
				case 4:
				default:
					t.Errorf("unexpected model round %d: reset must not call a summarizer", round)
					http.Error(w, "unexpected round", http.StatusBadRequest)
					return
				}
				contextDialectStream(w, dialect.name, round, calls, final)
			}))
			defer server.Close()

			provider, err := factory.New(factory.Options{Provider: dialect.name, Model: dialect.model, BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			registry := &tools.Registry{}
			manager := taskcontext.New(func() string { return dir })
			manager.SetEnabled(func() bool { return true })
			manager.Register(registry)
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			a := agent.New(provider, registry, agent.Options{
				Model: dialect.model, MaxTurns: 8, ContextWindow: 100_000,
				ResponsesStateful: dialect.previous != "", NativeCompaction: true,
				DisableAutoCompaction: true, RetentionPolicy: agent.RetentionPolicyDisabled,
				Now: func() time.Time { return now },
			})
			a.SetSystem(system)
			// Substantial prior evidence makes the reset reclaim real context, not
			// just replace the latest tool round with a larger recovery hint.
			a.SetTranscript([]llm.Message{
				{Role: llm.RoleUser, Origin: llm.MessageOriginPrompt, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: original}}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: evidence + strings.Repeat("\nold diagnostic detail", 1000)}}},
			})
			tree := session.NewTree(now, dir, "", "")
			var archives []agent.CompactionArchive
			a.SetCompactionArchiver(func(_ context.Context, archive agent.CompactionArchive) (string, error) {
				archives = append(archives, archive)
				ref, err := session.SaveCompaction(dir, session.Compaction{Time: now, Messages: archive.Messages, Summary: archive.Summary, SummarySource: archive.SummarySource})
				if err != nil {
					return "", err
				}
				return ref, tree.PrepareCompaction(a.Transcript(), len(archive.Messages), archive.Summary, ref, archive.TokensBefore, "", nil, nil)
			})
			var fresh llm.Request
			var rewriteErr error
			rewrites := 0
			sink := &contextDialectSink{rewrite: func() {
				rewrites++
				fresh = a.DebugRequest(false, "", nil, nil).Request
				if rewriteErr = tree.SyncTranscript(a.Transcript()); rewriteErr == nil {
					rewriteErr = tree.Save(dir)
				}
			}}
			image := llm.ContentBlock{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: png, ImageDetail: "high"}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := a.RunPromptContent(ctx, prompt, []llm.ContentBlock{image}, sink); err != nil {
				t.Fatal(err)
			}
			if rewriteErr != nil {
				t.Fatal(rewriteErr)
			}
			if rewrites != 1 || len(archives) != 1 || archives[0].SummarySource != "task_notes" {
				t.Fatalf("rewrites=%d archives=%+v", rewrites, archives)
			}
			if !strings.Contains(archives[0].Summary, note) {
				t.Fatal("archive omitted the saved notes preview")
			}
			if sink.text.String() != final {
				t.Fatalf("final answer = %q", sink.text.String())
			}
			if fresh.System != system || fresh.PreviousResponseID != "" || len(fresh.Messages) != 1 {
				t.Fatalf("fresh request retained stale state: system=%q previous=%q messages=%d", fresh.System, fresh.PreviousResponseID, len(fresh.Messages))
			}
			checkpoint := fresh.Messages[0]
			if checkpoint.Compaction == nil || !reflect.DeepEqual(checkpoint.Compaction.UserInstructions, []llm.ContentBlock{
				{Kind: llm.BlockText, Text: original}, image, {Kind: llm.BlockText, Text: prompt},
			}) {
				t.Fatalf("original user instructions/image changed: %+v", checkpoint.Compaction)
			}
			for _, transcript := range [][]llm.Message{archives[0].Messages, fresh.Messages, a.Transcript()} {
				if err := llm.ValidateTranscript(transcript); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(contextDialectJSON(transcript), system) {
					t.Error("system prompt leaked into transcript")
				}
			}
			mu.Lock()
			got := append([]map[string]any(nil), requests...)
			mu.Unlock()
			if len(got) != 5 {
				t.Fatalf("model requests=%d, want exactly five (no summarization)", len(got))
			}
			for _, index := range []int{0, 2} {
				sys, conversation := contextDialectRequestParts(dialect.name, got[index])
				body := contextDialectJSON(conversation)
				if !strings.Contains(sys, system) || strings.Contains(body, system) {
					t.Errorf("round %d: system placement changed", index)
				}
				for _, want := range []string{original, prompt, png} {
					if !strings.Contains(body, want) {
						t.Errorf("round %d: missing original user content %q", index, want)
					}
				}
				if !contextDialectHasImage(conversation, png) {
					t.Errorf("round %d: image was not projected as a typed image", index)
				}
			}
			_, conversation := contextDialectRequestParts(dialect.name, got[2])
			if !strings.Contains(checkpoint.Compaction.Summary, note) || !strings.Contains(contextDialectJSON(conversation), note) {
				t.Fatal("fresh checkpoint/wire request omitted the notes preview")
			}
			for _, stale := range []string{evidence, "old diagnostic detail", "save-notes", "reset-window", "Context refresh queued"} {
				if strings.Contains(contextDialectJSON(conversation), stale) {
					t.Errorf("fresh wire request replayed stale conversation: %q", stale)
				}
			}
			if dialect.previous != "" {
				if got[1][dialect.previous] != "response-0" || got[2][dialect.previous] != nil || got[3][dialect.previous] != "response-2" || got[4][dialect.previous] != "response-3" {
					t.Errorf("continuation did not reset then resume: %v / %v / %v / %v", got[1][dialect.previous], got[2][dialect.previous], got[3][dialect.previous], got[4][dialect.previous])
				}
			}
			if !strings.Contains(contextDialectResult(got[3], "recover-notes"), note) || !strings.Contains(contextDialectResult(got[4], "recover-evidence"), evidence) {
				t.Error("model did not receive recovered notes and exact archived evidence as tool results")
			}
			for _, message := range a.Transcript() {
				for _, block := range message.Content {
					if block.Kind == llm.BlockToolResult && block.ResultError {
						t.Errorf("recovery tool %s failed: %s", block.ResultForID, block.ResultText)
					}
				}
			}
			if err := tree.SyncTranscript(a.Transcript()); err != nil {
				t.Fatal(err)
			}
			if err := tree.Save(dir); err != nil {
				t.Fatal(err)
			}
			loaded, err := session.LoadTree(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			restored, err := loaded.BuildContext()
			if err != nil || !reflect.DeepEqual(restored, a.Transcript()) {
				t.Fatalf("saved tree did not restore the final transcript: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "compactions", "0001.input.json"))
			if err != nil || !strings.Contains(string(data), evidence) || !strings.Contains(string(data), "reset-window") {
				t.Fatalf("archive lost original evidence/reset round: %v", err)
			}
		})
	}
}

type contextDialectCall struct {
	id, name string
	input    map[string]any
}

// Emit the minimal successful SSE shape for each actual provider decoder.
func contextDialectStream(w http.ResponseWriter, dialect string, round int, calls []contextDialectCall, final string) {
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, value any) {
		if kind != "" {
			fmt.Fprintf(w, "event: %s\n", kind)
		}
		fmt.Fprintf(w, "data: %s\n\n", contextDialectJSON(value))
	}
	id := fmt.Sprintf("response-%d", round)
	switch dialect {
	case "openai":
		delta := map[string]any{"content": final}
		stop := "stop"
		if len(calls) != 0 {
			var wireCalls []any
			for i, call := range calls {
				wireCalls = append(wireCalls, map[string]any{"index": i, "id": call.id, "type": "function", "function": map[string]any{"name": call.name, "arguments": contextDialectJSON(call.input)}})
			}
			delta = map[string]any{"tool_calls": wireCalls}
			stop = "tool_calls"
		}
		event("", map[string]any{"id": id, "choices": []any{map[string]any{"index": 0, "delta": delta}}})
		event("", map[string]any{"id": id, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": stop}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	case "anthropic":
		event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "role": "assistant", "usage": map[string]any{"input_tokens": 1000, "output_tokens": 0}}})
		stop := "end_turn"
		if len(calls) == 0 {
			event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": final}})
			event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		} else {
			stop = "tool_use"
			for i, call := range calls {
				event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": call.id, "name": call.name, "input": map[string]any{}}})
				event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": contextDialectJSON(call.input)}})
				event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
			}
		}
		event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 20}})
		event("message_stop", map[string]any{"type": "message_stop"})
	case "interactions":
		if len(calls) == 0 {
			event("", map[string]any{"event_type": "step.start", "index": 0, "step": map[string]any{"type": "model_output"}})
			event("", map[string]any{"event_type": "step.delta", "index": 0, "delta": map[string]any{"type": "text", "text": final}})
			event("", map[string]any{"event_type": "step.stop", "index": 0})
		} else {
			for i, call := range calls {
				event("", map[string]any{"event_type": "step.start", "index": i, "step": map[string]any{"type": "function_call", "id": call.id, "name": call.name, "arguments": call.input}})
				event("", map[string]any{"event_type": "step.stop", "index": i})
			}
		}
		event("", map[string]any{"event_type": "interaction.completed", "interaction": map[string]any{"id": id, "status": "completed"}})
	case "responses":
		var output []any
		if len(calls) == 0 {
			event("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg-final", "output_index": 0, "content_index": 0, "delta": final})
			output = []any{map[string]any{"id": "msg-final", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": final}}}}
		} else {
			for i, call := range calls {
				item := map[string]any{"id": "fc-" + call.id, "type": "function_call", "call_id": call.id, "name": call.name, "arguments": contextDialectJSON(call.input), "status": "completed"}
				event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
				event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
				output = append(output, item)
			}
		}
		event("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 1000, "output_tokens": 20}}})
	}
}

func contextDialectJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	} // All inputs are JSON values or transcript structs.
	return string(data)
}

func contextDialectRequestParts(dialect string, req map[string]any) (string, any) {
	switch dialect {
	case "anthropic":
		return contextDialectJSON(req["system"]), req["messages"]
	case "interactions":
		return contextDialectJSON(req["system_instruction"]), req["input"]
	case "responses":
		return contextDialectJSON(req["instructions"]), req["input"]
	default:
		var system, conversation []any
		messages, _ := req["messages"].([]any)
		for _, message := range messages {
			item, _ := message.(map[string]any)
			if item["role"] == "system" {
				system = append(system, message)
			} else {
				conversation = append(conversation, message)
			}
		}
		return contextDialectJSON(system), conversation
	}
}

func contextDialectHistoryID(value any) string {
	switch value := value.(type) {
	case string:
		var result struct{ Hits []struct{ ID string } }
		if json.Unmarshal([]byte(value), &result) == nil && len(result.Hits) != 0 {
			return result.Hits[0].ID
		}
	case []any:
		for _, child := range value {
			if id := contextDialectHistoryID(child); id != "" {
				return id
			}
		}
	case map[string]any:
		for _, child := range value {
			if id := contextDialectHistoryID(child); id != "" {
				return id
			}
		}
	}
	return ""
}

// Match only tool results, not an earlier query or the notes preview in the
// checkpoint, so stateless requests cannot make recovery assertions pass by echo.
func contextDialectResult(value any, id string) string {
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			if result := contextDialectResult(child, id); result != "" {
				return result
			}
		}
	case map[string]any:
		if value["type"] == "tool_result" && value["tool_use_id"] == id ||
			value["role"] == "tool" && value["tool_call_id"] == id ||
			(value["type"] == "function_call_output" || value["type"] == "function_result") && value["call_id"] == id {
			return contextDialectJSON(value)
		}
		for _, child := range value {
			if result := contextDialectResult(child, id); result != "" {
				return result
			}
		}
	}
	return ""
}

func contextDialectHasImage(value any, data string) bool {
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			if contextDialectHasImage(child, data) {
				return true
			}
		}
	case map[string]any:
		if kind := value["type"]; kind == "image" || kind == "input_image" || kind == "image_url" {
			return strings.Contains(contextDialectJSON(value), data)
		}
		for _, child := range value {
			if contextDialectHasImage(child, data) {
				return true
			}
		}
	}
	return false
}

type contextDialectSink struct {
	text    strings.Builder
	rewrite func()
}

func (s *contextDialectSink) TextDelta(text string)                          { s.text.WriteString(text) }
func (*contextDialectSink) ReasoningSummary(string)                          {}
func (*contextDialectSink) TurnAttemptStart(int, int, agent.ContextEstimate) {}
func (*contextDialectSink) TurnAttemptComplete(agent.TurnAttemptUsage)       {}
func (*contextDialectSink) ToolUseStart(llm.ToolCall)                        {}
func (*contextDialectSink) ToolUseDelta(int, string)                         {}
func (*contextDialectSink) ToolStart(llm.ToolCall)                           {}
func (*contextDialectSink) ToolResult(llm.ToolResult)                        {}
func (*contextDialectSink) Notice(string)                                    {}
func (*contextDialectSink) TurnComplete(agent.TurnUsage)                     {}
func (*contextDialectSink) PromptComplete(agent.PromptUsage)                 {}
func (s *contextDialectSink) TranscriptRewritten()                           { s.rewrite() }
