package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"harness/internal/mcpproxy"
)

func TestLegacyMCPAutomaticMigration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			enable := "false"
			if enabled {
				enable = "true"
			}
			original := `{"mcp":{"enable":` + enable + `,"proxy":"https://example.test/mcp","api_key":"${TOKEN}","headers":{"X-Custom":"value"},"max_tools":17,"disabled_servers":["blocked"],"local":{"enable":` + enable + `,"command":"runner","args":["--key=${TOKEN}"],"env":{"KEY":"${TOKEN}"}}}}`
			path := writeConfig(t, original)
			result := load(t, nil, map[string]string{"TOKEN": "secret"}, path)
			shared, local := result.Config.MCP.Servers["shared"], result.Config.MCP.Servers["local"]
			want := "${TOKEN}"
			if enabled {
				want = "secret"
			}
			if shared.Enabled == nil || *shared.Enabled != enabled || shared.Type != "http" || shared.URL != "https://example.test/mcp" || shared.Headers["Authorization"] != "Bearer "+want || shared.Headers["X-Custom"] != "value" {
				t.Fatalf("shared=%+v", shared)
			}
			if local.Enabled == nil || *local.Enabled != enabled || local.Type != "stdio" || local.Command != "runner" || !reflect.DeepEqual(local.Args, []string{"--key=" + want}) || local.Env["KEY"] != want {
				t.Fatalf("local=%+v", local)
			}
			if result.Config.MCP.MaxTools != 17 || !reflect.DeepEqual(result.Config.MCP.DisabledServers, []string{"blocked"}) {
				t.Fatalf("mcp=%+v", result.Config.MCP)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != original {
				t.Fatalf("automatic load wrote config: %v", err)
			}
		})
	}
}

func TestLegacyMCPDefaultsDisabledAndSkipsExpansion(t *testing.T) {
	path := writeConfig(t, `{"mcp":{"url":"${MISSING}","api_key":"${MISSING}","local":{"command":"${MISSING}","args":["${MISSING}"],"env":{"KEY":"${MISSING}"}}}}`)
	result := load(t, nil, nil, path)
	for _, name := range []string{"shared", "local"} {
		server := result.Config.MCP.Servers[name]
		if server.Enabled == nil || *server.Enabled {
			t.Fatalf("%s enabled", name)
		}
		entry := Snapshot(result).Values["mcp.servers"].(map[string]any)[name].(map[string]any)
		if entry["enabled"] != false {
			t.Fatalf("projection=%v", entry)
		}
	}
}

func TestDisabledMCPServerExpansionAndClone(t *testing.T) {
	path := writeConfig(t, `{"mcp":{"servers":{"saved":{"enabled":false,"url":"${MISSING}","command":"${MISSING}","args":["${MISSING}"],"env":{"KEY":"${MISSING}"},"headers":{"Token":"${MISSING}"},"auth":{"type":"token_command","command":"${MISSING}"}}}}}`)
	result := load(t, nil, nil, path)
	server := result.Config.MCP.Servers["saved"]
	if server.URL != "${MISSING}" || server.Auth.Command != "${MISSING}" {
		t.Fatalf("definition changed: %+v", server)
	}
	cloned := cloneMCPServers(result.Config.MCP.Servers)
	*cloned["saved"].Enabled = true
	if *server.Enabled {
		t.Fatal("enabled pointer was not cloned")
	}
}

func TestLegacyMCPStrictFailures(t *testing.T) {
	bodies := []string{
		`{"mcp":{"enable":null}}`, `{"mcp":{"enable":"false"}}`,
		`{"mcp":{"proxy":null}}`, `{"mcp":{"proxy":{}}}`, `{"mcp":{"url":null}}`,
		`{"mcp":{"api_key":null}}`, `{"mcp":{"api_key":false}}`,
		`{"mcp":{"headers":null}}`, `{"mcp":{"headers":false}}`,
		`{"mcp":{"local":null}}`, `{"mcp":{"local":false}}`,
		`{"mcp":{"local":{"enable":null}}}`, `{"mcp":{"local":{"args":null}}}`,
		`{"mcp":{"local":{"unknown":true}}}`, `{"mcp":{"proxy":"x","unknown":true}}`,
		`{"unknown":true,"mcp":{"proxy":"x"}}`,
		`{"mcp":{"proxy":"x","servers":{"shared":{"url":"y"}}}}`,
		`{"mcp":{"local":{},"servers":{"local":{"command":"y"}}}}`,
		`{"mcp":{"proxy":"x","url":"y"}}`, `{"mcp":{"proxy":"x","servers":null}}`,
		`{"mcp":{"proxy":"x","servers":{"other":{"unknown":true}}}}`,
		`{"mcp":{"local":{"enable":true,"command":""}}}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			path := writeConfig(t, body)
			if _, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: path}); err == nil {
				t.Fatal("invalid config accepted")
			}
			if backup, changed, err := MigrateFile(path); err == nil || backup != "" || changed {
				t.Fatalf("backup=%q changed=%v err=%v", backup, changed, err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != body {
				t.Fatalf("original changed: %v", err)
			}
			if _, err := os.Stat(path + ".pre-mcp-migration.bak"); !os.IsNotExist(err) {
				t.Fatalf("backup unexpectedly created: %v", err)
			}
		})
	}
}

func TestMigrateFileBackupAndIdempotence(t *testing.T) {
	original := []byte(" {\n \"mcp\": {\"proxy\": \"${MISSING}\", \"api_key\": \"saved-secret\"}, \"model\": \"test\"\n}\n")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	backup, changed, err := MigrateFile(path)
	if err != nil || !changed || backup != path+".pre-mcp-migration.bak" {
		t.Fatalf("backup=%q changed=%v err=%v", backup, changed, err)
	}
	saved, err := os.ReadFile(backup)
	if err != nil || !bytes.Equal(saved, original) {
		t.Fatalf("backup mismatch: %v", err)
	}
	for _, name := range []string{path, backup} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%o", name, info.Mode().Perm())
		}
	}
	fc, err := decodeConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	shared := fc.MCP.Value.Servers.Value["shared"]
	if shared.IsEnabled() || shared.URL != "${MISSING}" || shared.Headers["Authorization"] != "Bearer saved-secret" {
		t.Fatalf("shared=%+v", shared)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if backup, changed, err := MigrateFile(path); err != nil || changed || backup != "" {
		t.Fatalf("second migration: %q %v %v", backup, changed, err)
	}
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(again, migrated) {
		t.Fatalf("idempotence: %v", err)
	}
}

func TestMigrateFileDoesNotClobberBackup(t *testing.T) {
	path := writeConfig(t, `{"mcp":{"proxy":"saved"}}`)
	backup := path + ".pre-mcp-migration.bak"
	if err := os.WriteFile(backup, []byte("existing backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := MigrateFile(path); err == nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("original changed: %v", err)
	}
	data, err = os.ReadFile(backup)
	if err != nil || string(data) != "existing backup" {
		t.Fatalf("backup clobbered: %v", err)
	}
}

func TestMigrateFileValidatesWholeConfigBeforeWriting(t *testing.T) {
	for _, body := range []string{`{"color_theme":"bogus","mcp":{"proxy":"x"}}`, `{"color_theme":"bogus"}`} {
		path := writeConfig(t, body)
		if _, changed, err := MigrateFile(path); err == nil || changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
	}
}

// Ensure Enabled remains optional when cloning new-format definitions.
func TestCloneMCPServersOmittedEnabled(t *testing.T) {
	cloned := cloneMCPServers(map[string]mcpproxy.ServerConfig{"test": {Command: "run"}})
	if cloned["test"].Enabled != nil {
		t.Fatal("omitted enabled changed")
	}
}

func TestLegacyMCPDefaultURL(t *testing.T) {
	path := writeConfig(t, `{"mcp":{"enable":true}}`)
	result := load(t, nil, nil, path)
	if got := result.Config.MCP.Servers["shared"].URL; got != mcpproxy.DefaultURL() {
		t.Fatalf("url=%q", got)
	}
	if _, changed, err := MigrateFile(path); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}

func TestLegacyMCPProjectLeafMerge(t *testing.T) {
	for _, project := range []string{
		`{"mcp":{"headers":{"X-Project":"yes"},"local":{"env":{"PROJECT":"yes"}}}}`,
		`{"mcp":{"headers":{"X-Project":"yes"},"local":{"args":["project"]}}}`,
	} {
		t.Run(project, func(t *testing.T) {
			global := writeConfig(t, `{"mcp":{"enable":true,"proxy":"https://example.test/mcp","api_key":"secret","headers":{"X-Global":"yes"},"local":{"enable":true,"command":"runner","args":["global"],"env":{"GLOBAL":"yes"}}}}`)
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".harness"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".harness", "config.json")
			if err := os.WriteFile(path, []byte(project), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: global, WorkingDir: root})
			if err != nil {
				t.Fatal(err)
			}
			shared, local := result.Config.MCP.Servers["shared"], result.Config.MCP.Servers["local"]
			if !shared.IsEnabled() || shared.URL != "https://example.test/mcp" || shared.Headers["Authorization"] != "Bearer secret" || shared.Headers["X-Project"] != "yes" || shared.Headers["X-Global"] != "" {
				t.Fatalf("shared=%+v", shared)
			}
			if got := result.Config.MCP.ServerConfigDirs["local"]; got != filepath.Dir(global) {
				t.Fatalf("local directory=%q", got)
			}
			if !local.IsEnabled() || local.Command != "runner" {
				t.Fatalf("local=%+v", local)
			}
			if local.Env["PROJECT"] == "yes" {
				if len(local.Env) != 1 || !reflect.DeepEqual(local.Args, []string{"global"}) {
					t.Fatalf("local=%+v", local)
				}
			} else if !reflect.DeepEqual(local.Args, []string{"project"}) || local.Env["GLOBAL"] != "yes" {
				t.Fatalf("local=%+v", local)
			}
			if _, changed, err := MigrateFile(path); err == nil || changed {
				t.Fatalf("project migration changed=%v err=%v", changed, err)
			}
			data, _ := os.ReadFile(path)
			if string(data) != project {
				t.Fatal("project changed")
			}
		})
	}
}

func TestMigrateFileRejectsMalformedHooksBeforeWrites(t *testing.T) {
	for _, hooks := range []string{`[]`, `{"bogus":[]}`, `{"PreToolUse":"bad"}`} {
		path := writeConfig(t, `{"mcp":{"enable":true},"hooks":`+hooks+`}`)
		original, _ := os.ReadFile(path)
		if _, changed, err := MigrateFile(path); err == nil || changed {
			t.Fatalf("hooks=%s changed=%v err=%v", hooks, changed, err)
		}
		data, _ := os.ReadFile(path)
		if !bytes.Equal(data, original) {
			t.Fatal("source changed")
		}
		if _, err := os.Stat(path + ".pre-mcp-migration.bak"); !os.IsNotExist(err) {
			t.Fatalf("backup exists: %v", err)
		}
	}
}

func TestMigrateFileRejectsSymlink(t *testing.T) {
	target := writeConfig(t, `{"mcp":{"enable":true}}`)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := MigrateFile(path); err == nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v", err)
	}
}

func TestMigrateFileRejectsConcurrentChange(t *testing.T) {
	for _, kind := range []string{"bytes", "identity", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			path := writeConfig(t, `{"mcp":{"enable":true}}`)
			original, _ := os.ReadFile(path)
			concurrent := []byte(`{"model":"concurrent"}`)
			_, changed, err := migrateFile(path, func() {
				switch kind {
				case "bytes":
					if e := os.WriteFile(path, concurrent, 0600); e != nil {
						t.Fatal(e)
					}
				case "identity":
					replacement := path + ".replacement"
					if e := os.WriteFile(replacement, original, 0600); e != nil {
						t.Fatal(e)
					}
					if e := os.Rename(replacement, path); e != nil {
						t.Fatal(e)
					}
				case "symlink":
					target := path + ".target"
					if e := os.WriteFile(target, concurrent, 0600); e != nil {
						t.Fatal(e)
					}
					if e := os.Remove(path); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink(target, path); e != nil {
						t.Fatal(e)
					}
				}
			})
			if err == nil || changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			data, _ := os.ReadFile(path)
			want := concurrent
			if kind == "identity" {
				want = original
			}
			if !bytes.Equal(data, want) {
				t.Fatal("concurrent content lost")
			}
			backup, _ := os.ReadFile(path + ".pre-mcp-migration.bak")
			if !bytes.Equal(backup, original) {
				t.Fatal("backup differs")
			}
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-migration-*.tmp"))
			if len(matches) != 0 {
				t.Fatal("temporary file leaked")
			}
		})
	}
}

func TestLegacyMCPMergeExplicitZeroAndURLAlias(t *testing.T) {
	global, _, _, err := decodeConfigData([]byte(`{"mcp":{"enable":true,"proxy":"https://global.test","api_key":"global","local":{"enable":true,"command":"runner"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	project, _, _, err := decodeConfigData([]byte(`{"mcp":{"enable":false,"url":"https://project.test","api_key":"","local":{"enable":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	merged, _ := mergeFileConfig(global, project, "global.json", "project.json")
	shared, local := merged.MCP.Value.Servers.Value["shared"], merged.MCP.Value.Servers.Value["local"]
	if shared.IsEnabled() || shared.URL != "https://project.test" || shared.Headers["Authorization"] != "" {
		t.Fatalf("shared=%+v", shared)
	}
	if local.IsEnabled() || local.Command != "runner" {
		t.Fatalf("local=%+v", local)
	}
}
