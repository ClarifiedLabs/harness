package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"harness/internal/llm"
)

const toolOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

// fakeTool is a configurable Tool for exercising Dispatch.
type fakeTool struct {
	name     string
	desc     string
	schema   string
	readOnly bool
	run      func(ctx context.Context, input json.RawMessage) (string, error)
}

func (f fakeTool) Name() string                  { return f.name }
func (f fakeTool) Description() string           { return f.desc }
func (f fakeTool) Schema() json.RawMessage       { return json.RawMessage(f.schema) }
func (f fakeTool) ReadOnly(json.RawMessage) bool { return f.readOnly }
func (f fakeTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	return f.run(ctx, input)
}

func newOK(name, out string) fakeTool {
	return fakeTool{
		name:   name,
		desc:   "ok tool",
		schema: `{"type":"object"}`,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			return out, nil
		},
	}
}

type preservingFakeTool struct {
	fakeTool
}

func (preservingFakeTool) PreserveSchemaDescriptions() bool { return true }

type sequentialFakeTool struct {
	fakeTool
	requires func(json.RawMessage) bool
}

func (t sequentialFakeTool) RequiresSequential(input json.RawMessage) bool {
	return t.requires(input)
}

type mutationFakeTool struct {
	fakeTool
	paths func(json.RawMessage) ([]string, error)
}

func (t mutationFakeTool) MutatedPaths(input json.RawMessage) ([]string, error) {
	return t.paths(input)
}

type meteredFakeTool struct {
	fakeTool
	result MeteredResult
	err    error
}

func (m meteredFakeTool) RunMetered(context.Context, json.RawMessage) (MeteredResult, error) {
	return m.result, m.err
}

type resultFakeTool struct {
	fakeTool
	result RunResult
	err    error
}

func (f resultFakeTool) RunResult(context.Context, json.RawMessage) (RunResult, error) {
	return f.result, f.err
}

type richFakeTool struct {
	fakeTool
	result     RichResult
	err        error
	richRuns   *int
	legacyRuns *int
	modality   string
}

func (f richFakeTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if f.legacyRuns != nil {
		*f.legacyRuns++
	}
	return f.fakeTool.Run(ctx, input)
}

func (f richFakeTool) RunRich(context.Context, json.RawMessage) (RichResult, error) {
	if f.richRuns != nil {
		*f.richRuns++
	}
	return f.result, f.err
}

func (f richFakeTool) RequiredInputModality() string { return f.modality }

func TestRegistrySupportsParallelDefaultsAndOptOut(t *testing.T) {
	reg := &Registry{}
	mutating := newOK("mutating", "ok")
	mutating.readOnly = false
	readonly := newOK("readonly", "ok")
	readonly.readOnly = true
	conditional := sequentialFakeTool{
		fakeTool: newOK("conditional", "ok"),
		requires: func(input json.RawMessage) bool {
			var args struct {
				Serial bool `json:"serial"`
			}
			return json.Unmarshal(input, &args) == nil && args.Serial
		},
	}
	reg.Register(mutating)
	reg.Register(readonly)
	reg.Register(conditional)

	for _, call := range []llm.ToolCall{
		{Name: "mutating", Input: json.RawMessage(`{}`)},
		{Name: "readonly", Input: json.RawMessage(`{}`)},
		{Name: "conditional", Input: json.RawMessage(`{"serial":false}`)},
		{Name: "missing", Input: json.RawMessage(`{}`)},
	} {
		if !reg.SupportsParallel(call) {
			t.Errorf("SupportsParallel(%q) = false, want true", call.Name)
		}
	}
	if reg.SupportsParallel(llm.ToolCall{Name: "conditional", Input: json.RawMessage(`{"serial":true}`)}) {
		t.Fatal("SequentialTool serial input remained parallel-eligible")
	}
	if !reg.SupportsParallel(llm.ToolCall{Name: "conditional"}) {
		t.Fatal("empty input was not normalized to an eligible object")
	}
}

func TestRegistryMutationKeysNormalizeAndDeduplicate(t *testing.T) {
	rel := filepath.Join("testdata", "..", "target.txt")
	abs, err := filepath.Abs("target.txt")
	if err != nil {
		t.Fatal(err)
	}
	reg := &Registry{}
	reg.Register(mutationFakeTool{
		fakeTool: newOK("mutation", "ok"),
		paths: func(json.RawMessage) ([]string, error) {
			return []string{rel, abs, rel}, nil
		},
	})
	keys := reg.MutationKeys(llm.ToolCall{Name: "mutation", Input: json.RawMessage(`{}`)})
	if len(keys) != 1 || keys[0] != filepath.Clean(abs) {
		t.Fatalf("MutationKeys = %v, want [%s]", keys, filepath.Clean(abs))
	}
	if got := reg.MutationKeys(llm.ToolCall{Name: "unknown"}); got != nil {
		t.Fatalf("unknown MutationKeys = %v, want nil", got)
	}
}

func TestRegistrySpecsOrdered(t *testing.T) {
	r := &Registry{}
	r.Register(newOK("alpha", "a"))
	r.Register(newOK("beta", "b"))
	r.Register(newOK("gamma", "c"))

	specs := r.Specs()
	if len(specs) != 3 {
		t.Fatalf("want 3 specs, got %d", len(specs))
	}
	want := []string{"alpha", "beta", "gamma"}
	for i, s := range specs {
		if s.Name != want[i] {
			t.Errorf("specs[%d].Name = %q, want %q", i, s.Name, want[i])
		}
	}
	if string(specs[0].Parameters) != `{"type":"object"}` {
		t.Errorf("Parameters = %q, want compact schema", specs[0].Parameters)
	}
	if specs[0].Description != "ok tool" {
		t.Errorf("Description not passed through: %q", specs[0].Description)
	}
}

func TestRegistrySpecsStripSchemaDescriptions(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTool{
		name: "schema",
		desc: "model-facing tool description stays",
		schema: `{
			"type": "object",
			"description": "drop top",
			"properties": {
				"name": {"type": "string", "description": "drop nested"}
			}
		}`,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "ok", nil
		},
	})

	specs := r.Specs()
	if specs[0].Description != "model-facing tool description stays" {
		t.Fatalf("tool description = %q", specs[0].Description)
	}
	if strings.Contains(string(specs[0].Parameters), "description") {
		t.Fatalf("schema descriptions were not stripped: %s", specs[0].Parameters)
	}
	want := `{"properties":{"name":{"type":"string"}},"type":"object"}`
	if string(specs[0].Parameters) != want {
		t.Fatalf("schema = %s, want %s", specs[0].Parameters, want)
	}
}

func TestRegistrySpecsCanPreserveSchemaDescriptions(t *testing.T) {
	r := &Registry{}
	r.Register(preservingFakeTool{fakeTool: fakeTool{
		name:   "dynamic_catalog",
		desc:   "catalog tool",
		schema: `{"type":"object","properties":{"agent":{"type":"string","description":"Available agents"}}}`,
		run: func(context.Context, json.RawMessage) (string, error) {
			return "ok", nil
		},
	}})

	specs := r.Specs()
	if !strings.Contains(string(specs[0].Parameters), `"description":"Available agents"`) {
		t.Fatalf("schema description was stripped despite opt-in: %s", specs[0].Parameters)
	}
}

// A schema property may legitimately have the same name as an annotation
// keyword. The sanitizer must remove annotations from schema nodes without
// deleting those properties from the input contract.
func TestRegistrySpecsPreserveAnnotationNamedProperties(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTool{
		name: "annotation_names",
		desc: "annotation names",
		schema: `{
			"type":"object",
			"title":"drop root title",
			"examples":[{"description":"sample"}],
			"properties":{
				"description":{"type":"string","description":"drop field prose"},
				"title":{"type":"string","default":"kept"},
				"examples":{"type":"array","items":[{"type":"string","description":"drop tuple prose"}]}
			}
		}`,
		run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	})

	var schema map[string]any
	if err := json.Unmarshal(r.Specs()[0].Parameters, &schema); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing: %v", schema)
	}
	for _, name := range []string{"description", "title", "examples"} {
		if _, ok := properties[name]; !ok {
			t.Errorf("property %q was mistaken for an annotation: %s", name, r.Specs()[0].Parameters)
		}
	}
	if _, ok := schema["title"]; ok {
		t.Error("root title annotation was not stripped")
	}
	if _, ok := schema["examples"]; ok {
		t.Error("root examples annotation was not stripped")
	}
	title := properties["title"].(map[string]any)
	if title["default"] != "kept" {
		t.Errorf("validation/default metadata changed: %v", title)
	}
	if strings.Contains(string(r.Specs()[0].Parameters), "drop tuple prose") {
		t.Errorf("nested tuple annotation was not stripped: %s", r.Specs()[0].Parameters)
	}
}

func TestRegistrySpecsPreserverStillDropsOtherAnnotations(t *testing.T) {
	r := &Registry{}
	r.Register(preservingFakeTool{fakeTool: fakeTool{
		name:   "catalog",
		desc:   "catalog",
		schema: `{"type":"object","title":"drop","properties":{"agent":{"type":"string","description":"keep","examples":["drop"]}}}`,
		run:    func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}})
	parameters := string(r.Specs()[0].Parameters)
	if !strings.Contains(parameters, `"description":"keep"`) {
		t.Fatalf("preserved description missing: %s", parameters)
	}
	if strings.Contains(parameters, `"title"`) || strings.Contains(parameters, `"examples"`) {
		t.Fatalf("non-description annotations were retained: %s", parameters)
	}
}

func TestBuiltInToolDescriptionsFitBudget(t *testing.T) {
	catalog, _ := CatalogWithOptions(Options{})
	catalog.Register(NewHandoff(nil, nil, true, nil))
	for _, spec := range catalog.Specs() {
		if len(spec.Description) > 80 {
			t.Errorf("%s description = %d bytes, budget 80: %q", spec.Name, len(spec.Description), spec.Description)
		}
		if strings.Contains(spec.Description, "\n") {
			t.Errorf("%s description is not one line: %q", spec.Name, spec.Description)
		}
	}
}

func TestDispatchPreservesMeteredToolUsage(t *testing.T) {
	r := &Registry{}
	r.Register(meteredFakeTool{
		fakeTool: newOK("metered", "ordinary path should not run"),
		result:   MeteredResult{Text: "metered output", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}},
	})

	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "m1", Name: "metered", Input: json.RawMessage(`{}`)})
	if res.Text != "metered output" || res.IsError {
		t.Fatalf("dispatch result = %+v", res)
	}
	if res.Usage.InputTokens != 7 || res.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v, want 7 input / 3 output", res.Usage)
	}
}

func TestDispatchRichToolExactlyOnceAndTruncatesOnlyText(t *testing.T) {
	richRuns, legacyRuns := 0, 0
	image := llm.ContentBlock{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: toolOnePixelPNG, ImageDetail: "high"}
	r := &Registry{}
	r.SetResultLimits(4, 0)
	r.Register(richFakeTool{
		fakeTool:   newOK("rich", "legacy should not run"),
		result:     RichResult{Text: "abcdefgh", Content: []llm.ContentBlock{image}, Usage: llm.Usage{InputTokens: 7}},
		richRuns:   &richRuns,
		legacyRuns: &legacyRuns,
		modality:   "image",
	})

	call := llm.ToolCall{ID: "rich_1", Name: "rich", Input: json.RawMessage(`{}`)}
	res := r.Dispatch(context.Background(), call)
	if res.IsError || !res.Truncated || len(res.Content) != 1 || res.Content[0].ImageData != image.ImageData {
		t.Fatalf("dispatch result = %+v", res)
	}
	if richRuns != 1 || legacyRuns != 0 {
		t.Fatalf("execution counts rich=%d legacy=%d, want 1/0", richRuns, legacyRuns)
	}
	if res.Usage.InputTokens != 7 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if modality, ok := r.RequiredModality(call); !ok || modality != "image" {
		t.Fatalf("RequiredModality = %q, %v", modality, ok)
	}
}

func TestDispatchRichToolRejectsInvalidOrErroredContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []llm.ContentBlock
		err     error
	}{
		{name: "text child", content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "not allowed"}}},
		{name: "nested result", content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "nested"}}},
		{name: "execution error", content: []llm.ContentBlock{{Kind: llm.BlockImage, ImageData: "secret"}}, err: fmt.Errorf("failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Registry{}
			r.Register(richFakeTool{fakeTool: newOK("rich", "legacy"), result: RichResult{Text: "text", Content: tc.content}, err: tc.err})
			res := r.Dispatch(context.Background(), llm.ToolCall{ID: "r", Name: "rich", Input: json.RawMessage(`{}`)})
			if !res.IsError || len(res.Content) != 0 {
				t.Fatalf("dispatch result = %+v, want text-only error", res)
			}
			if strings.Contains(res.Text, "secret") {
				t.Fatalf("image data leaked into error: %q", res.Text)
			}
		})
	}
}

func TestDispatchPreservesResultToolOriginal(t *testing.T) {
	r := &Registry{}
	r.Register(resultFakeTool{
		fakeTool: newOK("result", "ordinary path should not run"),
		result: RunResult{
			Text:         "compact receipt",
			OriginalText: "complete verbose transcript",
			Usage:        llm.Usage{InputTokens: 2},
			Metrics:      map[string]int{"unique_lines": 12},
		},
	})

	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "r1", Name: "result", Input: json.RawMessage(`{}`)})
	if res.Text != "compact receipt" || !res.Truncated || res.OriginalText != "complete verbose transcript" {
		t.Fatalf("dispatch result = %+v", res)
	}
	if res.OriginalBytes != len(res.OriginalText) || res.ShownBytes != len(res.Text) {
		t.Fatalf("result byte metadata = shown %d original %d", res.ShownBytes, res.OriginalBytes)
	}
	if res.Usage.InputTokens != 2 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if res.Metrics["unique_lines"] != 12 {
		t.Fatalf("metrics = %+v", res.Metrics)
	}
}

// The file tools must be reachable from outside the package; consumers (e.g.
// internal/agent) cannot register unexported tool types. Default() exposes a
// registry with all available built-ins so they are not dead code.
func TestDefaultRegistersFileTools(t *testing.T) {
	r := Default()
	if r == nil {
		t.Fatal("Default() returned nil")
	}
	got := map[string]bool{}
	for _, s := range r.Specs() {
		got[s.Name] = true
		if len(s.Parameters) == 0 {
			t.Errorf("tool %q has empty schema", s.Name)
		}
	}
	want := expectedDefaultNames()
	for _, name := range want {
		if !got[name] {
			t.Errorf("Default() missing tool %q", name)
		}
	}
	if len(r.Specs()) != len(want) {
		t.Errorf("Default() should register exactly %d tools, got %d", len(want), len(r.Specs()))
	}
}

func TestRegisterFileTools(t *testing.T) {
	// Derive the file-tool count from a clean registration so this test stays
	// robust as file tools are added or relocated.
	base := &Registry{}
	RegisterFileTools(base)
	fileToolCount := len(base.Specs())

	r := &Registry{}
	r.Register(newOK("existing", "x"))
	RegisterFileTools(r)
	specs := r.Specs()
	// The pre-existing tool keeps its leading position; file tools follow.
	if specs[0].Name != "existing" {
		t.Errorf("registration order not preserved: %q", specs[0].Name)
	}
	if want := fileToolCount + 1; len(specs) != want {
		t.Errorf("want %d tools after registration, got %d", want, len(specs))
	}
}

func TestDispatch(t *testing.T) {
	panicTool := fakeTool{
		name:   "boom",
		desc:   "panics",
		schema: `{"type":"object"}`,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			panic("kaboom")
		},
	}
	errTool := fakeTool{
		name:   "err",
		desc:   "errors",
		schema: `{"type":"object"}`,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "", fmt.Errorf("something broke")
		},
	}
	argTool := fakeTool{
		name:   "needsarg",
		desc:   "validates args",
		schema: `{"type":"object"}`,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var v struct {
				X int `json:"x"`
			}
			if err := json.Unmarshal(input, &v); err != nil {
				return "", err
			}
			return fmt.Sprintf("x=%d", v.X), nil
		},
	}

	r := &Registry{}
	r.Register(newOK("ok", "all good"))
	r.Register(panicTool)
	r.Register(errTool)
	r.Register(argTool)

	tests := []struct {
		name        string
		call        llm.ToolCall
		wantText    string
		wantErr     bool
		wantContain bool // wantText is a substring rather than the whole text
	}{
		{
			name:     "success passes through",
			call:     llm.ToolCall{ID: "1", Name: "ok", Input: json.RawMessage(`{}`)},
			wantText: "all good",
			wantErr:  false,
		},
		{
			name:        "unknown tool",
			call:        llm.ToolCall{ID: "2", Name: "nope", Input: json.RawMessage(`{}`)},
			wantText:    `unknown tool "nope"`,
			wantErr:     true,
			wantContain: true,
		},
		{
			name:        "invalid json args",
			call:        llm.ToolCall{ID: "3", Name: "needsarg", Input: json.RawMessage(`{not json`)},
			wantText:    "invalid arguments: invalid JSON at byte",
			wantErr:     true,
			wantContain: true,
		},
		{
			name:        "tool returns error",
			call:        llm.ToolCall{ID: "4", Name: "err", Input: json.RawMessage(`{}`)},
			wantText:    "something broke",
			wantErr:     true,
			wantContain: true,
		},
		{
			name:        "tool panics",
			call:        llm.ToolCall{ID: "5", Name: "boom", Input: json.RawMessage(`{}`)},
			wantText:    "tool panicked: kaboom",
			wantErr:     true,
			wantContain: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := r.Dispatch(context.Background(), tc.call)
			if res.ForID != tc.call.ID {
				t.Errorf("ForID = %q, want %q", res.ForID, tc.call.ID)
			}
			if res.IsError != tc.wantErr {
				t.Errorf("IsError = %v, want %v (text=%q)", res.IsError, tc.wantErr, res.Text)
			}
			if tc.wantContain {
				if !strings.Contains(res.Text, tc.wantText) {
					t.Errorf("Text = %q, want substring %q", res.Text, tc.wantText)
				}
			} else if res.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", res.Text, tc.wantText)
			}
		})
	}
}

func TestDispatchGuardBlocksBeforeToolExecutionAndSurvivesSubset(t *testing.T) {
	ran := false
	r := &Registry{}
	r.Register(fakeTool{
		name: "reader", desc: "reader", schema: `{"type":"object"}`, readOnly: true,
		run: func(context.Context, json.RawMessage) (string, error) {
			ran = true
			return "unexpected", nil
		},
	})
	r.SetDispatchGuard(func(call llm.ToolCall, activity Activity) error {
		if call.Name != "reader" || string(call.Input) != "{}" || activity.Class != ActivityInspect {
			t.Fatalf("guard call/activity = %+v/%+v", call, activity)
		}
		return fmt.Errorf("work decision required")
	})
	sub, err := r.Subset([]string{"reader"})
	if err != nil {
		t.Fatal(err)
	}
	res := sub.Dispatch(context.Background(), llm.ToolCall{ID: "guarded", Name: "reader"})
	if !res.IsError || res.ErrorKind != llm.ToolErrorBlocked || !strings.Contains(res.Text, "work decision required") {
		t.Fatalf("guarded result = %+v", res)
	}
	if ran {
		t.Fatal("guarded tool executed")
	}
}

func TestSpecFilterHidesToolsWithoutChangingDispatch(t *testing.T) {
	r := &Registry{}
	reader := newOK("reader", "read")
	reader.readOnly = true
	r.Register(reader)
	r.Register(newOK("writer", "write"))
	r.SetSpecFilter(func(name string) bool { return name != "reader" })
	sub, err := r.Subset([]string{"reader", "writer"})
	if err != nil {
		t.Fatal(err)
	}
	if specs := sub.Specs(); len(specs) != 1 || specs[0].Name != "writer" {
		t.Fatalf("filtered specs = %+v", specs)
	}
	if result := sub.Dispatch(context.Background(), llm.ToolCall{ID: "hidden", Name: "reader"}); result.IsError {
		t.Fatalf("visibility filter changed dispatch: %+v", result)
	}
}

func TestDispatchFormatsJSONUnmarshalTypeErrors(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTool{
		name:   "typed",
		desc:   "typed input",
		schema: `{"type":"object"}`,
		run: func(_ context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Args           []string        `json:"args"`
				TimeoutSeconds int             `json:"timeout_seconds"`
				Options        map[string]bool `json:"options"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				// Wrapping must not hide the generic JSON classification.
				return "", fmt.Errorf("decode typed input: %w", err)
			}
			return "ok", nil
		},
	})

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "array field received string",
			input: `{"args":"-n TODO ."}`,
			want:  `invalid arguments: invalid value for "args": expected an array of strings; got string`,
		},
		{
			name:  "integer field received string",
			input: `{"timeout_seconds":"5"}`,
			want:  `invalid arguments: invalid value for "timeout_seconds": expected an integer; got string`,
		},
		{
			name:  "array element has wrong type",
			input: `{"args":["-n",7]}`,
			want:  `invalid arguments: invalid value for "args": expected a string; got number`,
		},
		{
			name:  "object field received array",
			input: `{"options":[]}`,
			want:  `invalid arguments: invalid value for "options": expected an object; got array`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "typed", Input: json.RawMessage(tt.input)})
			if !res.IsError {
				t.Fatalf("Dispatch result = %+v, want error", res)
			}
			if res.Text != tt.want {
				t.Fatalf("Text = %q, want %q", res.Text, tt.want)
			}
			if strings.Contains(res.Text, "Go struct field") || strings.Contains(res.Text, "[]string") {
				t.Fatalf("Text exposes Go decoder details: %q", res.Text)
			}
		})
	}
}

func TestDispatchEmptyInputTreatedAsObject(t *testing.T) {
	// Models sometimes omit the input entirely for zero-arg tools. Dispatch
	// must not reject an empty/nil Input as invalid JSON; the tool sees "{}".
	r := &Registry{}
	r.Register(fakeTool{
		name:   "z",
		desc:   "zero arg",
		schema: `{"type":"object"}`,
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			var v map[string]any
			if err := json.Unmarshal(input, &v); err != nil {
				return "", err
			}
			return "ok", nil
		},
	})
	for _, in := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("{}")} {
		res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "z", Input: in})
		if res.IsError {
			t.Errorf("input %q: unexpected error %q", in, res.Text)
		}
	}
}

func TestDispatchTruncateByBytes(t *testing.T) {
	big := strings.Repeat("x", 70*1024) // > 64KB, single line
	r := &Registry{}
	r.Register(newOK("big", big))

	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "big", Input: json.RawMessage(`{}`)})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if len(res.Text) > len(big) {
		t.Errorf("truncated output longer than input: %d > %d", len(res.Text), len(big))
	}
	if !strings.Contains(res.Text, "[truncated:") {
		t.Errorf("missing truncation marker: %q", res.Text[max(0, len(res.Text)-200):])
	}
	// Marker reports the original size in bytes/KB.
	if !strings.Contains(res.Text, "use read offset/limit or a targeted shell command to narrow") {
		t.Errorf("marker missing narrowing advice: %q", res.Text)
	}
}

func TestDispatchTruncateByLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 4213; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	r := &Registry{}
	r.Register(newOK("many", b.String()))

	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "many", Input: json.RawMessage(`{}`)})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[truncated: showing first 1000 of 4213 lines") {
		t.Errorf("missing line-truncation marker with counts: tail=%q", res.Text[max(0, len(res.Text)-200):])
	}
	if !strings.Contains(res.Text, "use read offset/limit or a targeted shell command to narrow") {
		t.Errorf("marker missing narrowing advice")
	}
	// Only the first 1000 lines should remain (plus the marker line).
	lines := strings.Split(strings.TrimRight(res.Text, "\n"), "\n")
	if len(lines) > 1002 {
		t.Errorf("expected ~1001 lines after truncation, got %d", len(lines))
	}
	if !strings.HasPrefix(res.Text, "line 0\n") {
		t.Errorf("first line not preserved: %q", res.Text[:20])
	}
}

// Regression: when output exceeds the line cap but each line is large, the
// byte cap must still hold. >1000 lines of 200 chars each is ~200KB; truncating
// only by lines would keep all of it and bust the 64KB backstop (review issue:
// truncate.go line-cap branch skips the byte cap).
func TestDispatchTruncateLinesStillRespectsBytes(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 1500; i++ {
		b.WriteString(strings.Repeat("x", 200))
		b.WriteByte('\n')
	}
	r := &Registry{}
	r.Register(newOK("fat", b.String()))

	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "fat", Input: json.RawMessage(`{}`)})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if len(res.Text) > defaultMaxResultBytes {
		t.Errorf("output %d bytes exceeds byte cap %d after line truncation", len(res.Text), defaultMaxResultBytes)
	}
	if !strings.Contains(res.Text, "[truncated:") {
		t.Errorf("missing truncation marker")
	}
}

func TestToolResultLimits(t *testing.T) {
	r, _ := DefaultWithOptions(Options{})
	readLimits := r.resultLimitsFor("read")
	if readLimits.maxBytes != defaultReadResultBytes || readLimits.maxLines != defaultMaxResultLines {
		t.Fatalf("read limits = %d/%d, want %d/%d", readLimits.maxBytes, readLimits.maxLines, defaultReadResultBytes, defaultMaxResultLines)
	}
	configured, _ := DefaultWithOptions(Options{MaxResultBytes: 1000, MaxResultLines: 200, ReadResultLines: 40})
	readLimits = configured.resultLimitsFor("read")
	if readLimits.maxBytes != 1000 || readLimits.maxLines != 40 {
		t.Fatalf("configured read limits = %d/%d, want 1000/40", readLimits.maxBytes, readLimits.maxLines)
	}
}

func expectedDefaultNames() []string {
	return []string{"read", "view_image", "edit", "write", "shell", "web_fetch"}
}

func TestDefaultAndCatalogNames(t *testing.T) {
	if got := DefaultNames(); !slices.Equal(got, expectedDefaultNames()) {
		t.Fatalf("DefaultNames() = %v, want %v", got, expectedDefaultNames())
	}
	wantCatalog := []string{"read", "view_image", "edit", "write", "shell"}
	if GitAvailable() {
		wantCatalog = append(wantCatalog, "git")
	}
	wantCatalog = append(wantCatalog, "web_fetch")
	if GitAvailable() {
		wantCatalog = append(wantCatalog, "git_readonly")
	}
	wantCatalog = append(wantCatalog, "write_tmp_file")
	if got := Catalog().Names(); !slices.Equal(got, wantCatalog) {
		t.Fatalf("Catalog().Names() = %v, want %v", got, wantCatalog)
	}
}

func TestCatalogRejectsRemovedToolNames(t *testing.T) {
	catalog := Catalog()
	for _, name := range []string{"read_file", "write_file", "apply_patch", "rg", "grep", "list_dir", "glob"} {
		if _, ok := catalog.Lookup(name); ok {
			t.Errorf("removed tool %q remains registered", name)
		}
		if _, err := catalog.Subset([]string{name}); err == nil {
			t.Errorf("Subset accepted removed tool %q", name)
		}
	}
}

func TestCatalogDiagnosticsForMissingGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r, disabled := CatalogWithDiagnostics()
	for _, name := range []string{"git", "git_readonly"} {
		if slices.Contains(r.Names(), name) || !disabledContains(disabled, name) {
			t.Fatalf("missing git diagnostic for %q: names=%v disabled=%+v", name, r.Names(), disabled)
		}
	}
}

func disabledContains(disabled []DisabledTool, name string) bool {
	for _, item := range disabled {
		if item.Name == name {
			return true
		}
	}
	return false
}

func TestSubsetFiltersSpecsAndDispatch(t *testing.T) {
	sub, err := Catalog().Subset([]string{"shell", "read"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sub.Names(); !slices.Equal(got, []string{"read", "shell"}) {
		t.Fatalf("Subset names = %v, want [read shell]", got)
	}
	res := sub.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "edit", Input: json.RawMessage(`{}`)})
	if !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("excluded tool dispatch = %+v", res)
	}
}

func TestSubsetOfDefaultNamesEqualsDefault(t *testing.T) {
	sub, err := Catalog().Subset(DefaultNames())
	if err != nil {
		t.Fatal(err)
	}
	if got := sub.Names(); !slices.Equal(got, Default().Names()) {
		t.Fatalf("Subset(DefaultNames()) = %v, want %v", got, Default().Names())
	}
}

func TestSubsetUnknownNameErrors(t *testing.T) {
	_, err := Catalog().Subset([]string{"read", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown subset error = %v", err)
	}
}

func TestDispatchNoTruncateWithinCaps(t *testing.T) {
	out := "small output\nwith two lines"
	r := &Registry{}
	r.Register(newOK("small", out))
	res := r.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "small", Input: json.RawMessage(`{}`)})
	if res.Text != out {
		t.Errorf("output mutated: %q", res.Text)
	}
}

func TestRegisterAfter(t *testing.T) {
	r := &Registry{}
	r.Register(newOK("read", ""))
	r.Register(newOK("view_image", ""))
	r.Register(newOK("edit", ""))

	// Insert immediately after the anchor.
	r.RegisterAfter(newOK("lsp_definition", ""), "view_image")
	r.RegisterAfter(newOK("lsp_hover", ""), "lsp_definition")
	want := []string{"read", "view_image", "lsp_definition", "lsp_hover", "edit"}
	if got := r.Names(); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	// Missing anchor appends.
	r.RegisterAfter(newOK("tail_tool", ""), "no_such_anchor")
	if got := r.Names(); got[len(got)-1] != "tail_tool" {
		t.Fatalf("missing anchor should append: %v", got)
	}

	// Re-registration keeps the existing position.
	r.RegisterAfter(newOK("lsp_definition", ""), "read")
	if got := r.Names(); !slices.Equal(got, append(want, "tail_tool")) {
		t.Fatalf("re-registration moved tool: %v", got)
	}
}
