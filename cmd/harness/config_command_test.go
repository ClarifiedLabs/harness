package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/config"
	"harness/internal/configmeta"
	"harness/internal/ui"
)

func configCommandEnv(t *testing.T, args []string, values map[string]string) (environment, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	if values == nil {
		values = make(map[string]string)
	}
	values["HOME"] = home
	lookup := func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
	getenv := func(name string) string {
		value, _ := lookup(name)
		return value
	}
	var stdout, stderr bytes.Buffer
	return environment{
		args:      args,
		stdout:    &stdout,
		stderr:    &stderr,
		getenv:    getenv,
		lookupEnv: lookup,
	}, &stdout, &stderr
}

func TestRunConfigListFormatsCatalogWithoutLoadingConfiguration(t *testing.T) {
	for _, format := range []string{"text", "json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			env, stdout, stderr := configCommandEnv(t,
				[]string{"config", "list", "-format", format},
				map[string]string{
					"HARNESS_CONFIG":    filepath.Join(t.TempDir(), "missing.json"),
					"HARNESS_MAX_TURNS": "not-an-integer",
				},
			)
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("exit = %d, want ok; stderr=%q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			output := stdout.String()
			for _, expected := range []string{"max_turns", "HARNESS_MAX_TURNS", "max-turns"} {
				if !strings.Contains(output, expected) {
					t.Fatalf("%s output missing %q:\n%s", format, expected, output)
				}
			}
			if strings.Contains(output, "not-an-integer") {
				t.Fatalf("list unexpectedly loaded environment values:\n%s", output)
			}
		})
	}
}

func TestRunConfigShowResolvesPrecedenceSourcesAndRedaction(t *testing.T) {
	configPath := writeMainConfig(t, `{
		"max_turns": 3,
		"model_proxy_api_key": "file-secret",
		"mcp": {"headers": {"Authorization": "Bearer secret"}}
	}`)
	env, stdout, stderr := configCommandEnv(t,
		[]string{"config", "show", "-config", configPath, "-format", "json", "-sources", "-max-turns", "9"},
		map[string]string{"HARNESS_MAX_TURNS": "7"},
	)
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d, want ok; stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var projection config.Projection
	if err := json.Unmarshal(stdout.Bytes(), &projection); err != nil {
		t.Fatalf("decode projection: %v\n%s", err, stdout.String())
	}
	if projection.Version != 1 {
		t.Fatalf("version = %d, want 1", projection.Version)
	}
	if projection.Values["max_turns"] != float64(9) {
		t.Fatalf("max_turns = %v, want 9", projection.Values["max_turns"])
	}
	if projection.Values["model_proxy_api_key"] != "<redacted>" {
		t.Fatalf("model_proxy_api_key = %v, want redacted", projection.Values["model_proxy_api_key"])
	}
	mcp, ok := projection.Values["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp = %T, want object", projection.Values["mcp"])
	}
	headers, ok := mcp["headers"].(map[string]any)
	if !ok || headers["Authorization"] != "<redacted>" {
		t.Fatalf("mcp.headers = %#v, want redacted values", mcp["headers"])
	}
	if source := projection.Sources["max_turns"]; source != (configmeta.Source{Kind: configmeta.SourceFlag, Name: "--max-turns"}) {
		t.Fatalf("max_turns source = %+v", source)
	}
	if strings.Contains(stdout.String(), "file-secret") || strings.Contains(stdout.String(), "Bearer secret") {
		t.Fatalf("projection leaked secrets:\n%s", stdout.String())
	}
}

func TestRunConfigShowTextSourcesAreOptional(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		wantSources bool
	}{
		{name: "values only", args: []string{"config", "show"}},
		{name: "with sources", args: []string{"config", "show", "-sources"}, wantSources: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, stdout, stderr := configCommandEnv(t, tc.args, nil)
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
			}
			hasSource := strings.Contains(stdout.String(), "SOURCE") && strings.Contains(stdout.String(), "derived:runtime:model-proxy-url")
			if hasSource != tc.wantSources {
				t.Fatalf("source columns present = %t, want %t:\n%s", hasSource, tc.wantSources, stdout.String())
			}
		})
	}
}

func TestRunConfigShowRejectsNonSettingInvocationFlags(t *testing.T) {
	env, _, stderr := configCommandEnv(t, []string{"config", "show", "-p", "hello"}, nil)
	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage", code)
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -p") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunConfigCommandsRejectStrictSourceErrors(t *testing.T) {
	t.Run("show malformed environment", func(t *testing.T) {
		env, _, stderr := configCommandEnv(t, []string{"config", "show"}, map[string]string{"HARNESS_MAX_TURNS": "many"})
		if code := run(env); code != ui.ExitUsage || !strings.Contains(stderr.String(), "HARNESS_MAX_TURNS") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})
	t.Run("check unknown JSON field", func(t *testing.T) {
		path := writeMainConfig(t, `{"unknown_setting":true}`)
		env, _, stderr := configCommandEnv(t, []string{"config", "check", "-config", path}, nil)
		if code := run(env); code != ui.ExitUsage || !strings.Contains(stderr.String(), "unknown field") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})
	t.Run("explicit empty config path", func(t *testing.T) {
		env, _, stderr := configCommandEnv(t, []string{"config", "check", "-config="}, nil)
		if code := run(env); code != ui.ExitUsage || !strings.Contains(stderr.String(), "non-empty path") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})
}

func TestRunConfigCheckValidatesLocalReferencesWithoutModelProxy(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "system.txt")
	if err := os.WriteFile(promptPath, []byte("system"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	body := `{
		"model_proxy_url": "://not-a-real-proxy",
		"system_prompt": "@system.txt",
		"agents": {
			"audit": {
				"description": "Audit risky changes when review is requested.",
				"allowed_tools": ["read"],
				"prompt": "@system.txt"
			}
		}
	}`
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	env, stdout, stderr := configCommandEnv(t, []string{"config", "check", "-config", configPath}, nil)
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "config ok: "+configPath+"\n" {
		t.Fatalf("stdout = %q", got)
	}

	if err := os.Remove(promptPath); err != nil {
		t.Fatal(err)
	}
	env, _, stderr = configCommandEnv(t, []string{"config", "check", "-config", configPath}, nil)
	if code := run(env); code != ui.ExitUsage || !strings.Contains(stderr.String(), "system.txt") {
		t.Fatalf("missing reference: exit=%d stderr=%q", code, stderr.String())
	}

	overriddenPath := filepath.Join(dir, "overridden.json")
	if err := os.WriteFile(overriddenPath, []byte(`{"system_prompt":"@missing.txt"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env, _, stderr = configCommandEnv(t, []string{"config", "check", "-config", overriddenPath}, map[string]string{"HARNESS_SYSTEM_PROMPT": "inline override"})
	if code := run(env); code != ui.ExitUsage || !strings.Contains(stderr.String(), "missing.txt") {
		t.Fatalf("hidden missing reference: exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunConfigSubcommandHelpAndUsage(t *testing.T) {
	for _, args := range [][]string{
		{"config", "--help"},
		{"config", "list", "--help"},
		{"config", "show", "--help"},
		{"config", "check", "--help"},
	} {
		env, stdout, stderr := configCommandEnv(t, args, nil)
		if code := run(env); code != ui.ExitOK || stdout.Len() == 0 || stderr.Len() != 0 {
			t.Fatalf("args=%v exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}

	env, _, stderr := configCommandEnv(t, []string{"config", "unknown"}, nil)
	if code := run(env); code != ui.ExitUsage || !strings.Contains(stderr.String(), "Usage:\n  harness config") {
		t.Fatalf("unknown command: exit=%d stderr=%q", code, stderr.String())
	}
}
