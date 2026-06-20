package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"harness/internal/llm"
)

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

type meteredFakeTool struct {
	fakeTool
	result MeteredResult
	err    error
}

func (m meteredFakeTool) RunMetered(context.Context, json.RawMessage) (MeteredResult, error) {
	return m.result, m.err
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
			wantText:    `error: unknown tool "nope"`,
			wantErr:     true,
			wantContain: true,
		},
		{
			name:        "invalid json args",
			call:        llm.ToolCall{ID: "3", Name: "needsarg", Input: json.RawMessage(`{not json`)},
			wantText:    "error: invalid arguments:",
			wantErr:     true,
			wantContain: true,
		},
		{
			name:        "tool returns error",
			call:        llm.ToolCall{ID: "4", Name: "err", Input: json.RawMessage(`{}`)},
			wantText:    "error: something broke",
			wantErr:     true,
			wantContain: true,
		},
		{
			name:        "tool panics",
			call:        llm.ToolCall{ID: "5", Name: "boom", Input: json.RawMessage(`{}`)},
			wantText:    "error: tool panicked: kaboom",
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
	if !strings.Contains(res.Text, "use read_file offset/limit or grep to narrow") {
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
	if !strings.Contains(res.Text, "use read_file offset/limit or grep to narrow") {
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

func TestDefaultNamesMatchDefaultRegistry(t *testing.T) {
	want := expectedDefaultNames()
	if got := DefaultNames(); !slices.Equal(got, want) {
		t.Errorf("DefaultNames() = %v, want %v", got, want)
	}
	if got := Default().Names(); !slices.Equal(got, DefaultNames()) {
		t.Errorf("Default().Names() = %v, want DefaultNames() %v", got, DefaultNames())
	}
}

func expectedDefaultNames() []string {
	if RipgrepAvailable() {
		return expectedDefaultNamesForSearch(SearchToolsRG)
	}
	return expectedDefaultNamesForSearch(SearchToolsGrep)
}

func expectedDefaultNamesForSearch(mode string) []string {
	want := []string{"read_file", "list_dir", "glob"}
	switch mode {
	case SearchToolsBoth:
		want = append(want, "grep")
		if RipgrepAvailable() {
			want = append(want, "rg")
		}
	case SearchToolsRG:
		if RipgrepAvailable() {
			want = append(want, "rg")
		} else {
			want = append(want, "grep")
		}
	case SearchToolsGrep:
		want = append(want, "grep")
	default:
		if RipgrepAvailable() {
			want = append(want, "rg")
		} else {
			want = append(want, "grep")
		}
	}
	// apply_patch is not in the default set; it ships only in the Catalog (r56).
	want = append(want, "edit", "write_file", "run_command")
	if GitAvailable() {
		want = append(want, "git")
	}
	return append(want, "web_fetch")
}

func TestDefaultNamesWithSearchToolOptions(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode string
		want []string
	}{
		{name: "auto", mode: SearchToolsAuto, want: expectedDefaultNames()},
		{name: "grep", mode: SearchToolsGrep, want: expectedDefaultNamesForSearch(SearchToolsGrep)},
		{name: "rg", mode: SearchToolsRG, want: expectedDefaultNamesForSearch(SearchToolsRG)},
		{name: "both", mode: SearchToolsBoth, want: expectedDefaultNamesForSearch(SearchToolsBoth)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := DefaultNamesWithOptions(Options{SearchTools: tt.mode})
			if !slices.Equal(got, tt.want) {
				t.Fatalf("DefaultNamesWithOptions(%q) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

func TestCatalogRegistersDefaultPlusModeTools(t *testing.T) {
	r := Catalog()
	want := append([]string{}, DefaultNames()...)
	want = append(want, "apply_patch") // relocated out of the default set (r56)
	if GitAvailable() {
		want = append(want, "git_readonly")
	}
	want = append(want, "write_tmp_file")
	if got := r.Names(); !slices.Equal(got, want) {
		t.Errorf("Catalog().Names() = %v, want %v", got, want)
	}
	for _, s := range r.Specs() {
		if len(s.Parameters) == 0 {
			t.Errorf("tool %q has empty schema", s.Name)
		}
	}
}

// r56: apply_patch ships only in the Catalog, not the default request, but stays
// whitelist-constructible via Subset.
func TestApplyPatchRelocatedToCatalog(t *testing.T) {
	if slices.Contains(DefaultNames(), "apply_patch") {
		t.Errorf("apply_patch should not be in the default set: %v", DefaultNames())
	}
	if !slices.Contains(Catalog().Names(), "apply_patch") {
		t.Errorf("apply_patch should be in the catalog: %v", Catalog().Names())
	}
	sub, err := Catalog().Subset([]string{"apply_patch"})
	if err != nil {
		t.Fatalf("Subset([apply_patch]): %v", err)
	}
	if !slices.Contains(sub.Names(), "apply_patch") {
		t.Errorf("apply_patch should be selectable via Subset: %v", sub.Names())
	}
}

func TestCatalogDiagnosticsForMissingCLITools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	r, disabled := CatalogWithDiagnostics()
	for _, name := range []string{"git", "git_readonly"} {
		if slices.Contains(r.Names(), name) {
			t.Fatalf("CatalogWithDiagnostics registered %q without its binary; names=%v", name, r.Names())
		}
		if !disabledContains(disabled, name) {
			t.Fatalf("disabled diagnostics missing %q: %+v", name, disabled)
		}
	}
	if disabledContains(disabled, "rg") {
		t.Fatalf("auto search mode should fall back to grep without a disabled rg diagnostic: %+v", disabled)
	}
}

func TestCatalogDiagnosticsForExplicitMissingRipgrep(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	r, disabled := CatalogWithOptions(Options{SearchTools: SearchToolsBoth})
	if slices.Contains(r.Names(), "rg") {
		t.Fatalf("CatalogWithOptions registered rg without its binary; names=%v", r.Names())
	}
	if !disabledContains(disabled, "rg") {
		t.Fatalf("disabled diagnostics missing rg: %+v", disabled)
	}
}

func TestDefaultNamesOmitMissingGitTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	for _, name := range []string{"git", "git_readonly"} {
		if slices.Contains(DefaultNames(), name) {
			t.Fatalf("DefaultNames() includes unavailable %q: %v", name, DefaultNames())
		}
		if slices.Contains(Catalog().Names(), name) {
			t.Fatalf("Catalog() includes unavailable %q: %v", name, Catalog().Names())
		}
	}
}

func disabledContains(disabled []DisabledTool, name string) bool {
	for _, d := range disabled {
		if d.Name == name {
			return true
		}
	}
	return false
}

// Subset gating must be airtight: an excluded tool is neither advertised in
// Specs nor dispatchable — both read the same filtered registry.
func TestSubsetFiltersSpecsAndDispatch(t *testing.T) {
	sub, err := CatalogWithOptionsMust(t, Options{SearchTools: SearchToolsBoth}).Subset([]string{"grep", "read_file"}) // deliberately out of order
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	// Catalog order is preserved regardless of the requested order.
	if got := sub.Names(); !slices.Equal(got, []string{"read_file", "grep"}) {
		t.Errorf("Subset names = %v, want [read_file grep]", got)
	}
	for _, s := range sub.Specs() {
		if s.Name == "edit" {
			t.Error("excluded tool advertised in Specs")
		}
	}
	res := sub.Dispatch(context.Background(), llm.ToolCall{ID: "1", Name: "edit", Input: json.RawMessage(`{}`)})
	if !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Errorf("excluded tool should be undispatchable, got %+v", res)
	}
}

func CatalogWithOptionsMust(t *testing.T, opts Options) *Registry {
	t.Helper()
	r, _ := CatalogWithOptions(opts)
	return r
}

func TestSubsetOfDefaultNamesEqualsDefault(t *testing.T) {
	sub, err := Catalog().Subset(DefaultNames())
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	if got := sub.Names(); !slices.Equal(got, Default().Names()) {
		t.Errorf("Subset(DefaultNames()) = %v, want %v", got, Default().Names())
	}
}

func TestSubsetUnknownNameErrors(t *testing.T) {
	_, err := Catalog().Subset([]string{"read_file", "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown tool name")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the unknown tool: %v", err)
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
