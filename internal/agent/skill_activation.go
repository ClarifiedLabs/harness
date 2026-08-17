package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"harness/internal/llm"
	"harness/internal/skills"
	"harness/internal/toolresult"
)

type activeSkillSet struct {
	order  []string
	byPath map[string]activeSkill
}

type activeSkill struct {
	digest  [sha256.Size]byte
	context string
}

func (set *activeSkillSet) contexts() []string {
	if len(set.order) == 0 {
		return nil
	}
	context := make([]string, 0, len(set.order))
	for _, path := range set.order {
		context = append(context, set.byPath[path].context)
	}
	return context
}

func (set *activeSkillSet) activate(path, body, context string) (status, digest string) {
	if set.byPath == nil {
		set.byPath = make(map[string]activeSkill)
	}
	sum := skillBodyDigest(body)
	if context == "" {
		name := filepath.Base(filepath.Dir(path))
		context = skills.ActiveContext(name, path, body)
	}
	current, exists := set.byPath[path]
	switch {
	case !exists:
		status = "activated"
		set.order = append(set.order, path)
	case current.digest == sum:
		status = "already active"
		context = current.context
	default:
		status = "reactivated after source change"
	}
	set.byPath[path] = activeSkill{digest: sum, context: context}
	return status, fmt.Sprintf("%x", sum[:8])
}

// skillBodyDigest digests a skill body after trimming at most one trailing
// newline. The read path's decoded body and the raw file differ only by that
// newline (line numbering drops it and reconstruction does not re-add it), so
// normalized digests make both forms compare equal.
func skillBodyDigest(body string) [sha256.Size]byte {
	return sha256.Sum256([]byte(strings.TrimSuffix(body, "\n")))
}

// seedActiveSkills moves recognized active-skill blocks from extraContext into
// the pinned set so a model re-read of the same SKILL.md dedupes to one copy.
// Each parsed item keeps its original text as the pinned context, so the
// model-visible rendering matches the explicit-activation form exactly.
// Blocks without a source path cannot be keyed against a re-read, so they and
// unrecognized items are returned unchanged.
func seedActiveSkills(active *activeSkillSet, extraContext []string) []string {
	kept := make([]string, 0, len(extraContext))
	for _, item := range extraContext {
		_, location, body, ok := skills.ParseActiveContext(item)
		if !ok || location == "" {
			kept = append(kept, item)
			continue
		}
		active.activate(location, body, item)
	}
	return kept
}

// activateSkillReadResults recognizes a successful, complete read of one
// SKILL.md. It pins the decoded instructions in request-only context for the
// rest of the prompt and replaces the replayed result with a typed receipt.
// The exact line-numbered tool output is archived when the session sink
// supports artifacts; the source path remains a recovery path otherwise.
func (a *Agent) activateSkillReadResults(calls []llm.ToolCall, results []llm.ContentBlock, active *activeSkillSet, sink EventSink) {
	for i, call := range calls {
		if i >= len(results) || call.Name != "read" {
			continue
		}
		result := &results[i]
		if result.Kind != llm.BlockToolResult || result.ResultError {
			continue
		}
		path, body, ok := a.completeSkillRead(call, result.ResultText)
		if !ok {
			continue
		}
		status, digest := active.activate(path, body, "")
		if activationSink, ok := sink.(SkillActivationEventSink); ok {
			activationSink.SkillActivated(SkillActivationEvent{Source: "read", Status: strings.ReplaceAll(status, " ", "_")})
		}
		receipt := fmt.Sprintf(
			"[skill activation receipt]\nstatus: %s\nsource: %s\nsha256: %s\ninstructions: pinned in request context for this prompt",
			status,
			path,
			digest,
		)
		if archiver, ok := sink.(ToolResultArchiver); ok {
			archive, err := archiver.ArchiveToolResult(llm.ToolResult{
				ForID:         result.ResultForID,
				Text:          receipt,
				Truncated:     true,
				OriginalText:  result.ResultText,
				OriginalBytes: len(result.ResultText),
				ShownBytes:    len(receipt),
			})
			if err != nil || archive.ModelPath == "" {
				// Preserve the full transcript result on a session-artifact
				// failure. The active request context is still installed, but
				// a transient persistence problem must not discard exact data.
				continue
			}
			receipt += "\n" + toolresult.ArchivedHint(archive.ModelPath)
		}
		result.ResultText = receipt
	}
}

func reportExplicitSkillContexts(context []string, sink EventSink) {
	activationSink, ok := sink.(SkillActivationEventSink)
	if !ok {
		return
	}
	for _, item := range context {
		if strings.HasPrefix(strings.TrimSpace(item), skills.ActiveContextMarker) {
			activationSink.SkillActivated(SkillActivationEvent{Source: "explicit", Status: "activated"})
		}
	}
}

func normalizeSkillReadPath(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "skill://") {
		p = strings.TrimPrefix(p, "skill://")
	}
	return p
}

func isSkillReadPath(p string) bool {
	norm := normalizeSkillReadPath(p)
	base := filepath.Base(filepath.Clean(norm))
	return strings.EqualFold(base, "SKILL.md") || strings.HasPrefix(strings.TrimSpace(p), "skill://")
}

func (a *Agent) completeSkillRead(call llm.ToolCall, result string) (string, string, bool) {
	var args struct {
		Offset int `json:"offset"`
	}
	if json.Unmarshal(call.Input, &args) != nil || args.Offset > 1 {
		return "", "", false
	}
	paths, ok := a.tools.ReadPaths(call)
	if !ok || len(paths) != 1 {
		return "", "", false
	}
	path := paths[0]
	if !isSkillReadPath(path) {
		return "", "", false
	}
	path = normalizeSkillReadPath(path)
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	} else {
		path = filepath.Clean(path)
	}
	body, ok := decodeCompleteLineNumberedRead(result)
	return path, body, ok
}

func decodeCompleteLineNumberedRead(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	lines := strings.Split(text, "\n")
	var body strings.Builder
	for i, line := range lines {
		tab := strings.IndexByte(line, '\t')
		if tab <= 0 || !decimalDigits(line[:tab]) {
			return "", false
		}
		if i > 0 {
			body.WriteByte('\n')
		}
		body.WriteString(line[tab+1:])
	}
	return body.String(), true
}

func decimalDigits(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return text != ""
}
