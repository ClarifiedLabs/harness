package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/configmeta"
)

func TestCommandCatalogHandlersStayInSync(t *testing.T) {
	remaining := make(map[string]struct{}, len(commandHandlers))
	for id := range commandHandlers {
		remaining[id] = struct{}{}
	}
	for _, command := range commandCatalog.Commands() {
		if !command.Runnable {
			continue
		}
		if commandHandlers[command.ID] == nil {
			t.Errorf("runnable command %q has no handler", command.ID)
		}
		delete(remaining, command.ID)
	}
	for id := range remaining {
		t.Errorf("handler %q has no runnable catalog command", id)
	}
}

func TestRunConfigCommandsAreOfflineSafeAndNonMutating(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		env, stdout, stderr := testEnv(t, []string{"config", "list", "-format", "markdown"})
		if code := run(env); code != exitOK {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
		for _, want := range []string{"provider_configs", "models_dev_cache_ttl", "metrics_enabled"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("catalog output missing %q:\n%s", want, stdout.String())
			}
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider.json")
	const providerSecret = "provider-credential-must-not-leak"
	if err := os.WriteFile(providerPath, []byte(`{"name":"private","api_type":"openai","base_url":"https://example.invalid/v1","api_key":"`+providerSecret+`","models":[{"name":"test-model","context_window":8192}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	original := []byte(`{
  "provider_configs": ["provider.json"],
  "log_level": "info",
  "api_keys_file": "accepted-keys.json",
  "metrics": {"enabled": false, "listen": "127.0.0.1:9190"},
  "x-extension": {"preserve": true}
}`)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("show", func(t *testing.T) {
		env, stdout, stderr := testEnv(t, []string{
			"config", "show", "-config", configPath, "-format", "json", "-sources", "-log-level", "warn",
		})
		if code := run(env); code != exitOK {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr=%q", stderr.String())
		}
		if strings.Contains(stdout.String(), providerSecret) {
			t.Fatalf("resolved output leaked provider credential:\n%s", stdout.String())
		}
		var projection configmeta.Projection
		if err := json.Unmarshal(stdout.Bytes(), &projection); err != nil {
			t.Fatalf("decode projection: %v\n%s", err, stdout.String())
		}
		if projection.Version != 1 || projection.Values["log_level"] != "warn" {
			t.Fatalf("projection=%+v", projection)
		}
		if source := projection.Sources["log_level"]; source.Kind != configmeta.SourceFlag {
			t.Fatalf("log source=%+v, want flag", source)
		}
	})

	t.Run("check", func(t *testing.T) {
		env, stdout, stderr := testEnv(t, []string{"config", "check", "-config", configPath})
		if code := run(env); code != exitOK {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "config ok: "+configPath) || stderr.Len() != 0 {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("config commands mutated config:\nbefore=%s\nafter=%s", original, after)
	}
}

func TestRunConfigCheckReportsSkippedProviderAndRejectsEmptyCatalog(t *testing.T) {
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider.json")
	if err := os.WriteFile(providerPath, []byte(`{"name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	original := []byte(`{"provider_configs":["provider.json"]}`)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	env, stdout, stderr := testEnv(t, []string{"config", "check", "-config", configPath})
	if code := run(env); code != exitUsage {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"warning: skipping provider config provider.json", "no provider configs are configured"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %q", want, stderr.String())
		}
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(original) {
		t.Fatalf("config mutated: err=%v before=%q after=%q", err, original, after)
	}
}

func TestRunConfigCheckValidatesProviderAuthWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider.json")
	provider := []byte(`{"name":"private","api_type":"openai","base_url":"https://example.invalid/v1","models":[{"name":"test-model","context_window":8192}],"auth":{"type":"token_command"}}`)
	if err := os.WriteFile(providerPath, provider, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	config := []byte(`{"provider_configs":["provider.json"]}`)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	env, stdout, stderr := testEnv(t, []string{"config", "check", "-config", configPath})
	if code := run(env); code != exitUsage {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "token_command requires command") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	for path, want := range map[string][]byte{configPath: config, providerPath: provider} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("config check mutated %s", path)
		}
	}
}

func TestRunConfigCheckRejectsMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"log_level":`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _, stderr := testEnv(t, []string{"config", "check", "-config", path})
	if code := run(env); code != exitUsage || stderr.Len() == 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}
