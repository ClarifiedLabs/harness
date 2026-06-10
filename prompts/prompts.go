// Package prompts exposes the built-in prompt text embedded from the repository
// prompts directory.
package prompts

import (
	"embed"
	"strings"
)

// files embeds the durable prompt bodies shipped with harness.
//
//go:embed *.txt agents/*.txt
var files embed.FS

// System returns the built-in system prompt instructions.
func System() string {
	return mustText("system.txt")
}

// CompactionSummary returns the system instruction used for compaction summary calls.
func CompactionSummary() string {
	return mustText("compaction-summary.txt")
}

// SkillsInstructions returns the behavioral instruction block appended when
// skills are available.
func SkillsInstructions() string {
	return mustText("skills-instructions.txt")
}

// BuiltinAgentPrompt returns the prompt body for a built-in agent name.
func BuiltinAgentPrompt(name string) (string, bool) {
	switch name {
	case "auto", "independent", "plan":
	default:
		return "", false
	}
	return mustText("agents/" + name + ".txt"), true
}

func mustText(path string) string {
	data, err := files.ReadFile(path)
	if err != nil {
		panic("prompt asset missing: " + path)
	}
	return trimFinalNewline(string(data))
}

func trimFinalNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}
