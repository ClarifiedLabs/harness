package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"harness/internal/llm"
	"harness/internal/session"
)

type probeFixture struct {
	Files   []string
	Markers []string
	Prompt  string
}

type probeCall struct {
	Path string
	Turn int
}

func createProbeWorkspace(dir string, count, approximateBytes int) (probeFixture, error) {
	fixture := probeFixture{
		Files:   make([]string, 0, count),
		Markers: make([]string, 0, count),
	}
	for i := 1; i <= count; i++ {
		name := fmt.Sprintf("probe-%02d.txt", i)
		marker := fmt.Sprintf("RETENTION_PROBE_%02d_7F3A", i)
		var content strings.Builder
		content.WriteString(marker)
		content.WriteByte('\n')
		line := fmt.Sprintf("probe-%02d deterministic payload 0123456789abcdef\n", i)
		for content.Len()+len(line) <= approximateBytes {
			content.WriteString(line)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content.String()), 0o644); err != nil {
			return probeFixture{}, err
		}
		fixture.Files = append(fixture.Files, name)
		fixture.Markers = append(fixture.Markers, marker)
	}
	fixture.Prompt = probePrompt(fixture.Files, fixture.Markers)
	return fixture, nil
}

func probePrompt(files, markers []string) string {
	return fmt.Sprintf(
		"Work directly and do not delegate or modify files. Read these files in exact numerical order with read: %s. "+
			"Call read exactly once in each model turn, wait for its result, then call the next file; never batch multiple tool calls and use no other tool. "+
			"After all files are read, reply with %s followed by every RETENTION_PROBE marker in numerical order. "+
			"The expected marker count is %d. Do not finish early.",
		strings.Join(files, ", "),
		probeCompleteMarker,
		len(markers),
	)
}

func scoreProbe(
	fixture probeFixture,
	messages []llm.Message,
	events []session.Event,
	priorReasons []string,
) (bool, []string) {
	reasons := append([]string(nil), priorReasons...)
	var calls []probeCall
	for _, event := range events {
		if event.Type != session.EventToolStart {
			continue
		}
		if event.Tool != "read" {
			reasons = append(reasons, "unexpected tool call: "+event.Tool)
			continue
		}
		var input struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(event.Input, &input); err != nil {
			reasons = append(reasons, "invalid read input")
			continue
		}
		calls = append(calls, probeCall{Path: filepath.Base(input.Path), Turn: event.Turn})
	}
	if len(calls) != len(fixture.Files) {
		reasons = append(reasons, fmt.Sprintf("read calls = %d, want %d", len(calls), len(fixture.Files)))
	}
	for i := 0; i < min(len(calls), len(fixture.Files)); i++ {
		if calls[i].Path != fixture.Files[i] {
			reasons = append(reasons, fmt.Sprintf("read %d = %s, want %s", i+1, calls[i].Path, fixture.Files[i]))
		}
		if i > 0 && calls[i].Turn <= calls[i-1].Turn {
			reasons = append(reasons, fmt.Sprintf("reads %d and %d were not on separate ordered turns", i, i+1))
		}
	}
	final := finalAssistantText(messages)
	if !strings.Contains(final, probeCompleteMarker) {
		reasons = append(reasons, "final answer missing "+probeCompleteMarker)
	}
	for _, marker := range fixture.Markers {
		if !strings.Contains(final, marker) {
			reasons = append(reasons, "final answer missing "+marker)
		}
	}
	return len(reasons) == 0, reasons
}

func finalAssistantText(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message.Role != llm.RoleAssistant || message.Phase != llm.AssistantPhaseFinal {
			continue
		}
		var parts []string
		for _, block := range message.Content {
			if block.Kind == llm.BlockText && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
