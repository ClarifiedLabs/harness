package ui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"harness/internal/skills"
)

type skillMentionResolution struct {
	Prompt  string
	Context []string
	Unknown string
	Err     error
}

func normalizeSkillPath(p string) string {
	if strings.HasPrefix(p, "skill://") {
		p = strings.TrimPrefix(p, "skill://")
	}
	return strings.TrimSpace(p)
}

func pathIsSkill(p string) bool {
	norm := normalizeSkillPath(p)
	if strings.HasPrefix(p, "skill://") {
		return true
	}
	base := filepath.Base(filepath.Clean(norm))
	return strings.EqualFold(base, "SKILL.md")
}

func stripTrailingPunct(s string) string {
	for len(s) > 0 {
		switch s[len(s)-1] {
		case '.', ',', ';', ':', ')', ']', '"', '\'', '!', '?':
			s = s[:len(s)-1]
		default:
			return s
		}
	}
	return s
}

func entryMatchesPath(skill skills.Skill, raw string) bool {
	norm := stripTrailingPunct(strings.TrimSpace(raw))
	if norm == "" {
		return false
	}
	cleanNorm := filepath.Clean(normalizeSkillPath(norm))
	if filepath.Clean(skill.Location) == cleanNorm {
		return true
	}
	if strings.EqualFold(filepath.Base(cleanNorm), "SKILL.md") {
		dir := filepath.Dir(cleanNorm)
		if dir != "." && dir != "/" {
			parts := strings.FieldsFunc(cleanNorm, func(r rune) bool { return r == '/' || r == '\\' })
			for _, part := range parts {
				if part == skill.Name {
					return true
				}
			}
		}
	}
	return false
}

func extractSkillTokens(prompt string) []string {
	return strings.FieldsFunc(prompt, func(r rune) bool {
		return !('a' <= r && r <= 'z' ||
			'A' <= r && r <= 'Z' ||
			'0' <= r && r <= '9' ||
			r == '.' || r == '_' || r == '/' || r == ':' ||
			r == '$' || r == '\\' || r == '-')
	})
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
	// Add path/scheme mentions and exact, word-bounded plain names. Explicit
	// `$name` selections above remain primary; seen also acts as the blocked plain
	// name set so aliases do not activate the same skill twice.
	for _, raw := range extractSkillTokens(prompt) {
		if strings.HasPrefix(raw, "$") {
			continue
		}
		tok := stripTrailingPunct(raw)
		if skill, ok := available[tok]; ok {
			if !seen[skill.Name] {
				selected = append(selected, skill)
				seen[skill.Name] = true
			}
			continue
		}
		if !pathIsSkill(tok) {
			continue
		}
		for _, name := range sortedSkillNames(available) {
			skill := available[name]
			if !seen[skill.Name] && entryMatchesPath(skill, tok) {
				selected = append(selected, skill)
				seen[skill.Name] = true
				break
			}
		}
	}
	if len(selected) > 0 {
		context, err := explicitSkillContext(selected)
		return skillMentionResolution{Prompt: resolvedPrompt, Context: context, Err: err}
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

// sortedSkillNames returns the sorted skill names for $skill tab completion.
// nil/empty input returns nil, which disables completion.
func sortedSkillNames(m map[string]skills.Skill) []string {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

func explicitSkillContext(selected []skills.Skill) ([]string, error) {
	context := make([]string, 0, len(selected))
	for _, skill := range selected {
		body, err := skill.Read()
		if err != nil {
			return nil, fmt.Errorf("read skill %q at %s: %w", skill.Name, skill.Location, err)
		}
		context = append(context, skills.ActiveContext(skill.Name, skill.Location, body))
	}
	return context, nil
}

func (app *App) resolveSkillMentionContext(prompt string) (string, []string, bool) {
	res := resolveSkillMentions(prompt, app.Skills)
	if res.Unknown != "" {
		fmt.Fprintf(app.Errw, "unknown skill %q; type /skills\n", res.Unknown)
		return res.Prompt, nil, false
	}
	if res.Err != nil {
		fmt.Fprintf(app.Errw, "skill activation failed: %v\n", res.Err)
		return res.Prompt, nil, false
	}
	return res.Prompt, res.Context, true
}
