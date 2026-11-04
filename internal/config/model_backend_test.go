package config

import (
	"os"
	"path/filepath"
	"testing"

	"harness/internal/configmeta"
)

func TestModelBackendSettingsPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, body, mode, path string
		env                    map[string]string
		args                   []string
		kind                   configmeta.SourceKind
	}{
		{name: "defaults", body: `{}`, mode: "remote", kind: configmeta.SourceDefault},
		{name: "file", body: `{"model_backend":"local","model_config":"models.json"}`, mode: "local", path: "file", kind: configmeta.SourceFile},
		{name: "environment", body: `{"model_backend":"local","model_config":"models.json"}`, env: map[string]string{"HARNESS_MODEL_BACKEND": "remote", "HARNESS_MODEL_CONFIG": "env.json"}, mode: "remote", path: "env.json", kind: configmeta.SourceEnvironment},
		{name: "flags", body: `{"model_backend":"remote","model_config":"models.json"}`, env: map[string]string{"HARNESS_MODEL_BACKEND": "remote", "HARNESS_MODEL_CONFIG": "env.json"}, args: []string{"-model-backend", "local", "-model-config", "flag.json"}, mode: "local", path: "flag.json", kind: configmeta.SourceFlag},
		{name: "last flag", body: `{}`, args: []string{"-model-backend", "local", "-model-backend", "remote", "-model-config", "first.json", "-model-config", "last.json"}, mode: "remote", path: "last.json", kind: configmeta.SourceFlag},
		{name: "clear local path", body: `{"model_config":"models.json"}`, args: []string{"-model-config", ""}, mode: "remote", path: "", kind: configmeta.SourceDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			result := load(t, tc.args, tc.env, path)
			wantPath := tc.path
			if wantPath == "file" {
				wantPath = filepath.Join(filepath.Dir(path), "models.json")
			}
			if result.Config.ModelBackend != tc.mode || result.Config.ModelConfig != wantPath {
				t.Fatalf("settings mode=%q path=%q, want %q %q", result.Config.ModelBackend, result.Config.ModelConfig, tc.mode, wantPath)
			}
			if result.Sources["model_backend"].Kind != tc.kind {
				t.Fatalf("backend source=%+v want %v", result.Sources["model_backend"], tc.kind)
			}
		})
	}
}

func TestModelBackendRejectsInvalidCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		env        map[string]string
		args       []string
	}{
		{name: "unknown file mode", body: `{"model_backend":"auto"}`, args: []string{"-model-backend", "local"}},
		{name: "empty file mode", body: `{"model_backend":""}`},
		{name: "null mode", body: `{"model_backend":null}`},
		{name: "null config", body: `{"model_config":null}`},
		{name: "invalid environment", body: `{}`, env: map[string]string{"HARNESS_MODEL_BACKEND": "fallback"}, args: []string{"-model-backend", "local"}},
		{name: "invalid flag", body: `{}`, args: []string{"-model-backend", "auto"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(LoadOptions{Args: tc.args, LookupEnv: lookup(tc.env), DefaultConfigPath: writeConfig(t, tc.body)})
			if err == nil {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestProjectModelBackendConfigUsesOwningDirectory(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".harness"), 0700); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(project, ".harness", "config.json")
	if err := os.WriteFile(projectPath, []byte(`{"model_backend":"local","model_config":"models.json"}`), 0600); err != nil {
		t.Fatal(err)
	}
	global := writeConfig(t, `{"model_backend":"remote","model_config":"global.json"}`)
	result, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: global, WorkingDir: project})
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.ModelBackend != "local" || result.Config.ModelConfig != filepath.Join(project, ".harness", "models.json") {
		t.Fatalf("config=%+v", result.Config)
	}
	for _, key := range []string{"model_backend", "model_config"} {
		if result.Sources[key].Name != projectPath {
			t.Fatalf("%s source=%+v", key, result.Sources[key])
		}
	}
}
