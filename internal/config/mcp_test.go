package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"harness/internal/configmeta"
)

func TestRemovedLSPConfigReturnsMigrationErrors(t *testing.T) {
	for _, value := range []string{"null", "false", "{}", `{"enable":false}`, `{"serena":{"enable":false}}`} {
		path := writeConfig(t, `{"lsp":`+value+`}`)
		_, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: path})
		if err == nil || !strings.Contains(err.Error(), "lsp configuration has been removed") {
			t.Fatalf("value=%s error=%v", value, err)
		}
	}
}

func TestRemovedMCPAndLSPEnvironmentReturnsMigrationErrors(t *testing.T) {
	for _, name := range []string{"HARNESS_MCP_ENABLE", "HARNESS_MCP_PROXY", "HARNESS_MCP_PROXY_API_KEY", "HARNESS_MCP_LOCAL_ENABLE", "HARNESS_LSP_ENABLE", "HARNESS_LSP_PREWARM", "HARNESS_LSP_SERENA_ENABLE"} {
		for _, value := range []string{"", "false", "true"} {
			_, err := Load(LoadOptions{LookupEnv: lookup(map[string]string{name: value})})
			if err == nil || !strings.Contains(err.Error(), name+" is no longer supported") || !strings.Contains(err.Error(), "mcp.servers") {
				t.Fatalf("%s=%q error=%v", name, value, err)
			}
		}
	}
}

func TestMCPProjectMergeReplacesWholeServersAndTracksSources(t *testing.T) {
	global := writeConfig(t, `{"mcp":{"max_tools":17,"servers":{"inherited":{"command":"./inherited","env":{"TOKEN":"global"}},"replaced":{"command":"old","args":["old"],"env":{"OLD":"old"}}}}}`)
	root := t.TempDir()
	project := filepath.Join(root, ".harness", "config.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"mcp":{"servers":{"replaced":{"url":"https://example.test/mcp"},"new":{"command":"new"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: global, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Config.MCP.Servers) != 3 || result.Config.MCP.MaxTools != 17 {
		t.Fatalf("mcp=%+v", result.Config.MCP)
	}
	replaced := result.Config.MCP.Servers["replaced"]
	if replaced.Command != "" || len(replaced.Args) != 0 || replaced.Env != nil || replaced.URL == "" {
		t.Fatalf("server fields leaked through merge: %+v", replaced)
	}
	for name, path := range map[string]string{"inherited": global, "replaced": project, "new": project} {
		if got := result.Config.MCP.ServerConfigDirs[name]; got != filepath.Dir(path) {
			t.Errorf("%s dir=%q want %q", name, got, filepath.Dir(path))
		}
		if got := result.Sources["mcp.servers."+name]; got.Kind != configmeta.SourceFile || got.Name != path {
			t.Errorf("%s source=%+v", name, got)
		}
	}
	if result.Sources["mcp.max_tools"].Name != global {
		t.Fatalf("max_tools source=%+v", result.Sources["mcp.max_tools"])
	}
}

func TestMCPServerExpansionErrorsNameSource(t *testing.T) {
	for _, entry := range []string{
		`{"command":"${MISSING}"}`, `{"command":"run","args":["${MISSING}"]}`,
		`{"url":"${MISSING}"}`, `{"url":"https://example.test","headers":{"Token":"${MISSING}"}}`,
		`{"command":"run","env":{"TOKEN":"${MISSING}"}}`, `{"url":"https://example.test","auth":{"type":"oauth2","client_secret":"${MISSING}"}}`,
	} {
		path := writeConfig(t, `{"mcp":{"servers":{"test":`+entry+`}}}`)
		_, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: path})
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "mcp.servers.test") || !strings.Contains(err.Error(), "MISSING") {
			t.Fatalf("entry=%s error=%v", entry, err)
		}
	}
}

func TestMCPProjectionDoesNotMutateConfig(t *testing.T) {
	path := writeConfig(t, `{"mcp":{"servers":{"test":{"url":"https://example.test","env":{"SECRET":"env-secret"},"headers":{"Authorization":"header-secret"},"auth":{"type":"token_command","command":"get-token","args":["auth-secret"]}}}}}`)
	result := load(t, nil, nil, path)
	before, err := json.Marshal(result.Config.MCP)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := json.Marshal(Project(result, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"env-secret", "header-secret", "auth-secret"} {
		if strings.Contains(string(projected), secret) {
			t.Fatalf("projection leaked %q", secret)
		}
	}
	after, err := json.Marshal(result.Config.MCP)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("projection mutated original config")
	}
}

func TestMCPProjectionRedactsExpandedCommandsArgsAndURLs(t *testing.T) {
	const secret = "mcp-projection-secret-sentinel"
	path := writeConfig(t, `{"mcp":{"servers":{"local":{"type":"stdio","command":"/bin/${TOKEN}","args":["--token=${TOKEN}","literal-argument"],"excludedTools":["dangerous"]},"remote":{"type":"http","url":"https://${TOKEN}:${TOKEN}@example.test/mcp?token=${TOKEN}"}}}}`)
	result := load(t, nil, map[string]string{"TOKEN": secret}, path)
	if !strings.Contains(result.Config.MCP.Servers["local"].Command, secret) ||
		!strings.Contains(result.Config.MCP.Servers["local"].Args[0], secret) ||
		!strings.Contains(result.Config.MCP.Servers["remote"].URL, secret) {
		t.Fatal("test requires expanded secrets in resolved config")
	}
	servers := Snapshot(result).Values["mcp.servers"].(map[string]any)
	local := servers["local"].(map[string]any)
	remote := servers["remote"].(map[string]any)
	if len(servers) != 2 || local["type"] != "stdio" || remote["type"] != "http" ||
		local["command"] != redactedValue || remote["url"] != redactedValue ||
		local["url"] != "" || remote["command"] != "" ||
		!reflect.DeepEqual(local["args"], []string{redactedValue, redactedValue}) ||
		!reflect.DeepEqual(local["excludedTools"], []string{"dangerous"}) {
		t.Fatalf("projection lost safe shape or exposed values: %#v", servers)
	}
	for _, includeSources := range []bool{false, true} {
		for _, write := range []func(*bytes.Buffer) error{
			func(out *bytes.Buffer) error { return WriteProjectionJSON(out, result, includeSources) },
			func(out *bytes.Buffer) error { return WriteProjectionText(out, result, includeSources) },
		} {
			var out bytes.Buffer
			if err := write(&out); err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{secret, "literal-argument", "example.test"} {
				if strings.Contains(out.String(), value) {
					t.Fatalf("projection leaked %q: %s", value, out.String())
				}
			}
		}
	}
}

func TestMCPServersDefaultsAndStrictSchema(t *testing.T) {
	result := load(t, nil, nil, "")
	if len(result.Config.MCP.Servers) != 0 || len(result.Config.MCP.ServerConfigDirs) != 0 || result.Sources["mcp.servers"].Kind != configmeta.SourceDefault {
		t.Fatalf("default MCP=%+v", result.Config.MCP)
	}
	for _, body := range []string{`{"mcp":{"servers":null}}`, `{"mcp":{"servers":{"test":{"future":true}}}}`} {
		_, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: writeConfig(t, body)})
		if err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
