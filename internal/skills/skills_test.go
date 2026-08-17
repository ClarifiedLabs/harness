package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFrontmatterBasic(t *testing.T) {
	in := `---
name: pdf-processing
description: Extract PDF text, fill forms, merge files.
---
# PDF Processing Body
`
	fm, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fm["name"] != "pdf-processing" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "Extract PDF text, fill forms, merge files." {
		t.Errorf("description = %q", fm["description"])
	}
}

func TestParseFrontmatterQuotedValues(t *testing.T) {
	in := `---
name: "data-analysis"
description: 'Analyze datasets, generate charts, and create summary reports.'
---
body
`
	fm, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fm["name"] != "data-analysis" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "Analyze datasets, generate charts, and create summary reports." {
		t.Errorf("description = %q", fm["description"])
	}
}

func TestParseFrontmatterBlockScalar(t *testing.T) {
	in := `---
name: code-review
description: |
  Review code changes for correctness, style, and performance.
  Provides actionable feedback.
---
body
`
	fm, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := "Review code changes for correctness, style, and performance.\nProvides actionable feedback."
	if fm["description"] != want {
		t.Errorf("description = %q, want %q", fm["description"], want)
	}
}

func TestParseFrontmatterFoldedScalar(t *testing.T) {
	in := `---
name: x
description: >
  First line
  second line
  third line
---
`
	fm, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := "First line second line third line"
	if fm["description"] != want {
		t.Errorf("description = %q, want %q", fm["description"], want)
	}
}

func TestParseFrontmatterNoOpening(t *testing.T) {
	in := `no frontmatter here`
	_, err := parseFrontmatter(in)
	if err == nil {
		t.Fatal("want error when no frontmatter")
	}
}

func TestParseFrontmatterUnterminated(t *testing.T) {
	in := `---
name: broken
description: never closed
`
	_, err := parseFrontmatter(in)
	if err == nil {
		t.Fatal("want error when unterminated")
	}
}

func TestParseFrontmatterInlineComment(t *testing.T) {
	in := `---
name: foo
description: my skill # this is a comment
---
`
	fm, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fm["description"] != "my skill" {
		t.Errorf("description = %q (want comment stripped)", fm["description"])
	}
}

func TestParseFrontmatterColonInValue(t *testing.T) {
	// The unquoted colon-in-value case from the spec: lenient parsers accept it.
	in := `---
name: x
description: Use this skill when: the user asks about PDFs
---
`
	fm, err := parseFrontmatter(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// parseFrontmatter splits on the first colon only, so the rest stays.
	if !strings.Contains(fm["description"], "when: the user asks about PDFs") {
		t.Errorf("description should keep inline colons after the first: %q", fm["description"])
	}
}

func TestDiscoverFindsSkills(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pdf-processing", "SKILL.md"), `---
name: pdf-processing
description: Handle PDFs.
---
body`)
	writeFile(t, filepath.Join(root, "data-analysis", "SKILL.md"), `---
name: data-analysis
description: Analyze data.
---
body`)
	// Not a skill (no SKILL.md).
	if err := os.MkdirAll(filepath.Join(root, "just-a-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// README directly in root: ignored.
	writeFile(t, filepath.Join(root, "README.md"), "# skills")

	var w Warnings
	got := Discover([]Dir{{Path: root, Scope: ScopeUser}}, &w)
	if len(got) != 2 {
		t.Fatalf("want 2 skills, got %d: %v (warnings: %v)", len(got), got, w)
	}
	if _, ok := got["pdf-processing"]; !ok {
		t.Errorf("pdf-processing missing")
	}
	if _, ok := got["data-analysis"]; !ok {
		t.Errorf("data-analysis missing")
	}
}

func TestDiscoverMissingDescriptionSkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "orphan", "SKILL.md"), `---
name: orphan
---
body only`)

	var w Warnings
	got := Discover([]Dir{{Path: root, Scope: ScopeUser}}, &w)
	if len(got) != 0 {
		t.Errorf("skill with no description should be skipped, got %v", got)
	}
	found := false
	for _, msg := range w {
		if strings.Contains(msg, "no description") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected no-description warning, got: %v", w)
	}
}

func TestDiscoverNameMismatchWarns(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "actual-dir", "SKILL.md"), `---
name: different-name
description: x
---
body`)
	var w Warnings
	got := Discover([]Dir{{Path: root, Scope: ScopeUser}}, &w)
	if len(got) != 1 {
		t.Fatalf("want 1 skill, got %d", len(got))
	}
	if _, ok := got["different-name"]; !ok {
		t.Errorf("skill should be keyed by frontmatter name, got: %v", got)
	}
	found := false
	for _, msg := range w {
		if strings.Contains(msg, "does not match directory") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected name-mismatch warning, got: %v", w)
	}
}

func TestDiscoverNameMissingFallsBackToDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "my-tool", "SKILL.md"), `---
description: a tool
---
body`)
	var w Warnings
	got := Discover([]Dir{{Path: root, Scope: ScopeUser}}, &w)
	if s, ok := got["my-tool"]; !ok {
		t.Fatalf("want skill keyed by directory name, got: %v", got)
	} else if s.Name != "my-tool" {
		t.Errorf("Name = %q, want my-tool", s.Name)
	}
}

func TestDiscoverProjectOverridesUser(t *testing.T) {
	userDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(userDir, "review", "SKILL.md"), `---
name: review
description: from user
---
user body`)
	writeFile(t, filepath.Join(projDir, "review", "SKILL.md"), `---
name: review
description: from project
---
project body`)

	var w Warnings
	got := Discover([]Dir{
		{Path: userDir, Scope: ScopeUser},
		{Path: projDir, Scope: ScopeProject},
	}, &w)
	if len(got) != 1 {
		t.Fatalf("want 1 skill, got %d", len(got))
	}
	if got["review"].Description != "from project" {
		t.Errorf("project skill should override user, got %q", got["review"].Description)
	}
	// A warning should log the shadow.
	found := false
	for _, msg := range w {
		if strings.Contains(msg, "shadows") || strings.Contains(msg, "shadowed") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected collision warning, got: %v", w)
	}
}

func TestDiscoverLongNameWarnsStillLoads(t *testing.T) {
	root := t.TempDir()
	longName := strings.Repeat("a", nameMaxLen+10)
	writeFile(t, filepath.Join(root, longName, "SKILL.md"), `---
name: `+longName+`
description: long
---
body`)
	var w Warnings
	got := Discover([]Dir{{Path: root, Scope: ScopeUser}}, &w)
	if len(got) != 1 {
		t.Fatalf("want 1 skill loaded, got %d", len(got))
	}
	found := false
	for _, msg := range w {
		if strings.Contains(msg, "exceeds") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected length warning, got: %v", w)
	}
}

func TestDiscoverMissingDirIgnored(t *testing.T) {
	var w Warnings
	// Non-existent paths are silently skipped.
	got := Discover([]Dir{{Path: "/non/existent/path/for/sure", Scope: ScopeUser}}, &w)
	if len(got) != 0 {
		t.Errorf("want 0 skills, got %v", got)
	}
}

func TestBuildCatalogEmpty(t *testing.T) {
	if out := BuildCatalog(nil); out != "" {
		t.Errorf("empty catalog for empty input, got %q", out)
	}
	if out := BuildCatalog(map[string]Skill{}); out != "" {
		t.Errorf("empty catalog for empty map, got %q", out)
	}
}

func TestBuildCatalogShape(t *testing.T) {
	m := map[string]Skill{
		"b": {Name: "b", Description: "desc b", Location: "/path/b/SKILL.md"},
		"a": {Name: "a", Description: "desc a", Location: "/path/a/SKILL.md"},
	}
	out := BuildCatalog(m)
	if !strings.Contains(out, "## Skills") {
		t.Errorf("missing ## Skills header: %q", out)
	}
	if !strings.Contains(out, "### Available skills") {
		t.Errorf("missing Available skills heading: %q", out)
	}
	if !strings.Contains(out, "### How to use skills") {
		t.Errorf("missing How to use heading: %q", out)
	}
	// Sorted: a before b.
	aIdx := strings.Index(out, "- a: desc a (file: /path/a/SKILL.md)")
	bIdx := strings.Index(out, "- b: desc b (file: /path/b/SKILL.md)")
	if aIdx < 0 || bIdx < 0 {
		t.Fatalf("both skills missing: %q", out)
	}
	if aIdx > bIdx {
		t.Errorf("want 'a' before 'b' for stable output: aIdx=%d bIdx=%d", aIdx, bIdx)
	}
	if strings.Contains(out, "<") || strings.Contains(out, ">") {
		t.Errorf("catalog should not use XML tags: %q", out)
	}
}

func TestBuildCatalogOneLinesDescriptions(t *testing.T) {
	m := map[string]Skill{
		"s": {Name: "s", Description: "first line\nsecond   line", Location: "/p/q.r"},
	}
	out := BuildCatalog(m)
	if !strings.Contains(out, "- s: first line second line (file: /p/q.r)") {
		t.Errorf("description should be collapsed to one line: %q", out)
	}
}

func TestBuildCatalogTruncatesToFirstSentence(t *testing.T) {
	m := map[string]Skill{
		"s": {Name: "s", Description: "Do the thing. Then a long tail of extra detail that should not ride in the always-resident prompt.", Location: "/p/q.r"},
	}
	out := BuildCatalog(m)
	if !strings.Contains(out, "- s: Do the thing. (file: /p/q.r)") {
		t.Errorf("catalog should keep only the first sentence: %q", out)
	}
	if strings.Contains(out, "long tail") {
		t.Errorf("catalog should drop text after the first sentence: %q", out)
	}
}

func TestBuildCatalogHardCapAddsEllipsis(t *testing.T) {
	long := strings.Repeat("x", 250) // no sentence boundary
	m := map[string]Skill{
		"s": {Name: "s", Description: long, Location: "/p/q.r"},
	}
	out := BuildCatalog(m)
	if !strings.Contains(out, "…") {
		t.Errorf("a hard-capped description should end with an ellipsis: %q", out)
	}
	// The rendered description must be capped near the limit, not the full 250.
	if strings.Contains(out, strings.Repeat("x", catalogDescMaxChars+1)) {
		t.Errorf("description should be truncated to the char cap: %q", out)
	}
}

func TestCatalogDescription(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short single sentence kept whole", "Does X.", "Does X."},
		{"no punctuation short kept whole", "does x", "does x"},
		{"whitespace collapsed", "first\nsecond   third", "first second third"},
		{"first sentence with tail dropped", "Lead in. More here.", "Lead in."},
		{"question boundary", "Use when? Yes really.", "Use when?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := catalogDescription(tc.in); got != tc.want {
				t.Errorf("catalogDescription(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// An over-cap first sentence falls back to the rune cap plus ellipsis.
	long := strings.Repeat("a", 205) + "."
	got := catalogDescription(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("over-long first sentence should be capped with ellipsis: %q", got)
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "…")); n != catalogDescMaxChars {
		t.Errorf("capped body = %d runes, want %d", n, catalogDescMaxChars)
	}
}

func TestSkillRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	writeFile(t, path, `---
name: x
description: x
---
hello world`)
	s := Skill{Name: "x", Location: path}
	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "---\nname: x\ndescription: x\n---\nhello world" {
		t.Errorf("Read content = %q", got)
	}
}

func TestParseActiveContextRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		inName   string
		location string
		body     string
	}{
		{name: "normal body", inName: "pdf", location: "/skills/pdf/SKILL.md", body: "first\nsecond\n"},
		{name: "body without trailing newline", inName: "pdf", location: "/skills/pdf/SKILL.md", body: "only"},
		{name: "empty name", inName: "", location: "/skills/pdf/SKILL.md", body: "body\n"},
		{name: "empty location", inName: "pdf", location: "", body: "body\n"},
		{name: "body containing end marker", inName: "pdf", location: "/p/SKILL.md", body: "quoted [end active skill instructions] inside\n"},
		{name: "empty body", inName: "pdf", location: "/p/SKILL.md", body: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := ActiveContext(tc.inName, tc.location, tc.body)
			name, location, body, ok := ParseActiveContext(item)
			if !ok {
				t.Fatalf("ParseActiveContext rejected a rendered block:\n%s", item)
			}
			if name != tc.inName || location != tc.location {
				t.Fatalf("header = name %q location %q, want %q %q", name, location, tc.inName, tc.location)
			}
			wantBody := strings.TrimSuffix(tc.body, "\n")
			if body != wantBody {
				t.Fatalf("body = %q, want %q", body, wantBody)
			}
			if rewrapped := ActiveContext(name, location, body); rewrapped != item {
				t.Fatalf("re-wrapped block differs:\n got: %q\nwant: %q", rewrapped, item)
			}
		})
	}
}

func TestParseActiveContextRejectsForeignText(t *testing.T) {
	tcases := []struct {
		name string
		in   string
	}{
		{name: "plain text", in: "unrelated context"},
		{name: "marker without prompt line", in: "[active skill instructions]\nname: x\n[end active skill instructions]"},
		{name: "truncated block", in: "[active skill instructions]\nname: x\nsource: /p/SKILL.md\nThe following full SKILL.md is authoritative for this prompt:\n\nbody without end"},
		{name: "trailing text after marker", in: "[active skill instructions]\nThe following full SKILL.md is authoritative for this prompt:\n\nbody\n[end active skill instructions]\ntrailing"},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, ok := ParseActiveContext(tc.in); ok {
				t.Fatalf("ParseActiveContext accepted %q", tc.in)
			}
		})
	}
}

func TestDiscoverNestedSkillFound(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "parent", "child", "my-skill", "SKILL.md"), `---
name: my-skill
description: nested
---
body`)
	var w Warnings
	got := Discover([]Dir{{Path: root, Scope: ScopeUser}}, &w)
	if _, ok := got["my-skill"]; !ok {
		t.Errorf("nested skill not discovered: %v", got)
	}
}

func TestAncestorSkillDirsFindsNearestShadow(t *testing.T) {
	tmp := t.TempDir()
	outer := filepath.Join(tmp, "a")
	inner := filepath.Join(outer, "b")
	// .git at outer to define project root.
	if err := os.MkdirAll(filepath.Join(outer, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outer, ".agents", "skills", "dup", "SKILL.md"), `---
name: dup
description: outer
---
outer`)
	writeFile(t, filepath.Join(inner, ".agents", "skills", "dup", "SKILL.md"), `---
name: dup
description: inner
---
inner`)
	writeFile(t, filepath.Join(outer, ".agents", "skills", "outer-only", "SKILL.md"), `---
name: outer-only
description: outer bar
---
bar`)
	dirs := AncestorSkillDirs(inner, t.TempDir())
	var w Warnings
	got := Discover(dirs, &w)
	if got["dup"].Description != "inner" {
		t.Errorf("nearest should shadow outer, got %q", got["dup"].Description)
	}
	if _, ok := got["outer-only"]; !ok {
		t.Errorf("outer ancestor skill not found")
	}
}

func TestAncestorSkillDirsDedupesAndCoversAncestors(t *testing.T) {
	tmp := t.TempDir()
	outer := filepath.Join(tmp, "proj")
	inner := filepath.Join(outer, "pkg")
	if err := os.MkdirAll(filepath.Join(outer, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outer, ".agents", "skills", "a", "SKILL.md"), `---
name: a
description: a
---
a`)
	writeFile(t, filepath.Join(inner, ".agents", "skills", "b", "SKILL.md"), `---
name: b
description: b
---
b`)
	dirs := AncestorSkillDirs(inner, "")
	if len(dirs) < 2 {
		t.Fatalf("dirs = %v, want at least 2", dirs)
	}
	seen := make(map[string]bool)
	for _, d := range dirs {
		if seen[d.Path] {
			t.Errorf("duplicate dir %q", d.Path)
		}
		seen[d.Path] = true
	}
	var w Warnings
	got := Discover(dirs, &w)
	if _, ok := got["a"]; !ok {
		t.Errorf("ancestor skill a missing")
	}
	if _, ok := got["b"]; !ok {
		t.Errorf("inner skill b missing")
	}
}

func TestCatalogBudgetUsesContextWindowPercentage(t *testing.T) {
	tests := []struct {
		name          string
		contextWindow int
		want          int
	}{
		{name: "unknown falls back", contextWindow: 0, want: 8000},
		{name: "negative falls back", contextWindow: -1, want: 8000},
		{name: "400k window", contextWindow: 400_000, want: 8000},
		{name: "one million window", contextWindow: 1_000_000, want: 20_000},
		{name: "small positive window", contextWindow: 49, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CatalogBudget(tc.contextWindow); got != tc.want {
				t.Fatalf("CatalogBudget(%d) = %d, want %d", tc.contextWindow, got, tc.want)
			}
		})
	}
}

func TestBuildCatalogBudgetTruncatesRoundRobin(t *testing.T) {
	m := make(map[string]Skill)
	for i := 0; i < 5; i++ {
		name := "s" + string(rune('a'+i))
		m[name] = Skill{Name: name, Description: strings.Repeat("y", 100), Location: "/p/" + name + "/SKILL.md"}
	}
	out, rep := BuildCatalogBudgeted(m, 200)
	if rep.Total != 5 || rep.Included != 5 {
		t.Errorf("report = %+v", rep)
	}
	if rep.TruncatedCount == 0 {
		t.Errorf("want truncation under tight budget, rep=%+v out=%q", rep, out)
	}
	if !strings.Contains(out, "## Skills") || !strings.Contains(out, "### Available skills") {
		t.Errorf("missing headings: %q", out)
	}
	if strings.Contains(out, "more skills not shown") {
		t.Errorf("should not omit when sumMin fits: %q", out)
	}
}

func TestBuildCatalogBudgetOmission(t *testing.T) {
	m := make(map[string]Skill)
	for i := 0; i < 10; i++ {
		name := "s" + string(rune('a'+i))
		m[name] = Skill{Name: name, Description: "desc", Location: "/p/" + name + "/SKILL.md"}
	}
	out, rep := BuildCatalogBudgeted(m, 10)
	if rep.Omitted == 0 {
		t.Fatalf("want omission, rep=%+v", rep)
	}
	if !strings.Contains(out, "more skills not shown") {
		t.Errorf("missing omission notice: %q", out)
	}
	if rep.Included+rep.Omitted != rep.Total {
		t.Errorf("included+omitted != total: %+v", rep)
	}
}

func TestBuildCatalogBudgetOmissionCharsBounded(t *testing.T) {
	m := make(map[string]Skill)
	for i := 0; i < 3; i++ {
		name := "s" + string(rune('a'+i))
		m[name] = Skill{Name: name, Description: strings.Repeat("z", 200), Location: "/p/" + name + "/SKILL.md"}
	}
	budget := 50
	out, rep := BuildCatalogBudgeted(m, budget)
	_ = rep
	// Available-skills section lines should respect budget: count rune length of "- name" lines only.
	// Extract lines between headings.
	lines := strings.Split(out, "\n")
	sum := 0
	inAvailable := false
	for _, line := range lines {
		if strings.HasPrefix(line, "### Available skills") {
			inAvailable = true
			continue
		}
		if strings.HasPrefix(line, "### How to use") {
			break
		}
		if inAvailable && strings.HasPrefix(line, "- ") {
			sum += utf8.RuneCountInString(line + "\n")
		}
	}
	if sum > budget {
		t.Errorf("available skill lines exceed budget: sum=%d budget=%d report=%+v out=%q", sum, budget, rep, out)
	}
}
