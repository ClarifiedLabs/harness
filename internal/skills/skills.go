// Package skills discovers and parses Agent Skills from SKILL.md files, builds
// a catalog for prompt disclosure, and supplies behavioral instructions so the
// model can activate skills via explicit request context or its existing
// file-read tool (progressive disclosure: catalog → SKILL.md body → bundled
// resources on demand).
package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// skillFile is the canonical filename inside each skill subdirectory.
const skillFile = "SKILL.md"

// ActiveContextMarker is the stable prefix for a fully activated skill body in
// request-only context.
const ActiveContextMarker = "[active skill instructions]"

// activeContextPromptLine introduces the authoritative body inside an
// ActiveContext block.
const activeContextPromptLine = "The following full SKILL.md is authoritative for this prompt:"

// activeContextEndMarker terminates an ActiveContext block. Because it may
// also appear inside a body, parsers must anchor on the last occurrence.
const activeContextEndMarker = "[end active skill instructions]"

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
// model (via read) or by the harness to feed the body into context at
// activation time.
func (s Skill) Read() (string, error) {
	data, err := os.ReadFile(s.Location)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ActiveContext wraps a fully loaded SKILL.md as request-only authoritative
// context. Keeping the wrapper here gives explicit $mentions and tool-driven
// activation the same stable contract without adding another model-visible
// tool.
func ActiveContext(name, location, body string) string {
	var b strings.Builder
	b.WriteString(ActiveContextMarker + "\n")
	if name = oneLine(name); name != "" {
		fmt.Fprintf(&b, "name: %s\n", name)
	}
	if location != "" {
		fmt.Fprintf(&b, "source: %s\n", location)
	}
	fmt.Fprintf(&b, "%s\n\n", activeContextPromptLine)
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(activeContextEndMarker)
	return b.String()
}

// ParseActiveContext extracts name, location, and body from a block rendered
// by ActiveContext. ok is false for any other text. The end marker is matched
// at its last occurrence and must be the final non-space content, so a body
// that itself contains the marker round-trips. Because ActiveContext always
// separates the body from the end marker with exactly one newline, the body is
// returned in canonical form with at most one trailing newline removed; the
// rendered block is unchanged by re-wrapping it. It never errors.
func ParseActiveContext(item string) (name, location, body string, ok bool) {
	text := strings.TrimSpace(item)
	if !strings.HasPrefix(text, ActiveContextMarker) {
		return "", "", "", false
	}
	rest := text[len(ActiveContextMarker):]
	if !strings.HasPrefix(rest, "\n") {
		return "", "", "", false
	}
	rest = rest[1:]
	for {
		line, tail, found := strings.Cut(rest, "\n")
		if !found {
			return "", "", "", false
		}
		switch {
		case strings.HasPrefix(line, "name: "):
			name = strings.TrimPrefix(line, "name: ")
			rest = tail
		case strings.HasPrefix(line, "source: "):
			location = strings.TrimPrefix(line, "source: ")
			rest = tail
		default:
			// Anything else must be the authoritative prompt line, followed by
			// the blank line that separates it from the body.
			if line != activeContextPromptLine || !strings.HasPrefix(tail, "\n") {
				return "", "", "", false
			}
			bodyText := tail[1:]
			end := strings.LastIndex(bodyText, activeContextEndMarker)
			if end < 0 || strings.TrimSpace(bodyText[end+len(activeContextEndMarker):]) != "" {
				return "", "", "", false
			}
			// ActiveContext guarantees exactly one newline before the end marker;
			// strip only that one so the original body round-trips whether or not
			// it ended in a newline.
			body = strings.TrimSuffix(bodyText[:end], "\n")
			return name, location, body, true
		}
	}
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

// resolve applies the precedence rule: project > user, and for equal scope the
// later (nearest ancestor) wins so inner directories shadow outer ones. Collisions
// produce a warning naming the shadowed skill's origin.
func resolve(found []discovered, warn *Warnings) map[string]Skill {
	result := make(map[string]Skill)
	source := make(map[string]Scope)
	for _, d := range found {
		name := d.skill.Name
		if existing, ok := result[name]; ok {
			if d.scope > source[name] || d.scope == source[name] {
				// Higher scope wins; equal scope later (nearer cwd) wins.
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

// AncestorSkillDirs returns skill directories covering the ancestry from the
// project root to wd, plus the user-level directory. Project roots are resolved
// via `git rev-parse --show-toplevel` with a `.git` walk fallback. Each
// discovered `dir/.agents/skills` that exists and is a directory is included.
// Order is outer (project root) → inner (wd) so the nearest directory wins on
// name collisions when resolve uses later-wins for equal scope. Duplicates are
// removed by canonical path. The user directory is appended last (lowest
// precedence). The implementation is stdlib-only and synchronous.
func AncestorSkillDirs(wd, home string) []Dir {
	var out []Dir
	seen := make(map[string]bool)
	add := func(path string, scope Scope) {
		if path == "" {
			return
		}
		clean := filepath.Clean(path)
		if seen[clean] {
			return
		}
		info, err := os.Stat(clean)
		if err != nil || !info.IsDir() {
			return
		}
		seen[clean] = true
		out = append(out, Dir{Path: clean, Scope: scope})
	}
	absWD := wd
	if absWD != "" {
		if p, err := filepath.Abs(absWD); err == nil {
			absWD = p
		}
		if p, err := filepath.EvalSymlinks(absWD); err == nil {
			absWD = p
		}
	}
	root := projectRoot(absWD)
	ancestors := dirsBetween(root, absWD)
	for _, dir := range ancestors {
		candidate := filepath.Join(dir, ".agents", "skills")
		add(candidate, ScopeProject)
	}
	if home != "" {
		homeSkills := filepath.Join(home, ".agents", "skills")
		add(homeSkills, ScopeUser)
	}
	return out
}

func projectRoot(wd string) string {
	if wd == "" {
		return wd
	}
	if out, err := exec.CommandContext(context.Background(), "git", "-C", wd, "rev-parse", "--show-toplevel").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			if p, err := filepath.EvalSymlinks(s); err == nil {
				s = p
			}
			return filepath.Clean(s)
		}
	}
	dir := filepath.Clean(wd)
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Clean(wd)
}

func dirsBetween(root, wd string) []string {
	if root == "" || wd == "" {
		if wd != "" {
			return []string{filepath.Clean(wd)}
		}
		return nil
	}
	root = filepath.Clean(root)
	wd = filepath.Clean(wd)
	rel, err := filepath.Rel(root, wd)
	if err != nil || strings.HasPrefix(rel, "..") {
		return []string{wd}
	}
	if rel == "." {
		return []string{root}
	}
	parts := strings.Split(rel, string(filepath.Separator))
	out := make([]string, 0, len(parts)+1)
	cur := root
	out = append(out, cur)
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		out = append(out, cur)
	}
	return out
}

// CatalogReport describes how a budgeted catalog was rendered.
type CatalogReport struct {
	Total          int
	Included       int
	Omitted        int
	TruncatedCount int
}

// defaultCatalogBudget is used when no model context window is known.
const defaultCatalogBudget = 8000

// CatalogBudget returns the character budget for always-resident skill metadata:
// 2% of the effective model context window, or 8000 characters when unknown.
// The minimum of one preserves a positive budget for unusually small test or
// custom windows without falling through BuildCatalogBudgeted's default rule.
func CatalogBudget(contextWindow int) int {
	if contextWindow <= 0 {
		return defaultCatalogBudget
	}
	budget := contextWindow / 50
	if budget < 1 {
		return 1
	}
	return budget
}

const catalogOmissionNotice = "\u2026 %d more skills not shown; narrow task or disable skills.\n"

// BuildCatalogBudgeted renders the catalog with a character budget for the
// always-resident "### Available skills" lines. The intro and How-to-use text
// is not counted against the budget. When budget <=0 the default is used.
func BuildCatalogBudgeted(skills map[string]Skill, budget int) (string, CatalogReport) {
	if len(skills) == 0 {
		return "", CatalogReport{}
	}
	if budget <= 0 {
		budget = defaultCatalogBudget
	}
	names := sortedNames(skills)
	report := CatalogReport{Total: len(names)}
	fullDescs := make([]string, len(names))
	locs := make([]string, len(names))
	for i, name := range names {
		s := skills[name]
		fullDescs[i] = catalogDescription(s.Description)
		locs[i] = s.Location
	}
	fullLines := make([]string, len(names))
	minLines := make([]string, len(names))
	for i, name := range names {
		fullLines[i] = fmt.Sprintf("- %s: %s (file: %s)\n", oneLine(name), fullDescs[i], locs[i])
		minLines[i] = fmt.Sprintf("- %s:  (file: %s)\n", oneLine(name), locs[i])
	}
	runeCount := func(s string) int { return utf8.RuneCountInString(s) }
	sumRunes := func(lines []string) int {
		sum := 0
		for _, l := range lines {
			sum += runeCount(l)
		}
		return sum
	}
	sumFull := sumRunes(fullLines)
	sumMin := sumRunes(minLines)
	var b strings.Builder
	b.WriteString("## Skills\n")
	b.WriteString("A skill is a set of instructions stored in a `SKILL.md` file. Below is the list of available skills. Each entry is `name: description (file: absolute-path)`.\n")
	b.WriteString("### Available skills\n")
	if sumFull <= budget {
		for i, name := range names {
			fmt.Fprintf(&b, "- %s: %s (file: %s)\n", oneLine(name), fullDescs[i], locs[i])
		}
		report.Included = len(names)
		report.Omitted = 0
		report.TruncatedCount = 0
	} else if sumMin > budget {
		included := 0
		running := 0
		for i := range names {
			need := runeCount(minLines[i])
			if running+need > budget {
				break
			}
			running += need
			included++
		}
		for i := 0; i < included; i++ {
			fmt.Fprintf(&b, "- %s:  (file: %s)\n", oneLine(names[i]), locs[i])
		}
		report.Included = included
		report.Omitted = len(names) - included
		report.TruncatedCount = 0
		if report.Omitted > 0 {
			fmt.Fprintf(&b, catalogOmissionNotice, report.Omitted)
		}
	} else {
		remaining := budget - sumMin
		allocated := make([]int, len(names))
		need := make([]int, len(names))
		for i := range names {
			need[i] = runeCount(fullDescs[i])
		}
		sumAlloc := 0
		for sumAlloc < remaining {
			progress := false
			for i := range names {
				if allocated[i] < need[i] && sumAlloc < remaining {
					allocated[i]++
					sumAlloc++
					progress = true
					if sumAlloc == remaining {
						break
					}
				}
			}
			if !progress {
				break
			}
		}
		truncatedDescs := make([]string, len(names))
		truncatedCount := 0
		for i := range names {
			if allocated[i] >= need[i] {
				truncatedDescs[i] = fullDescs[i]
			} else {
				// Keep the description within its allocation, including the
				// truncation marker, so rendered skill lines never exceed budget.
				cut := ""
				if allocated[i] > 0 {
					cut = truncateRunes(fullDescs[i], allocated[i]-1)
					cut = strings.TrimRight(cut, " \t") + "\u2026"
				}
				truncatedCount++
				truncatedDescs[i] = cut
			}
		}
		for i, name := range names {
			fmt.Fprintf(&b, "- %s: %s (file: %s)\n", oneLine(name), truncatedDescs[i], locs[i])
		}
		report.Included = len(names)
		report.Omitted = 0
		report.TruncatedCount = truncatedCount
	}
	b.WriteString("### How to use skills\n")
	b.WriteString("- Discovery: list above is authoritative (`file:` lives on host).\n")
	b.WriteString("- Trigger: if user names a skill (`$Name`) OR task clearly matches description, you must use it for that turn. Multiple mentions \u2192 use all. Don't carry across turns.\n")
	b.WriteString("- Missing: if named skill absent or unreadable, say so briefly and fallback.\n")
	b.WriteString("- Progressive disclosure:\n")
	b.WriteString("  1) After deciding, read its `SKILL.md` completely before acting (via `read`). If truncated/paginated, continue until EOF.\n")
	b.WriteString("  2) Resolve relative paths (scripts/references/assets) relative to that `SKILL.md`'s directory.\n")
	b.WriteString("  3) If `SKILL.md` points to `references/`, read only the routing-indicated files; main agent reads them itself.\n")
	b.WriteString("  4) Prefer executing provided scripts/templates over re-typing.\n")
	b.WriteString("- Coordination: choose minimal covering set, announce `using skill X: reason` (one line), say why you skip an obvious one.\n")
	out := strings.TrimRight(b.String(), "\n")
	return out, report
}

// BuildCatalog renders the compact catalog block listing the given skills (Tier 1
// of progressive disclosure). The block is empty when no skills are supplied;
// callers should then omit the entire skills section from the system prompt.
func BuildCatalog(skills map[string]Skill) string {
	s, _ := BuildCatalogBudgeted(skills, CatalogBudget(0))
	return s
}

// catalogDescMaxChars caps the per-skill description rendered into the
// always-resident catalog. The full frontmatter description stays in SKILL.md
// for Tier-2 read, so this only bounds the always-paid prompt cost.
const catalogDescMaxChars = 160

// catalogDescription renders a skill description for the Tier-1 catalog: the
// first sentence or 160 runes, whichever is shorter, with an ellipsis when the
// text was cut mid-sentence. Preferring the first sentence over a hard char cap
// keeps the leading trigger keywords intact while dropping the long tail that
// would otherwise sit in the cache-anchored system prompt for every session,
// even ones that never touch a skill.
func catalogDescription(desc string) string {
	s := oneLine(desc)
	cut := s
	// Prefer the first sentence when it leaves text behind.
	if end := firstSentenceEnd(s); end > 0 && end < len(s) {
		cut = s[:end]
	}
	// Hard char cap as a backstop: a very long first sentence, or no sentence
	// boundary at all. Count runes, not bytes, so a multibyte rune is never split.
	if utf8.RuneCountInString(cut) > catalogDescMaxChars {
		cut = truncateRunes(cut, catalogDescMaxChars)
	}
	if cut == s {
		return s
	}
	cut = strings.TrimRight(cut, " \t")
	if endsWithSentencePunct(cut) {
		// A clean sentence boundary already signals a complete unit.
		return cut
	}
	return cut + "…"
}

// firstSentenceEnd returns the byte index just past the first sentence-ending
// punctuation (. ! ?) that is followed by a space or the end of the string, or
// -1 when there is none. It is a heuristic, not a full sentence tokenizer.
func firstSentenceEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', '!', '?':
			if i+1 >= len(s) || s[i+1] == ' ' {
				return i + 1
			}
		}
	}
	return -1
}

// endsWithSentencePunct reports whether s ends with . ! or ?.
func endsWithSentencePunct(s string) bool {
	if s == "" {
		return false
	}
	switch s[len(s)-1] {
	case '.', '!', '?':
		return true
	}
	return false
}

// truncateRunes returns the first n runes of s, never splitting a multibyte rune.
func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
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
