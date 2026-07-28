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
	if len(res.Context) != 1 {
		t.Fatalf("Context = %d, want 1", len(res.Context))
	}
	ctx := res.Context[0]
	for _, want := range []string{
		"[active skill instructions]",
		"name: commit",
		"source: " + commit.Location,
		"# Commit skill",
		"COMMIT BODY",
		"[end active skill instructions]",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context missing %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "read the full SKILL.md") {
		t.Fatalf("explicit activation should not request a model tool round:\n%s", ctx)
	}
}

func TestResolveSkillMentionsPreservesFirstMentionOrderAndDedupes(t *testing.T) {
	alpha := testSkill(t, "alpha", "Alpha", "ALPHA BODY")
	beta := testSkill(t, "beta", "Beta", "BETA BODY")
	res := resolveSkillMentions("$beta then $alpha then $beta", map[string]skills.Skill{
		"alpha": alpha,
		"beta":  beta,
	})
	if len(res.Context) != 2 {
		t.Fatalf("Context = %d, want 2", len(res.Context))
	}
	ctx := strings.Join(res.Context, "\n")
	betaIndex := strings.Index(ctx, "name: beta")
	alphaIndex := strings.Index(ctx, "name: alpha")
	if betaIndex < 0 || alphaIndex < 0 || betaIndex > alphaIndex {
		t.Fatalf("skills should appear in first-mention order and once:\n%s", ctx)
	}
	if strings.Count(ctx, "name: beta") != 1 {
		t.Fatalf("beta should be deduped:\n%s", ctx)
	}
}

func TestResolveSkillMentionsEscapedDollarIsLiteral(t *testing.T) {
	res := resolveSkillMentions("please use $$commit", map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	})
	if res.Unknown != "" || len(res.Context) != 0 {
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
	if len(res.Context) != 1 {
		t.Fatalf("Context = %d, want 1", len(res.Context))
	}
	if !strings.Contains(res.Context[0], "name: build-ios-apps:swiftui-patterns") ||
		!strings.Contains(res.Context[0], "SWIFTUI BODY") {
		t.Fatalf("context missing colon skill:\n%s", res.Context[0])
	}
}

func TestResolveSkillMentionsReportsLoadFailure(t *testing.T) {
	res := resolveSkillMentions("$commit", map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/missing/SKILL.md"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), `read skill "commit"`) {
		t.Fatalf("Err = %v, want skill read failure", res.Err)
	}
	if len(res.Context) != 0 {
		t.Fatalf("Context = %v, want none on failed activation", res.Context)
	}
}

func TestResolveSkillMentionsUnknownInlineDollarIsLiteral(t *testing.T) {
	res := resolveSkillMentions("print $PATH and $missing", map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	})
	if res.Unknown != "" || len(res.Context) != 0 {
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
	if len(res.Context) != 0 {
		t.Fatalf("Context = %d, want 0", len(res.Context))
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
