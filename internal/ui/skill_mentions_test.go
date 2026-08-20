package ui

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/skills"
)

func TestResolveSkillMentionsSelectsKnownSkillAnywhere(t *testing.T) {
	commit := testSkill(t, "commit", "Create a git commit", "# Commit skill\nCOMMIT BODY")
	res := resolveSkillMentions("please use $commit for this", map[string]skills.Skill{
		"commit": commit,
	})
	if res.Unknown != "" {
		t.Fatalf("Unknown = %q, want none", res.Unknown)
	}
	if res.Injected != 1 {
		t.Fatalf("Injected = %d, want 1", res.Injected)
	}
	for _, want := range []string{
		"<skill>",
		"<name>commit</name>",
		"<path>" + commit.Location + "</path>",
		"# Commit skill",
		"COMMIT BODY",
		"</skill>\n\nplease use $commit for this",
	} {
		if !strings.Contains(res.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, res.Prompt)
		}
	}
	if strings.Contains(res.Prompt, "read the full SKILL.md") {
		t.Fatalf("explicit injection should not request a model tool round:\n%s", res.Prompt)
	}
}

func TestResolveSkillMentionsPreservesFirstMentionOrderAndDedupes(t *testing.T) {
	alpha := testSkill(t, "alpha", "Alpha", "ALPHA BODY")
	beta := testSkill(t, "beta", "Beta", "BETA BODY")
	res := resolveSkillMentions("$beta then $alpha then $beta", map[string]skills.Skill{
		"alpha": alpha,
		"beta":  beta,
	})
	if res.Injected != 2 {
		t.Fatalf("Injected = %d, want 2", res.Injected)
	}
	betaIndex := strings.Index(res.Prompt, "<name>beta</name>")
	alphaIndex := strings.Index(res.Prompt, "<name>alpha</name>")
	if betaIndex < 0 || alphaIndex < 0 || betaIndex > alphaIndex {
		t.Fatalf("skills should appear in first-mention order and once:\n%s", res.Prompt)
	}
	if strings.Count(res.Prompt, "<name>beta</name>") != 1 {
		t.Fatalf("beta should be deduped:\n%s", res.Prompt)
	}
	if !strings.HasSuffix(res.Prompt, "\n\n$beta then $alpha then $beta") {
		t.Fatalf("user text should follow injected blocks:\n%s", res.Prompt)
	}
}

func TestResolveSkillMentionsEscapedDollarIsLiteral(t *testing.T) {
	res := resolveSkillMentions("please use $$commit", map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	})
	if res.Unknown != "" || res.Injected != 0 {
		t.Fatalf("escaped dollar should not resolve a skill: %+v", res)
	}
	if res.Prompt != "please use $commit" {
		t.Fatalf("Prompt = %q, want %q", res.Prompt, "please use $commit")
	}
}

func TestResolveSkillMentionsSupportsColonNames(t *testing.T) {
	swift := testSkill(t, "build-ios-apps:swiftui-patterns", "Build SwiftUI screens", "SWIFTUI BODY")
	res := resolveSkillMentions("please use $build-ios-apps:swiftui-patterns", map[string]skills.Skill{
		"build-ios-apps:swiftui-patterns": swift,
	})
	if res.Unknown != "" {
		t.Fatalf("Unknown = %q, want none", res.Unknown)
	}
	if res.Injected != 1 {
		t.Fatalf("Injected = %d, want 1", res.Injected)
	}
	if !strings.Contains(res.Prompt, "<name>build-ios-apps:swiftui-patterns</name>") ||
		!strings.Contains(res.Prompt, "SWIFTUI BODY") {
		t.Fatalf("prompt missing colon skill:\n%s", res.Prompt)
	}
}

func TestResolveSkillMentionsMissingSkillWarnsAndInjectsOthers(t *testing.T) {
	commit := testSkill(t, "commit", "Create a git commit", "COMMIT BODY")
	missing := skills.Skill{Name: "plans", Description: "Plan work", Location: filepath.Join(t.TempDir(), "gone", "SKILL.md")}
	res := resolveSkillMentions("use $commit and $plans", map[string]skills.Skill{
		"commit": commit,
		"plans":  missing,
	})
	if res.Unknown != "" {
		t.Fatalf("Unknown = %q, want none", res.Unknown)
	}
	if res.Injected != 1 {
		t.Fatalf("Injected = %d, want 1 (readable block only)", res.Injected)
	}
	if got := strings.Count(res.Prompt, "<skill>"); got != 1 {
		t.Fatalf("prompt contains %d <skill> blocks, want 1:\n%s", got, res.Prompt)
	}
	for _, want := range []string{"<name>commit</name>", "COMMIT BODY", "\n\nuse $commit and $plans"} {
		if !strings.Contains(res.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, res.Prompt)
		}
	}
	if strings.Contains(res.Prompt, "<name>plans</name>") {
		t.Fatalf("unreadable skill should not be injected:\n%s", res.Prompt)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], `read skill "plans" at `+missing.Location) {
		t.Fatalf("Warnings = %v, want unreadable plans skill warning", res.Warnings)
	}
}

func TestResolveSkillMentionsAllMissingSkillsRunRawPrompt(t *testing.T) {
	missing := skills.Skill{Name: "commit", Description: "Create a git commit", Location: filepath.Join(t.TempDir(), "missing", "SKILL.md")}
	res := resolveSkillMentions("please use $commit", map[string]skills.Skill{
		"commit": missing,
	})
	if res.Unknown != "" {
		t.Fatalf("Unknown = %q, want none", res.Unknown)
	}
	if res.Injected != 0 {
		t.Fatalf("Injected = %d, want 0", res.Injected)
	}
	if res.Prompt != "please use $commit" {
		t.Fatalf("Prompt = %q, want the raw prompt without a prefix", res.Prompt)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], `read skill "commit"`) {
		t.Fatalf("Warnings = %v, want unreadable commit skill warning", res.Warnings)
	}
}

func TestResolveSkillMentionsUnknownInlineDollarIsLiteral(t *testing.T) {
	res := resolveSkillMentions("print $PATH and $missing", map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	})
	if res.Unknown != "" || res.Injected != 0 {
		t.Fatalf("unknown inline dollars should stay literal: %+v", res)
	}
}

func TestResolveSkillMentionsStandaloneUnknownSkill(t *testing.T) {
	res := resolveSkillMentions("$missing", map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	})
	if res.Unknown != "missing" {
		t.Fatalf("Unknown = %q, want missing", res.Unknown)
	}
	if res.Injected != 0 {
		t.Fatalf("Injected = %d, want 0", res.Injected)
	}
}

func TestResolveSkillMentionContextWarnsAndContinuesOnMissingSkill(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("fake"))
	missing := filepath.Join(t.TempDir(), "gone", "SKILL.md")
	app.Skills = map[string]skills.Skill{
		"plans": {Name: "plans", Description: "Plan work", Location: missing},
	}

	prompt, injected, ok := app.resolveSkillMentionContext("$plans hello")
	if !ok {
		t.Fatal("missing skill should not block the prompt")
	}
	if injected != 0 {
		t.Fatalf("Injected = %d, want 0", injected)
	}
	if prompt != "$plans hello" {
		t.Fatalf("prompt = %q, want the raw prompt without a prefix", prompt)
	}
	if want := "warning: skill not injected: read skill \"plans\" at " + missing; !strings.Contains(errw.String(), want) {
		t.Fatalf("missing warning %q in errw=%q", want, errw.String())
	}
}

func TestResolveSkillMentionContextStillBlocksUnknownSkill(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("fake"))
	app.Skills = map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	}

	if _, _, ok := app.resolveSkillMentionContext("$typo"); ok {
		t.Fatal("unknown skill should still block the prompt")
	}
	if !strings.Contains(errw.String(), `unknown skill "typo"`) {
		t.Fatalf("missing unknown-skill notice, errw=%q", errw.String())
	}
}

func TestREPLMissingSkillMentionWarnsAndRunsPrompt(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	app := newTestApp(t, &out, &errw, fp)
	app.Skills = map[string]skills.Skill{
		"plans": {Name: "plans", Description: "Plan work", Location: filepath.Join(t.TempDir(), "gone", "SKILL.md")},
	}

	if code := Run(strings.NewReader("$plans hello\n/exit\n"), app, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 1 {
		t.Fatalf("provider requests = %d, want 1", fp.RequestCount())
	}
	req := fp.Requests[0]
	if got := req.Messages[0].Content[0].Text; got != "$plans hello" {
		t.Fatalf("prompt = %q, want the raw prompt", got)
	}
	if !strings.Contains(errw.String(), "warning: skill not injected: read skill \"plans\"") {
		t.Fatalf("missing warning, errw=%q", errw.String())
	}
}

func TestSortedSkillNamesSortsAndTreatsEmptyAsDisabled(t *testing.T) {
	if got := sortedSkillNames(nil); got != nil {
		t.Fatalf("sortedSkillNames(nil) = %v, want nil", got)
	}
	if got := sortedSkillNames(map[string]skills.Skill{}); got != nil {
		t.Fatalf("sortedSkillNames(empty) = %v, want nil", got)
	}
	got := sortedSkillNames(map[string]skills.Skill{
		"gamma": {Name: "gamma"},
		"alpha": {Name: "alpha"},
		"beta":  {Name: "beta"},
	})
	want := []string{"alpha", "beta", "gamma"}
	if !slices.Equal(got, want) {
		t.Fatalf("sortedSkillNames = %v, want %v", got, want)
	}
}

func TestResolveSkillMentionsSkillScheme(t *testing.T) {
	skill := testSkill(t, "my-skill", "does stuff", "MY BODY")
	available := map[string]skills.Skill{"my-skill": skill}

	res := resolveSkillMentions("please use skill://my-skill/SKILL.md for this", available)
	if res.Injected != 1 || !strings.Contains(res.Prompt, "MY BODY") {
		t.Fatalf("skill:// should inject: %+v", res)
	}
	res = resolveSkillMentions("read "+skill.Location+".", available)
	if res.Injected != 1 {
		t.Fatalf("absolute SKILL.md path should inject: %+v", res)
	}
	res = resolveSkillMentions("use $$skill://my-skill/SKILL.md", available)
	if res.Injected != 0 {
		t.Fatalf("escaped skill:// should not inject: %+v", res)
	}
}

func TestResolveSkillMentionsPlainNameFallback(t *testing.T) {
	commit := testSkill(t, "commit", "Create commits", "COMMIT BODY")
	review := testSkill(t, "review", "Review changes", "REVIEW BODY")
	available := map[string]skills.Skill{"commit": commit, "review": review}

	res := resolveSkillMentions("please (review), then commit.", available)
	if res.Injected != 2 {
		t.Fatalf("plain-name injections = %d, want 2: %+v", res.Injected, res)
	}
	if strings.Index(res.Prompt, "<name>review</name>") > strings.Index(res.Prompt, "<name>commit</name>") {
		t.Fatalf("plain names should preserve prompt order:\n%s", res.Prompt)
	}

	res = resolveSkillMentions("commitment should not trigger", available)
	if res.Injected != 0 {
		t.Fatalf("plain name must be word bounded: %+v", res)
	}

	res = resolveSkillMentions("$commit then commit", available)
	if res.Injected != 1 {
		t.Fatalf("explicit and plain aliases should dedupe: %+v", res)
	}
}
