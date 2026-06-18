package ui

import (
	"fmt"
	"strings"

	"harness/internal/skills"
)

type skillMentionResolution struct {
	Prompt  string
	Context []string
	Unknown string
}

func resolveSkillMentions(prompt string, available map[string]skills.Skill) skillMentionResolution {
	resolvedPrompt := unescapeDollarEscapes(prompt)
	if available == nil {
		return skillMentionResolution{Prompt: resolvedPrompt}
	}
	var selected []skills.Skill
	seen := make(map[string]bool)
	for i := 0; i < len(prompt); i++ {
		if prompt[i] != '$' {
			continue
		}
		if i+1 < len(prompt) && prompt[i+1] == '$' {
			i++
			continue
		}
		name, end, ok := skillMentionToken(prompt, i)
		if !ok {
			continue
		}
		if skill, ok := available[name]; ok && !seen[name] {
			selected = append(selected, skill)
			seen[name] = true
		}
		i = end - 1
	}
	if len(selected) > 0 {
		return skillMentionResolution{Prompt: resolvedPrompt, Context: []string{explicitSkillContext(selected)}}
	}
	if name, ok := standaloneUnknownSkillMention(prompt); ok {
		return skillMentionResolution{Prompt: resolvedPrompt, Unknown: name}
	}
	return skillMentionResolution{Prompt: resolvedPrompt}
}

func skillMentionToken(s string, dollar int) (name string, end int, ok bool) {
	if dollar < 0 || dollar >= len(s) || s[dollar] != '$' {
		return "", dollar, false
	}
	start := dollar + 1
	if start >= len(s) || s[start] == '$' {
		return "", start, false
	}
	end = start
	for end < len(s) && isSkillMentionChar(s[end]) {
		end++
	}
	if end == start {
		return "", end, false
	}
	return s[start:end], end, true
}

func isSkillMentionChar(c byte) bool {
	return c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == ':'
}

func standaloneUnknownSkillMention(prompt string) (string, bool) {
	trimmed := strings.TrimSpace(prompt)
	if strings.HasPrefix(trimmed, "$$") || !strings.HasPrefix(trimmed, "$") {
		return "", false
	}
	name, end, ok := skillMentionToken(trimmed, 0)
	if !ok {
		return "", false
	}
	if strings.TrimSpace(trimmed[end:]) != "" {
		return "", false
	}
	return name, true
}

func unescapeDollarEscapes(prompt string) string {
	if !strings.Contains(prompt, "$$") {
		return prompt
	}
	var b strings.Builder
	b.Grow(len(prompt))
	for i := 0; i < len(prompt); i++ {
		if prompt[i] == '$' && i+1 < len(prompt) && prompt[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		b.WriteByte(prompt[i])
	}
	return b.String()
}

func explicitSkillContext(selected []skills.Skill) string {
	var b strings.Builder
	b.WriteString("[explicit skill mentions]\n")
	b.WriteString("The user explicitly mentioned the following skill(s) in this prompt. ")
	b.WriteString("For each listed skill, use the file-read tool to read the full SKILL.md before taking task actions. ")
	b.WriteString("Read each listed SKILL.md completely, then follow the skill instructions. ")
	b.WriteString("Resolve relative paths against the directory containing that SKILL.md.\n")
	for _, skill := range selected {
		fmt.Fprintf(&b, "\n- %s", singleLine(skill.Name))
		if desc := singleLine(skill.Description); desc != "" {
			fmt.Fprintf(&b, ": %s", desc)
		}
		if skill.Location != "" {
			fmt.Fprintf(&b, "\n  path: %s", skill.Location)
		}
	}
	return b.String()
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func (app *App) resolveSkillMentionContext(prompt string) (string, []string, bool) {
	res := resolveSkillMentions(prompt, app.Skills)
	if res.Unknown != "" {
		fmt.Fprintf(app.Errw, "unknown skill %q; type /skills\n", res.Unknown)
		return res.Prompt, nil, false
	}
	return res.Prompt, res.Context, true
}
