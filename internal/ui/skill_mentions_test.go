package ui

import (
	"strings"
	"testing"

	"harness/internal/skills"
)

func TestResolveSkillMentionsSelectsKnownSkillAnywhere(t *testing.T) {
	res := resolveSkillMentions("please use $commit for this", map[string]skills.Skill{
		"commit": {Name: "commit", Description: "Create a git commit", Location: "/skills/commit/SKILL.md"},
	})
	if res.Unknown != "" {
		t.Fatalf("Unknown = %q, want none", res.Unknown)
	}
	if len(res.Context) != 1 {
		t.Fatalf("Context = %d, want 1", len(res.Context))
	}
	ctx := res.Context[0]
	for _, want := range []string{
		"[explicit skill mentions]",
		"user explicitly mentioned",
		"read the full SKILL.md",
		"- commit: Create a git commit",
		"path: /skills/commit/SKILL.md",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context missing %q:\n%s", want, ctx)
		}
	}
}

func TestResolveSkillMentionsPreservesFirstMentionOrderAndDedupes(t *testing.T) {
	res := resolveSkillMentions("$beta then $alpha then $beta", map[string]skills.Skill{
		"alpha": {Name: "alpha", Description: "Alpha", Location: "/skills/alpha/SKILL.md"},
		"beta":  {Name: "beta", Description: "Beta", Location: "/skills/beta/SKILL.md"},
	})
	if len(res.Context) != 1 {
		t.Fatalf("Context = %d, want 1", len(res.Context))
	}
	ctx := res.Context[0]
	beta := strings.Index(ctx, "- beta")
	alpha := strings.Index(ctx, "- alpha")
	if beta < 0 || alpha < 0 || beta > alpha {
		t.Fatalf("skills should appear in first-mention order and once:\n%s", ctx)
	}
	if strings.Count(ctx, "- beta") != 1 {
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
	res := resolveSkillMentions("please use $build-ios-apps:swiftui-patterns", map[string]skills.Skill{
		"build-ios-apps:swiftui-patterns": {
			Name:        "build-ios-apps:swiftui-patterns",
			Description: "Build SwiftUI screens",
			Location:    "/skills/swiftui-patterns/SKILL.md",
		},
	})
	if res.Unknown != "" {
		t.Fatalf("Unknown = %q, want none", res.Unknown)
	}
	if len(res.Context) != 1 {
		t.Fatalf("Context = %d, want 1", len(res.Context))
	}
	if !strings.Contains(res.Context[0], "- build-ios-apps:swiftui-patterns: Build SwiftUI screens") {
		t.Fatalf("context missing colon skill:\n%s", res.Context[0])
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
