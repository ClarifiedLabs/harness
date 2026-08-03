package main

import (
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
		env, stdout, stderr := testEnv(t, []string{"config", "list", "-format", "json"})
		if code := run(env); code != exitOK {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
		for _, want := range []string{"mcp_servers", "api_keys_file", "metrics_enabled"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("catalog output missing %q:\n%s", want, stdout.String())
			}
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	const (
		argSecret       = "argument-secret-must-not-leak"
		envSecret       = "environment-secret-must-not-leak"
		headerSecret    = "header-secret-must-not-leak"
		urlSecret       = "url-secret-must-not-leak"
		extensionSecret = "extension-secret-must-not-leak"
	)
	original := []byte(`{
  "mcpServers": {
    "local": {
      "command": "/bin/echo",
      "args": ["` + argSecret + `"],
      "env": {"TOKEN": "` + envSecret + `"}
    },
    "remote": {
      "type": "http",
      "url": "https://user:` + urlSecret + `@example.com/mcp",
      "headers": {"Authorization": "Bearer ` + headerSecret + `"}
    }
  },
  "proxy": {
    "listen": "127.0.0.1:8181",
    "api_keys_file": "accepted-keys.json",
    "metrics": {"enabled": false, "listen": "127.0.0.1:9191"}
  },
  "x-extension": {"secret": "` + extensionSecret + `"}
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
		for _, secret := range []string{argSecret, envSecret, headerSecret, urlSecret, extensionSecret} {
			if strings.Contains(stdout.String(), secret) {
				t.Fatalf("resolved output leaked %q:\n%s", secret, stdout.String())
			}
		}
		var projection configmeta.Projection
		if err := json.Unmarshal(stdout.Bytes(), &projection); err != nil {
			t.Fatalf("decode projection: %v\n%s", err, stdout.String())
		}
		proxy, ok := projection.Values["proxy"].(map[string]any)
		if !ok || proxy["logLevel"] != "warn" {
			t.Fatalf("proxy projection=%v", projection.Values["proxy"])
		}
		servers, ok := projection.Values["mcpServers"].(map[string]any)
		if !ok || servers["count"] != float64(2) {
			t.Fatalf("server summary=%v", projection.Values["mcpServers"])
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

func TestRunConfigCommandsDoNotLeakExpandedURLCredentialsInWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"bad":{"type":"http","url":"https://user:${MCP_SECRET}@example.invalid/%zz"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const secret = "expanded-url-password-must-not-leak"
	for _, command := range []string{"show", "check"} {
		t.Run(command, func(t *testing.T) {
			env, stdout, stderr := testEnv(t, []string{"config", command, "-config", path})
			baseGetenv := env.getenv
			env.getenv = func(key string) string {
				if key == "MCP_SECRET" {
					return secret
				}
				return baseGetenv(key)
			}
			if code := run(env); code != exitOK {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if combined := stdout.String() + stderr.String(); strings.Contains(combined, secret) {
				t.Fatalf("config %s leaked expanded URL credentials: %s", command, combined)
			}
			if !strings.Contains(stderr.String(), `server "bad" skipped`) {
				t.Fatalf("config %s warning=%q", command, stderr.String())
			}
		})
	}
}

func TestRunConfigCheckRejectsMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"proxy":`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _, stderr := testEnv(t, []string{"config", "check", "-config", path})
	if code := run(env); code != exitUsage || stderr.Len() == 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}
