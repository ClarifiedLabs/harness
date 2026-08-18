package ui

import (
	"slices"
	"strings"
	"testing"

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

func TestResolveSkillMentionsReportsLoadFailure(t *testing.T) {
	res := resolveSkillMentions("$commit", map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/missing/SKILL.md"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), `read skill "commit"`) {
		t.Fatalf("Err = %v, want skill read failure", res.Err)
	}
	if res.Injected != 0 || res.Prompt != "$commit" {
		t.Fatalf("failed injection changed prompt: %+v", res)
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
