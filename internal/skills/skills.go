// Package skills discovers and parses Agent Skills from SKILL.md files, builds
// a catalog for prompt disclosure, and supplies behavioral instructions so the
// model can activate skills via its existing file-read tool (progressive
// disclosure: catalog → SKILL.md body → bundled resources on demand).
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"harness/prompts"
)

// skillFile is the canonical filename inside each skill subdirectory.
const skillFile = "SKILL.md"

// maxScanDepth bounds recursive scanning of a skill directory to prevent
// runaway traversal in large or cyclic directory trees.
const maxScanDepth = 4

// maxDirs is an upper bound on the number of directories scanned per skill
// root, preventing runaway scanning.
const maxDirs = 2000

// skippedDirs are subdirectory names the scanner never descends into.
var skippedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
}

// nameMaxLen is the spec-defined maximum length of a skill name. Names that
// exceed it trigger a warning but are still loaded (lenient validation).
const nameMaxLen = 64

// Scope determines precedence on name collisions: project-level skills override
// user-level skills.
type Scope int

const (
	ScopeUser    Scope = iota // ~/.agents/skills/ (lowest precedence)
	ScopeProject              // <project>/.agents/skills/ (highest precedence)
)

// Warnings collects non-fatal diagnostic messages produced during discovery.
// Callers surface them to the user (stderr, log) without blocking skill loading.
type Warnings []string

// Warn appends a formatted warning.
func (w *Warnings) Warn(format string, args ...any) {
	*w = append(*w, fmt.Sprintf(format, args...))
}

// Skill is a discovered and parsed agent skill. Name and Description come from
// the YAML frontmatter; Location is the absolute path to the SKILL.md file
// (the skill's base directory is filepath.Dir(Location)). Body is the markdown
// content after the frontmatter — populated only when Read is called.
type Skill struct {
	Name        string
	Description string
	Location    string // absolute path to SKILL.md
	Scope       Scope
}

// Read returns the full text of the SKILL.md file at Location. Called by the
// model (via read_file) or by the harness to feed the body into context at
// activation time.
func (s Skill) Read() (string, error) {
	data, err := os.ReadFile(s.Location)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// discovered bundles a parsed skill with its scope for collision resolution.
type discovered struct {
	skill Skill
	scope Scope
}

// scopeName renders a scope for diagnostic messages.
func scopeName(s Scope) string {
	if s == ScopeUser {
		return "user"
	}
	return "project"
}

// Dir is an absolute path to scan for skill subdirectories.
type Dir struct {
	Path  string
	Scope Scope
}

// Discover scans the given directories for skills and returns a map keyed by
// name. Project-level skills override user-level skills on name collision; a
// warning is recorded when this happens. Skips skills missing a description
// (essential for disclosure) and records warnings for other issues (name
// length, directory-name mismatch) without dropping the skill.
func Discover(dirs []Dir, warn *Warnings) map[string]Skill {
	if warn == nil {
		warn = new(Warnings)
	}
	var found []discovered
	for _, d := range dirs {
		found = append(found, scanDir(d.Path, d.Scope, warn)...)
	}
	return resolve(found, warn)
}

// resolve applies the precedence rule: project > user, first-found within the
// same scope. Collisions produce a warning naming the shadowed skill's origin.
func resolve(found []discovered, warn *Warnings) map[string]Skill {
	result := make(map[string]Skill)
	source := make(map[string]Scope)
	for _, d := range found {
		name := d.skill.Name
		if existing, ok := result[name]; ok {
			if d.scope > source[name] {
				// Higher scope wins; warn about the shadowed lower-scope skill.
				warn.Warn("skill %q from %s scope (%s) shadows %s scope (%s)",
					name, scopeName(d.scope), filepath.Dir(d.skill.Location),
					scopeName(source[name]), filepath.Dir(existing.Location))
				result[name] = d.skill
				source[name] = d.scope
			} else {
				warn.Warn("skill %q from %s scope (%s) shadowed by %s scope (%s)",
					name, scopeName(d.scope), filepath.Dir(d.skill.Location),
					scopeName(source[name]), filepath.Dir(existing.Location))
			}
			continue
		}
		result[name] = d.skill
		source[name] = d.scope
	}
	return result
}

// scanDir walks one skill root, collecting any subdirectory that contains a
// SKILL.md file (up to maxScanDepth levels deep, capped at maxDirs dirs).
func scanDir(root string, scope Scope, warn *Warnings) []discovered {
	var out []discovered
	if root == "" {
		return nil
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	dirs := 0
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > maxScanDepth || dirs >= maxDirs {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if dirs >= maxDirs {
				return
			}
			name := e.Name()
			if !e.IsDir() {
				continue // skills live in subdirectories
			}
			if skippedDirs[name] {
				continue
			}
			// Hidden directories are skipped unless literally named `.agents`,
			// which is the cross-client convention the scan root may descend into.
			if strings.HasPrefix(name, ".") && name != ".agents" {
				continue
			}
			dirs++
			sub := filepath.Join(dir, name)
			skillPath := filepath.Join(sub, skillFile)
			if info, err := os.Stat(skillPath); err == nil && !info.IsDir() {
				if s, ok := parseSKILL(skillPath, scope, warn); ok {
					out = append(out, discovered{skill: s, scope: scope})
				}
			} else {
				walk(sub, depth+1)
			}
		}
	}
	walk(root, 0)
	return out
}

// parseSKILL reads and parses a SKILL.md at path, returning the skill and ok
// on success. A missing description drops the skill; other issues warn and
// still load.
func parseSKILL(path string, scope Scope, warn *Warnings) (Skill, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		warn.Warn("reading %s: %v", path, err)
		return Skill{}, false
	}
	fm, err := parseFrontmatter(string(data))
	if err != nil {
		warn.Warn("parsing frontmatter in %s: %v", path, err)
		return Skill{}, false
	}
	dirName := filepath.Base(filepath.Dir(path))
	name := fm["name"]
	desc := fm["description"]
	if name == "" {
		// Fall back to the parent directory name when frontmatter omits name.
		name = dirName
	}
	if desc == "" {
		warn.Warn("skill %q at %s has no description; skipping", name, path)
		return Skill{}, false
	}
	if name != dirName {
		warn.Warn("skill %q at %s: name does not match directory name %q (loading anyway)",
			name, path, dirName)
	}
	if len(name) > nameMaxLen {
		warn.Warn("skill %q at %s: name exceeds %d characters (loading anyway)",
			name, path, nameMaxLen)
	}
	return Skill{
		Name:        name,
		Description: desc,
		Location:    path,
		Scope:       scope,
	}, true
}

// BuildCatalog renders the compact catalog block listing the given skills (Tier 1
// of progressive disclosure). The block is empty when no skills are supplied;
// callers should then omit the entire skills section from the system prompt.
func BuildCatalog(skills map[string]Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available skills:")
	for _, name := range sortedNames(skills) {
		s := skills[name]
		fmt.Fprintf(&b, "\n- %s: %s (read %s)", oneLine(s.Name), oneLine(s.Description), s.Location)
	}
	return b.String()
}

// Instructions returns the behavioral block that accompanies the catalog in
// the system prompt, telling the model how to activate skills via its existing
// file-read tool. Empty when no skills are supplied.
func Instructions(count int) string {
	if count == 0 {
		return ""
	}
	return prompts.SkillsInstructions()
}

// parseFrontmatter extracts key/value pairs from the YAML frontmatter block
// delimited by `---` at the start of content. It handles single- and
// double-quoted values, plain values (with optional inline comments stripped),
// and `|` / `>` block scalars — enough for real-world SKILL.md files without a
// full YAML parser. Returns (nil, "no frontmatter found") when the file does
// not begin with a `---` line.
func parseFrontmatter(content string) (map[string]string, error) {
	lines := splitLines(content)
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("no frontmatter found")
	}
	result := make(map[string]string)
	i := 1
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			return result, nil
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			i++
			continue // skip malformed lines
		}
		key := strings.TrimSpace(line[:colonIdx])
		rest := ""
		if colonIdx+1 < len(line) {
			rest = strings.TrimSpace(line[colonIdx+1:])
		}
		if rest == "" || rest == "|" || rest == ">" {
			// Multi-line block scalar: collect indented lines until a non-blank
			// non-indented line or the closing delimiter.
			fold := rest == ">"
			i++
			var block []string
			for i < len(lines) {
				bl := lines[i]
				if strings.TrimSpace(bl) == "---" {
					// Closing delimiter; stop here.
					break
				}
				if bl == "" {
					block = append(block, "")
					i++
					continue
				}
				if len(bl) == 0 || (bl[0] != ' ' && bl[0] != '\t') {
					break
				}
				block = append(block, strings.TrimLeft(bl, " \t"))
				i++
			}
			if fold {
				result[key] = strings.Join(block, " ")
			} else {
				result[key] = strings.Join(block, "\n")
			}
			continue
		}
		result[key] = unquoteValue(rest)
		i++
	}
	return result, fmt.Errorf("unterminated frontmatter")
}

// unquoteValue strips matching single or double quotes from v and returns the
// inner text; non-quoted values are returned as-is after stripping a trailing
// ` #…` inline comment.
func unquoteValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	if idx := strings.Index(v, " #"); idx >= 0 {
		v = strings.TrimRight(v[:idx], " \t")
	}
	return v
}

// splitLines splits content into lines without trailing newlines, handling
// both \n and \r\n line endings.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if s == "" {
		return nil
	}
	if strings.HasSuffix(s, "\n") {
		s = s[:len(s)-1]
	}
	return strings.Split(s, "\n")
}

// sortedNames returns the map keys in ascending order for stable catalog output.
func sortedNames(m map[string]Skill) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
