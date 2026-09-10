package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/plan"
	"harness/internal/session"
	"harness/internal/taskcontext"
	"harness/internal/todo"
	"harness/internal/tools"
)

type continuityFixture struct {
	a        *Agent
	manager  *taskcontext.Manager
	policy   taskcontext.Policy
	registry *tools.Registry
	dir      string
	tree     *session.Tree
	archives []CompactionArchive
}

func newContinuityFixture(t *testing.T, provider llm.Provider) *continuityFixture {
	t.Helper()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	f := &continuityFixture{
		policy:   taskcontext.Policy{Reset: true, Identity: provider.Name() + ":large"},
		registry: &tools.Registry{}, dir: t.TempDir(),
		tree: session.NewTree(now, "", "", ""),
	}
	f.manager = taskcontext.New(func() string { return f.dir })
	f.manager.SetPolicy(func() taskcontext.Policy { return f.policy })
	f.manager.Register(f.registry)
	f.a = newAgent(provider, f.registry, Options{
		Model: "large", ContextWindow: 100_000, DisableAutoCompaction: true,
		RetentionPolicy: RetentionPolicyDisabled, Now: func() time.Time { return now },
	})
	f.a.SetCompactionArchiver(func(_ context.Context, archive CompactionArchive) (string, error) {
		ref, err := session.SaveCompaction(f.dir, session.Compaction{
			Messages: archive.Messages, Summary: archive.Summary, SummarySource: archive.SummarySource,
		})
		if err != nil {
			return "", err
		}
		if err := f.tree.PrepareCompaction(f.a.Transcript(), len(archive.Messages), archive.Summary, ref, archive.TokensBefore, "", nil, nil); err != nil {
			return "", err
		}
		f.archives = append(f.archives, archive)
		return ref, nil
	})
	return f
}

func (f *continuityFixture) run(t *testing.T, name, input string) string {
	t.Helper()
	tool, ok := f.registry.Lookup(name)
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	text, err := tool.Run(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return text
}

func (f *continuityFixture) save(t *testing.T) {
	t.Helper()
	if err := llm.ValidateTranscript(f.a.Transcript()); err != nil {
		t.Fatal(err)
	}
	if err := f.tree.SyncTranscript(f.a.Transcript()); err != nil {
		t.Fatal(err)
	}
	if err := f.tree.Save(f.dir); err != nil {
		t.Fatal(err)
	}
}

func continuityJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func continuityContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %.2500s", want, text)
		}
	}
}

func continuityMemoryTools(t *testing.T, specs []llm.ToolSchema, memory, reset bool) {
	t.Helper()
	found := make(map[string]bool)
	for _, spec := range specs {
		found[spec.Name] = true
	}
	for _, name := range taskcontext.Names {
		want := memory
		if name == "new_context" {
			want = reset
		}
		if found[name] != want {
			t.Errorf("tool %s available = %v, want %v", name, found[name], want)
		}
	}
}

func TestContinuityResetOrdinaryRecoverReconcile(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII="
	image := llm.ContentBlock{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: png, ImageDetail: "high"}
	original := userText("Fix the screenshot regression without losing the original image.")
	original.Origin = llm.MessageOriginPrompt
	original.Content = append(original.Content, image)
	const evidence = "Original screenshot exposes stale generation 17"
	result := toolResult("screen", evidence+strings.Repeat(" diagnostic detail", 2000))
	result.Content[0].ResultContent = []llm.ContentBlock{image}
	source := llmtest.New("reset")
	f := newContinuityFixture(t, source)
	f.a.SetSystem("Keep system instructions outside the transcript.")
	f.a.SetTranscript([]llm.Message{original, asstToolUse("screen", "view_image", `{"path":"screen.png"}`), result})
	f.run(t, "task_notes", `{"text":"Initial checkpoint: recover screenshot evidence before fixing the cache."}`)
	f.run(t, "new_context", `{}`)
	if changed, err := f.a.applyContextEpoch(context.Background(), &recordSink{}); err != nil || !changed {
		t.Fatalf("initial reset: changed=%v err=%v", changed, err)
	}
	f.save(t)
	if source.RequestCount() != 0 || len(f.a.Transcript()) != 1 {
		t.Fatal("notes reset summarized with the model or retained old conversation")
	}
	if got := f.a.Transcript()[0].Compaction.UserInstructions; !reflect.DeepEqual(got, original.Content) {
		t.Fatalf("reset changed original user instructions/image: %+v", got)
	}
	continuityUserImage(t, f.a.Transcript()[0], image)

	const work = "Intervening ordinary-model work: generation check implemented; race test still pending."
	ordinarySummary := work + strings.Repeat(" Preserve the generation-check decision.", 200)
	ordinary := llmtest.New("ordinary", summaryStep(work+strings.Repeat(" implementation detail", 2000), 1500, 100), summaryStep(ordinarySummary, 2000, 100))
	beforeSwitch := cloneMessages(f.a.Transcript())
	f.policy = taskcontext.Policy{Identity: "ordinary:small"}
	f.a.SetProvider(ordinary)
	f.a.SetModel("small", 24_000)
	continuityMemoryTools(t, f.a.ToolSpecs(), true, false)
	if !reflect.DeepEqual(beforeSwitch, f.a.Transcript()) {
		t.Fatal("ordinary model switch mutated the canonical image-bearing checkpoint")
	}
	if f.manager.ContextEnabled() || f.a.contextManager() != nil {
		t.Fatal("ordinary provider enabled notes reset")
	}
	reset, _ := f.registry.Lookup("new_context")
	if _, err := reset.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("ordinary provider accepted new_context")
	}
	continuityContains(t, f.run(t, "task_notes", `{"action":"read"}`), "Initial checkpoint")
	var found struct {
		Hits []struct {
			ID string `json:"id"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(f.run(t, "history_search", continuityJSON(t, map[string]any{"query": evidence}))), &found); err != nil || len(found.Hits) == 0 {
		t.Fatalf("archived evidence not searchable: %+v, %v", found, err)
	}
	var recovered struct {
		Text       string `json:"text"`
		ImageCount int    `json:"image_count"`
		Image      *struct {
			Path string `json:"path"`
		} `json:"image"`
	}
	body := f.run(t, "history_read", continuityJSON(t, map[string]any{"id": found.Hits[0].ID, "image_index": 0}))
	if err := json.Unmarshal([]byte(body), &recovered); err != nil || recovered.Image == nil || recovered.ImageCount != 1 {
		t.Fatalf("archived tool image not recoverable: %s, %v", body, err)
	}
	// Text is paged independently of image selection: the image marker lies
	// after the long tool result, outside this first text page.
	continuityContains(t, recovered.Text, evidence)
	data, err := os.ReadFile(recovered.Image.Path)
	if err != nil || base64.StdEncoding.EncodeToString(data) != png {
		t.Fatalf("archived tool image bytes changed: %v", err)
	}
	if err := f.a.RunPrompt(context.Background(), "Continue the recovered task on the ordinary model.", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	continuityMemoryTools(t, ordinary.Requests[0].Tools, true, false)
	continuityContains(t, strings.Join(ordinary.Requests[0].RequestContext, "\n"), "ordinary compaction", "Initial checkpoint", "history_read")
	if got := ordinary.Requests[0].Messages[0].Compaction.UserInstructions; !reflect.DeepEqual(got, original.Content) {
		t.Fatal("ordinary request lost the original user image")
	}
	continuityUserImage(t, ordinary.Requests[0].Messages[0], image)
	// A checkpoint written while ordinary must not pre-authorize a future reset.
	const stale = "Ordinary checkpoint: generation check implemented; race test pending."
	f.run(t, "task_notes", continuityJSON(t, map[string]any{"text": stale}))
	if _, changed, err := f.a.CompactForContinuation(context.Background(), &recordSink{}); err != nil || !changed {
		t.Fatalf("ordinary compaction: changed=%v err=%v", changed, err)
	}
	f.save(t)
	if ordinary.RequestCount() != 2 || len(f.archives) != 2 || f.archives[1].SummarySource != "model" {
		t.Fatalf("ordinary compaction bypassed model summary: requests=%d archives=%d", ordinary.RequestCount(), len(f.archives))
	}
	continuityContains(t, dump(f.archives[1].Messages), work)
	continuityContains(t, f.a.Transcript()[0].Compaction.Summary, work)
	continuityUserImage(t, f.a.Transcript()[0], image)

	destination := llmtest.New("reset-again")
	f.policy = taskcontext.Policy{Reset: true, Identity: "reset-again:large"}
	f.a.SetProvider(destination)
	f.a.SetModel("large", 100_000)
	continuityMemoryTools(t, f.a.ToolSpecs(), true, true)
	beforeReconcile := cloneMessages(f.a.Transcript())
	for _, input := range []string{`{"action":"read"}`, continuityJSON(t, map[string]any{"text": stale}), `{"action":"append","text":""}`} {
		f.run(t, "task_notes", input)
		if f.manager.ContextEnabled() || f.a.contextManager() != nil {
			t.Fatalf("stale/no-op note operation enabled reset: %s", input)
		}
		if _, err := reset.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Fatal("new_context accepted unreconciled notes")
		}
	}
	if changed, err := f.a.applyContextEpoch(context.Background(), &recordSink{}); err != nil || changed {
		t.Fatalf("blocked reset applied: changed=%v err=%v", changed, err)
	}
	if !reflect.DeepEqual(beforeReconcile, f.a.Transcript()) || destination.RequestCount() != 0 {
		t.Fatal("blocked reset changed intervening work")
	}
	continuityContains(t, f.a.contextManagementContext(), "blocked until reconciliation")
	f.run(t, "task_notes", `{"action":"append","text":"\nReconciled after ordinary compaction: recover generation-check evidence from history; finish the race test."}`)
	if !f.manager.ContextEnabled() {
		t.Fatal("changed nonempty reset-mode checkpoint did not reconcile")
	}
	f.run(t, "new_context", `{}`)
	if changed, err := f.a.applyContextEpoch(context.Background(), &recordSink{}); err != nil || !changed {
		t.Fatalf("reconciled reset: changed=%v err=%v", changed, err)
	}
	f.save(t)
	if destination.RequestCount() != 0 || len(f.archives) != 3 || f.archives[2].SummarySource != "task_notes" {
		t.Fatal("reconciled reset did not use notes plus archive")
	}
	continuityContains(t, dump(f.archives[2].Messages), work)
	continuityContains(t, f.a.Transcript()[0].Compaction.Summary, "Reconciled after ordinary compaction")
	continuityUserImage(t, f.a.Transcript()[0], image)
}

func continuityUserImage(t *testing.T, checkpoint llm.Message, want llm.ContentBlock) {
	t.Helper()
	if checkpoint.Compaction == nil {
		t.Fatal("missing compaction metadata")
	}
	for _, projection := range []struct {
		name   string
		blocks []llm.ContentBlock
	}{
		{"active content", checkpoint.Content},
		{"typed original instructions", checkpoint.Compaction.UserInstructions},
	} {
		var images []llm.ContentBlock
		for _, block := range projection.blocks {
			if block.Kind == llm.BlockImage {
				images = append(images, block)
			}
		}
		if !reflect.DeepEqual(images, []llm.ContentBlock{want}) {
			t.Errorf("%s lost or changed original user image after %s compaction", projection.name, checkpoint.Compaction.SummarySource)
		}
	}
}

func TestContinuityPendingResetCancelledByIdentityAndBudgetUsesCurrentWindow(t *testing.T) {
	f := newContinuityFixture(t, llmtest.New("reset"))
	f.a.SetTranscript([]llm.Message{userText("Continue the task"), asstText(strings.Repeat("evidence ", 1000))})
	f.run(t, "new_context", `{}`)
	before := cloneMessages(f.a.Transcript())
	// Eligibility stays true; identity alone must invalidate a queued reset.
	f.policy.Identity = "reset:different-model"
	f.a.SetModel("different-model", 100_000)
	if changed, err := f.a.applyContextEpoch(context.Background(), &recordSink{}); err != nil || changed {
		t.Fatalf("identity switch applied old reset: changed=%v err=%v", changed, err)
	}
	if !reflect.DeepEqual(before, f.a.Transcript()) || len(f.archives) != 0 {
		t.Fatal("cancelled pending reset mutated transcript")
	}
	f.a.contextManagementContext()
	var large, small struct {
		TokensLeft int  `json:"tokens_left"`
		Limit      int  `json:"limit"`
		Estimated  bool `json:"estimated"`
	}
	if err := json.Unmarshal([]byte(f.run(t, "get_context_remaining", `{}`)), &large); err != nil {
		t.Fatal(err)
	}
	ordinary := llmtest.New("ordinary", llmtest.Step{Events: []llm.StreamEvent{toolDone(0, "budget", "get_context_remaining", `{}`)}, Stop: llm.StopToolUse}, summaryStep("continued", 1000, 10))
	f.policy = taskcontext.Policy{Identity: "ordinary:small"}
	f.a.SetProvider(ordinary)
	f.a.SetModel("small", 24_000)
	if err := f.a.RunPrompt(context.Background(), "Check headroom before more work.", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if ordinary.RequestCount() != 2 {
		t.Fatalf("requests = %d, want budget tool and continuation", ordinary.RequestCount())
	}
	var budgetText string
	for _, message := range ordinary.Requests[1].Messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockToolResult && block.ResultForID == "budget" {
				budgetText = block.ResultText
			}
		}
	}
	if err := json.Unmarshal([]byte(budgetText), &small); err != nil {
		t.Fatalf("budget result: %q, %v", budgetText, err)
	}
	if !small.Estimated || small.Limit != f.a.contextSoftLimit() || small.Limit >= large.Limit || small.Limit > 24_000 || small.TokensLeft <= 0 || small.TokensLeft >= large.TokensLeft || small.TokensLeft > small.Limit {
		t.Fatalf("budget did not follow smaller ordinary model: large=%+v small=%+v", large, small)
	}
	continuityMemoryTools(t, ordinary.Requests[1].Tools, true, false)
	continuityContains(t, strings.Join(ordinary.Requests[1].RequestContext, "\n"), "Ordinary compaction is active")
}

func (f *continuityFixture) coordination(t *testing.T) (string, *todo.Store) {
	t.Helper()
	store := todo.NewStore()
	plans := plan.NewStore()
	f.registry.Register(todo.NewTool(store))
	f.registry.Register(plan.NewTool(plans, func() string { return f.dir }).WithNoteReader(taskcontext.ReadNote))
	f.a.SetTools(f.registry)
	f.run(t, "update_todos", `{"todos":[{"step":"Already inspected the cache","status":"completed"},{"step":"Fix generation handling","status":"in_progress"},{"step":"Run race regression","status":"pending"}]}`)
	f.run(t, "record_plan", `{"title":"Superseded plan","plan":"Old approach."}`)
	f.run(t, "task_notes", `{"path":"draft/repair.md","text":"Implement generation check and verify the race regression."}`)
	f.run(t, "record_plan", `{"title":"Current repair plan","path":"draft/repair.md"}`)
	published, ok := plans.Latest()
	if !ok || !filepath.IsAbs(published.Path) || !strings.Contains(published.Path, string(filepath.Separator)+"plans"+string(filepath.Separator)) {
		t.Fatalf("missing immutable plan publication: %+v", published)
	}
	f.run(t, "task_notes", `{"path":"draft/repair.md","text":"Unpublished draft change must not alter the plan."}`)
	data, err := os.ReadFile(published.Path)
	if err != nil || string(data) != plan.Render(published) {
		t.Fatalf("draft mutation changed published plan: %v", err)
	}
	return published.Path, store
}

func continuityCoordination(t *testing.T, text, planPath string) {
	t.Helper()
	continuityContains(t, text, "Fix generation handling", "Run race regression", "Latest published plan:", planPath, "Current repair plan")
	for _, step := range []string{"Fix generation handling", "Run race regression"} {
		if strings.Count(text, step) != 1 {
			t.Errorf("unresolved TODO %q duplicated in recovery", step)
		}
	}
	for _, unwanted := range []string{"Already inspected the cache", "Superseded plan", "Unpublished draft change"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("recovery included stale/completed coordination: %q", unwanted)
		}
	}
}

func TestContinuityNotesResetAndSwitchProjectCanonicalCoordination(t *testing.T) {
	f := newContinuityFixture(t, llmtest.New("reset"))
	planPath, todos := f.coordination(t)
	const checkpoint = "Evidence: stale generation observed; preserve this fact, not a duplicate checklist."
	f.run(t, "task_notes", continuityJSON(t, map[string]any{"text": checkpoint}))
	f.a.SetTranscript([]llm.Message{userText("Fix the cache"), asstText(strings.Repeat("old work ", 1000))})
	if _, err := f.a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatal(err)
	}
	continuityCoordination(t, f.a.Transcript()[0].Compaction.Summary, planPath)
	if got, err := taskcontext.ReadNote(f.dir, ""); err != nil || got != checkpoint {
		t.Fatalf("reset copied coordination into mutable notes: %q, %v", got, err)
	}
	ordinary := llmtest.New("ordinary", summaryStep("continue current plan", 1000, 10))
	f.policy = taskcontext.Policy{Identity: "ordinary:small"}
	f.a.SetProvider(ordinary)
	if err := f.a.RunPrompt(context.Background(), "Continue without repeating completed work.", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	continuityCoordination(t, strings.Join(ordinary.Requests[0].RequestContext, "\n"), planPath)
	if got := todos.Snapshot(); len(got) != 3 || got[0].Status != todo.StatusCompleted {
		t.Fatal("recovery projection changed the canonical TODO store")
	}
}

func continuityHandoffPending(t *testing.T, dir string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "context-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Pending bool `json:"handoff_pending"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.Pending
}

func TestContinuityFailedSendRetainsHandoff(t *testing.T) {
	want := &llm.APIError{StatusCode: 401}
	p := llmtest.New("reset", llmtest.Step{Err: want}, summaryStep("recovered", 100, 10))
	f := newContinuityFixture(t, p)
	f.run(t, "task_notes", `{"text":"Recovery evidence survives failed sends."}`)
	f.policy.Off = true
	f.a.SetProvider(p)
	if err := f.a.RunPrompt(context.Background(), "continue", &recordSink{}); !errors.Is(err, want) {
		t.Fatalf("failed send: %v", err)
	}
	if !continuityHandoffPending(t, f.dir) {
		t.Fatal("failed model attempt acknowledged the handoff")
	}
	if err := f.a.RunPrompt(context.Background(), "try again", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if continuityHandoffPending(t, f.dir) {
		t.Fatal("successful model attempt did not acknowledge handoff")
	}
	continuityContains(t, strings.Join(p.Requests[len(p.Requests)-1].RequestContext, "\n"), "Recovery evidence survives failed sends.")
}

func TestContinuityOffHandoffWaitsForActualSend(t *testing.T) {
	for _, failure := range []string{"cancelled", "invalid-image"} {
		t.Run(failure, func(t *testing.T) {
			provider := llmtest.New("reset", summaryStep("recovered with standard read", 1000, 10))
			f := newContinuityFixture(t, provider)
			planPath, _ := f.coordination(t)
			f.run(t, "task_notes", `{"text":"Saved checkpoint preview: generation mismatch identified."}`)
			f.policy.Off = true
			f.a.SetProvider(provider)
			continuityMemoryTools(t, f.a.ToolSpecs(), false, false)
			peek := f.a.contextManagementContext()
			continuityContains(t, peek, "standard read tool", filepath.Join(f.dir, "task-notes.md"), filepath.Join(f.dir, "notes"), filepath.Join(f.dir, "tree.ndjson"), "Saved checkpoint preview")
			continuityCoordination(t, peek, planPath)
			if again := f.a.contextManagementContext(); again != peek || !continuityHandoffPending(t, f.dir) {
				t.Fatal("guidance peek consumed the pending handoff")
			}
			var err error
			if failure == "cancelled" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				err = f.a.RunPrompt(ctx, "not sent", &recordSink{})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled request: %v", err)
				}
			} else {
				bad := llm.ContentBlock{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "not-base64"}
				err = f.a.RunPromptContent(context.Background(), "invalid image must not send", []llm.ContentBlock{bad}, &recordSink{})
			}
			if err == nil || provider.RequestCount() != 0 || !continuityHandoffPending(t, f.dir) {
				t.Fatalf("pre-send failure acknowledged handoff: err=%v requests=%d", err, provider.RequestCount())
			}
			if got := f.a.contextManagementContext(); got != peek {
				t.Fatal("pre-send failure changed recovery handoff")
			}
			if err := f.a.RunPrompt(context.Background(), "Recover the saved task with standard tools.", &recordSink{}); err != nil {
				t.Fatal(err)
			}
			if provider.RequestCount() != 1 || continuityHandoffPending(t, f.dir) {
				t.Fatal("actual send did not acknowledge handoff")
			}
			request := provider.Requests[0]
			continuityMemoryTools(t, request.Tools, false, false)
			continuityContains(t, strings.Join(request.RequestContext, "\n"), peek)
			if err := llm.ValidateTranscript(f.a.Transcript()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
