package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if !strings.HasPrefix(out, "Available skills:\n") {
		t.Errorf("missing header: %q", out)
	}
	// Sorted: a before b.
	aIdx := strings.Index(out, "- a: desc a (read /path/a/SKILL.md)")
	bIdx := strings.Index(out, "- b: desc b (read /path/b/SKILL.md)")
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
	if !strings.Contains(out, "- s: first line second line (read /p/q.r)") {
		t.Errorf("description should be collapsed to one line: %q", out)
	}
}

func TestInstructionsEmpty(t *testing.T) {
	if out := Instructions(0); out != "" {
		t.Errorf("expected empty instructions for 0 skills, got %q", out)
	}
	if out := Instructions(3); out == "" {
		t.Errorf("expected instructions for 3 skills, got empty")
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
