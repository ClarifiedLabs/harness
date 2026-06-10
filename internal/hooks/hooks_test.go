package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeFileAcceptsWrapperAndAliases(t *testing.T) {
	cfg, err := DecodeFile([]byte(`{
		"hooks": {
			"PreToolUse": [
				{
					"matcher": "^run_command$",
					"hooks": [
						{"type":"command","command":"printf ok","timeout":3,"statusMessage":"Checking"}
					]
				}
			]
		}
	}`))
	if err != nil {
		t.Fatalf("DecodeFile: %v", err)
	}
	groups := cfg.Groups(PreToolUse)
	if len(groups) != 1 {
		t.Fatalf("PreToolUse groups = %d, want 1", len(groups))
	}
	if !groups[0].matches("run_command") || groups[0].matches("read_file") {
		t.Fatalf("matcher did not behave as expected")
	}
	h := groups[0].Hooks[0]
	if h.TimeoutSeconds != 3 || h.StatusMessage != "Checking" {
		t.Fatalf("aliases not decoded: %+v", h)
	}
}

func TestLoadFilesAppendsInOrderAndUsesBaseDir(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.json")
	second := filepath.Join(dir, "second.json")
	if err := os.WriteFile(first, []byte(`{"PreToolUse":[{"hooks":[{"type":"command","command":"printf first"}]}]}`), 0o644); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := os.WriteFile(second, []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"printf second"}]}]}}`), 0o644); err != nil {
		t.Fatalf("write second: %v", err)
	}
	cfg, err := LoadFiles(dir, []string{"first.json", "second.json"})
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	groups := cfg.Groups(PreToolUse)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if groups[0].Hooks[0].Command != "printf first" || groups[1].Hooks[0].Command != "printf second" {
		t.Fatalf("groups out of order: %+v", groups)
	}
}

func TestRunnerBlocksAndPassesPayloadOnStdin(t *testing.T) {
	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.json")
	cfg, err := DecodeEventMap([]byte(`{
		"PreToolUse": [
			{
				"hooks": [
					{"type":"command","command":"cat > ` + shellQuote(payloadPath) + `; printf '{\"decision\":\"block\",\"reason\":\"no writes\"}'"}
				]
			}
		]
	}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	runner := &Runner{Config: cfg, CWD: dir, SessionID: "s1", TranscriptPath: "s1", Model: "m1"}
	res := runner.Run(context.Background(), PreToolUse, "write_file", Payload{"tool_name": "write_file"})
	if !res.Block || res.Reason() != "no writes" {
		t.Fatalf("result = %+v, want block reason", res)
	}
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(string(payload), `"hook_event_name":"PreToolUse"`) || !strings.Contains(string(payload), `"tool_name":"write_file"`) {
		t.Fatalf("payload missing fields: %s", payload)
	}
}

func TestRunnerExitCodeTwoBlocksPlainOutput(t *testing.T) {
	cfg, err := DecodeEventMap([]byte(`{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf blocked; exit 2"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	res := (&Runner{Config: cfg}).Run(context.Background(), UserPromptSubmit, "", nil)
	if !res.Block || res.Reason() != "blocked" {
		t.Fatalf("result = %+v, want block from exit code 2", res)
	}
}

func TestRunnerTimeoutFailsOpen(t *testing.T) {
	oldUnit := hookTimeoutUnit
	hookTimeoutUnit = 25 * time.Millisecond
	t.Cleanup(func() { hookTimeoutUnit = oldUnit })

	cfg, err := DecodeEventMap([]byte(`{"PreToolUse":[{"hooks":[{"type":"command","command":"sleep 2","timeout_seconds":1}]}]}`))
	if err != nil {
		t.Fatalf("DecodeEventMap: %v", err)
	}
	start := time.Now()
	res := (&Runner{Config: cfg}).Run(context.Background(), PreToolUse, "run_command", nil)
	if res.Block {
		t.Fatalf("timeout should fail open: %+v", res)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
	if len(res.Notices) == 0 || !strings.Contains(res.Notices[0], "timed out") {
		t.Fatalf("timeout notice = %v", res.Notices)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
