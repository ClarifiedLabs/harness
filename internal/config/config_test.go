package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"harness/internal/configmeta"
)

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, args []string, env map[string]string, path string) Result {
	t.Helper()
	result, err := Load(LoadOptions{Args: args, LookupEnv: lookup(env), DefaultConfigPath: path, Defaults: RuntimeDefaults{ModelProxyURL: "http://model", MCPProxyURL: "http://mcp", HistoryPath: "/state/history", Agent: "auto"}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return result
}

func TestCatalogAndGeneratedFlags(t *testing.T) {
	catalog := Catalog()
	if catalog.Len() < 70 {
		t.Fatalf("catalog has %d parameters, want comprehensive catalog", catalog.Len())
	}
	for _, parameter := range catalog.Parameters() {
		if parameter.Key == "" || parameter.Type == "" || parameter.Description == "" {
			t.Fatalf("incomplete parameter: %+v", parameter)
		}
		for _, name := range parameter.Flags {
			state := newFlagState()
			if state.set.Lookup(name) == nil {
				t.Errorf("catalog flag %q is not generated", name)
			}
		}
	}
	noColor, ok := catalog.Lookup("no_color")
	if !ok || len(noColor.Environment) != 2 || noColor.Environment[1] != "NO_COLOR" {
		t.Fatalf("no_color metadata = %+v", noColor)
	}
	secret, _ := catalog.Lookup("model_proxy_api_key")
	if !secret.Sensitive {
		t.Fatal("model proxy API key not marked sensitive")
	}
}

func TestGeneratedUsageIncludesConfigDefaultsAndEnvironment(t *testing.T) {
	state := newFlagState()
	for _, name := range []string{"h", "help", "version"} {
		if state.set.Lookup(name) == nil {
			t.Errorf("root meta flag %q is not generated", name)
		}
	}
	for _, parameter := range Catalog().Parameters() {
		for _, name := range parameter.Flags {
			settingFlag := state.set.Lookup(name)
			if settingFlag == nil {
				t.Fatalf("catalog flag %q is not generated", name)
			}
			if want := configmeta.FormatDefault(parameter.Default); settingFlag.DefValue != want {
				t.Errorf("flag %q default = %q, want %q", name, settingFlag.DefValue, want)
			}
			for _, environment := range parameter.Environment {
				if !strings.Contains(settingFlag.Usage, environment) {
					t.Errorf("flag %q usage %q does not name environment variable %q", name, settingFlag.Usage, environment)
				}
			}
		}
	}

	var usage bytes.Buffer
	Usage(&usage)
	for _, want := range []string{
		"-h\tshow help and exit", "-help\n", "-version\n",
		"Harness model setting. (env: HARNESS_MODEL) (default unset (provider/model selected elsewhere))",
		"Harness max turns setting. (env: HARNESS_MAX_TURNS) (default 0 (non-positive means unlimited))",
		"Harness no color setting. (env: HARNESS_NO_COLOR, NO_COLOR) (default false (NO_COLOR is a presence-based override))",
		"Harness responses stateful setting. (env: HARNESS_RESPONSES_STATEFUL) (default true)",
		"Harness model proxy url setting. (env: HARNESS_MODEL_PROXY_URL) (default derived: runtime model proxy URL)",
	} {
		if !strings.Contains(usage.String(), want) {
			t.Errorf("usage does not contain %q:\n%s", want, usage.String())
		}
	}
}

func TestDefaultToolTimeout(t *testing.T) {
	result := load(t, nil, nil, filepath.Join(t.TempDir(), "missing.json"))
	if got := result.Config.ToolTimeoutSeconds; got != 1800 {
		t.Fatalf("ToolTimeoutSeconds = %d, want 1800", got)
	}
}

func TestRootMetaFlagsShortCircuitConfigResolution(t *testing.T) {
	invalidConfig := writeConfig(t, `{"unknown_setting":true}`)
	tests := []struct {
		arg         string
		wantHelp    bool
		wantVersion bool
	}{
		{arg: "-h", wantHelp: true},
		{arg: "--help", wantHelp: true},
		{arg: "-version", wantVersion: true},
		{arg: "--version", wantVersion: true},
	}
	for _, test := range tests {
		t.Run(test.arg, func(t *testing.T) {
			result, err := Load(LoadOptions{
				Args:              []string{test.arg},
				LookupEnv:         lookup(nil),
				DefaultConfigPath: invalidConfig,
			})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if result.Run.Help != test.wantHelp || result.Run.Version != test.wantVersion {
				t.Fatalf("run options = %+v, want Help=%t Version=%t", result.Run, test.wantHelp, test.wantVersion)
			}
		})
	}
}

func TestGeneratedUsageParameterTableIsCurrent(t *testing.T) {
	usagePath := filepath.Join("..", "..", "docs", "usage.md")
	usage, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	const startMarker = "<!-- harness-config-parameters:start -->"
	const endMarker = "<!-- harness-config-parameters:end -->"
	start := bytes.Index(usage, []byte(startMarker))
	end := bytes.Index(usage, []byte(endMarker))
	if start < 0 || end < 0 || end < start {
		t.Fatalf("%s is missing ordered config reference markers", usagePath)
	}
	end += len(endMarker)

	var generated bytes.Buffer
	generated.WriteString(startMarker + "\n")
	if err := configmeta.WriteMarkdown(&generated, Catalog()); err != nil {
		t.Fatal(err)
	}
	generated.WriteString(endMarker)
	if documented := string(usage[start:end]); documented != generated.String() {
		t.Fatalf("generated config reference in %s is stale; regenerate it with `harness config list -format markdown`", usagePath)
	}
}

func TestExampleConfigStrictlyLoads(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "harness", "config.json")
	result, err := Load(LoadOptions{
		LookupEnv:         lookup(nil),
		DefaultConfigPath: path,
		Defaults: RuntimeDefaults{
			ModelProxyURL: "http://127.0.0.1:8765",
			MCPProxyURL:   "http://127.0.0.1:8766",
			HistoryPath:   "/tmp/harness-history",
			Agent:         "auto",
		},
	})
	if err != nil {
		t.Fatalf("strict-load %s: %v", path, err)
	}
	if result.ConfigPath != path || result.Config.MaxTurns != 0 {
		t.Fatalf("example result = %+v", result)
	}
}

func TestCatalogCoversEveryConfigFileField(t *testing.T) {
	var filePaths []string
	collectJSONPaths(reflect.TypeOf(fileConfig{}), "", &filePaths)
	sort.Strings(filePaths)
	var catalogPaths []string
	for _, parameter := range Catalog().Parameters() {
		if parameter.JSONPath != "" {
			catalogPaths = append(catalogPaths, parameter.JSONPath)
		}
	}
	sort.Strings(catalogPaths)
	if !reflect.DeepEqual(filePaths, catalogPaths) {
		t.Fatalf("file fields and catalog paths differ\nfile:    %v\ncatalog: %v", filePaths, catalogPaths)
	}
}

func collectJSONPaths(typ reflect.Type, prefix string, paths *[]string) {
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		valueType := field.Type
		if valueField, ok := valueType.FieldByName("Value"); ok {
			valueType = valueField.Type
		}
		if valueType.Kind() == reflect.Struct && valueType.PkgPath() == reflect.TypeOf(fileConfig{}).PkgPath() {
			collectJSONPaths(valueType, path, paths)
			continue
		}
		*paths = append(*paths, path)
	}
}

func TestPrecedenceAndExactProvenance(t *testing.T) {
	path := writeConfig(t, `{"max_turns":3}`)
	result := load(t, []string{"--max-turns=9", "--max-turns=11"}, map[string]string{"HARNESS_MAX_TURNS": "7"}, path)
	if result.Config.MaxTurns != 11 {
		t.Fatalf("MaxTurns=%d", result.Config.MaxTurns)
	}
	if got := result.Sources["max_turns"]; got != (configmeta.Source{Kind: configmeta.SourceFlag, Name: "--max-turns"}) {
		t.Fatalf("source=%+v", got)
	}

	result = load(t, nil, map[string]string{"HARNESS_MAX_TURNS": "7"}, path)
	if result.Config.MaxTurns != 7 || result.Sources["max_turns"].Name != "HARNESS_MAX_TURNS" {
		t.Fatalf("env result=%+v", result)
	}
	result = load(t, nil, nil, path)
	if result.Config.MaxTurns != 3 || result.Sources["max_turns"] != (configmeta.Source{Kind: configmeta.SourceFile, Name: path}) {
		t.Fatalf("file source=%+v", result.Sources["max_turns"])
	}
}

func TestReadLimitsPrecedence(t *testing.T) {
	globalPath := writeConfig(t, `{"read_default_limit":10,"read_total_lines_max_bytes":10000,"read_result_max_bytes":100,"read_result_max_lines":1000}`)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(root, ".harness")
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(projectDir, "config.json")
	if err := os.WriteFile(projectPath, []byte(`{"read_default_limit":20,"read_total_lines_max_bytes":20000,"read_result_max_bytes":200,"read_result_max_lines":2000}`), 0o644); err != nil {
		t.Fatal(err)
	}

	options := LoadOptions{
		LookupEnv:         lookup(nil),
		DefaultConfigPath: globalPath,
		WorkingDir:        root,
	}
	result, err := Load(options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.ReadDefaultLimit != 20 || result.Config.ReadTotalLinesMaxBytes != 20000 || result.Config.ReadResultMaxBytes != 200 || result.Config.ReadResultMaxLines != 2000 {
		t.Fatalf("project read limits = %d/%d/%d/%d, want 20/20000/200/2000", result.Config.ReadDefaultLimit, result.Config.ReadTotalLinesMaxBytes, result.Config.ReadResultMaxBytes, result.Config.ReadResultMaxLines)
	}
	for _, key := range []string{"read_default_limit", "read_total_lines_max_bytes", "read_result_max_bytes", "read_result_max_lines"} {
		if got := result.Sources[key]; got != (configmeta.Source{Kind: configmeta.SourceFile, Name: projectPath}) {
			t.Errorf("%s source = %+v, want project file", key, got)
		}
	}

	options.LookupEnv = lookup(map[string]string{
		"HARNESS_READ_DEFAULT_LIMIT":         "30",
		"HARNESS_READ_TOTAL_LINES_MAX_BYTES": "30000",
		"HARNESS_READ_RESULT_MAX_BYTES":      "300",
		"HARNESS_READ_RESULT_MAX_LINES":      "3000",
	})
	result, err = Load(options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.ReadDefaultLimit != 30 || result.Config.ReadTotalLinesMaxBytes != 30000 || result.Config.ReadResultMaxBytes != 300 || result.Config.ReadResultMaxLines != 3000 {
		t.Fatalf("environment read limits = %d/%d/%d/%d, want 30/30000/300/3000", result.Config.ReadDefaultLimit, result.Config.ReadTotalLinesMaxBytes, result.Config.ReadResultMaxBytes, result.Config.ReadResultMaxLines)
	}
	for key, name := range map[string]string{
		"read_default_limit":         "HARNESS_READ_DEFAULT_LIMIT",
		"read_total_lines_max_bytes": "HARNESS_READ_TOTAL_LINES_MAX_BYTES",
		"read_result_max_bytes":      "HARNESS_READ_RESULT_MAX_BYTES",
		"read_result_max_lines":      "HARNESS_READ_RESULT_MAX_LINES",
	} {
		if got := result.Sources[key]; got != (configmeta.Source{Kind: configmeta.SourceEnvironment, Name: name}) {
			t.Errorf("%s source = %+v, want environment %s", key, got, name)
		}
	}
}

func TestRemovedParametersAreUnavailable(t *testing.T) {
	oldKeys := []string{
		"trajectory_context",
		"read_file_default_limit",
		"read_file_result_max_bytes",
		"read_file_result_max_lines",
		"rg_result_max_bytes",
		"rg_result_max_lines",
		"grep_result_max_bytes",
		"grep_result_max_lines",
	}
	for _, key := range oldKeys {
		if _, ok := Catalog().Lookup(key); ok {
			t.Errorf("obsolete parameter %q remains in catalog", key)
		}
		path := writeConfig(t, `{"`+key+`":1}`)
		if _, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: path}); err == nil || !strings.Contains(err.Error(), `unknown field "`+key+`"`) {
			t.Errorf("obsolete file setting %q error = %v, want strict unknown-field rejection", key, err)
		}
	}

	result := load(t, nil, map[string]string{
		"HARNESS_READ_FILE_DEFAULT_LIMIT":    "1",
		"HARNESS_READ_FILE_RESULT_MAX_BYTES": "2",
		"HARNESS_READ_FILE_RESULT_MAX_LINES": "3",
		"HARNESS_RG_RESULT_MAX_BYTES":        "4",
		"HARNESS_RG_RESULT_MAX_LINES":        "5",
		"HARNESS_GREP_RESULT_MAX_BYTES":      "6",
		"HARNESS_GREP_RESULT_MAX_LINES":      "7",
	}, "")
	if result.Config.ReadDefaultLimit != 0 || result.Config.ReadTotalLinesMaxBytes != 0 || result.Config.ReadResultMaxBytes != 0 || result.Config.ReadResultMaxLines != 0 {
		t.Fatalf("obsolete environment variables affected read limits: %+v", result.Config)
	}
}

func TestOTelHeadersPrecedenceExpansionAndRedaction(t *testing.T) {
	path := writeConfig(t, `{"otel":{"headers":{"authorization":"file-${TOKEN}"}}}`)
	tests := []struct {
		name       string
		env        map[string]string
		wantValue  string
		wantSource configmeta.Source
	}{
		{
			name:       "file",
			env:        map[string]string{"TOKEN": "secret"},
			wantValue:  "file-secret",
			wantSource: configmeta.Source{Kind: configmeta.SourceFile, Name: path},
		},
		{
			name:       "standard environment overrides file",
			env:        map[string]string{"TOKEN": "secret", "OTEL_EXPORTER_OTLP_HEADERS": "authorization=standard-${TOKEN}"},
			wantValue:  "standard-secret",
			wantSource: configmeta.Source{Kind: configmeta.SourceEnvironment, Name: "OTEL_EXPORTER_OTLP_HEADERS"},
		},
		{
			name: "harness environment overrides standard environment and file",
			env: map[string]string{
				"TOKEN":                      "secret",
				"OTEL_EXPORTER_OTLP_HEADERS": "authorization=standard-${TOKEN}",
				"HARNESS_OTEL_HEADERS":       "authorization=harness-${TOKEN}",
			},
			wantValue:  "harness-secret",
			wantSource: configmeta.Source{Kind: configmeta.SourceEnvironment, Name: "HARNESS_OTEL_HEADERS"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := load(t, nil, test.env, path)
			if got := result.Config.OTel.Headers["authorization"]; got != test.wantValue {
				t.Fatalf("authorization header = %q, want %q", got, test.wantValue)
			}
			if got := result.Sources["otel.headers"]; got != test.wantSource {
				t.Fatalf("source = %+v, want %+v", got, test.wantSource)
			}
			projected, ok := Snapshot(result).Values["otel.headers"].(map[string]string)
			if !ok || projected["authorization"] != redactedValue {
				t.Fatalf("projected headers = %#v, want redacted authorization", projected)
			}
		})
	}
}

func TestOTelHostnameSetDistinguishesDefaultFromExplicitEmpty(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		path       func(*testing.T) string
		wantSet    bool
		wantSource configmeta.SourceKind
	}{
		{name: "default", wantSource: configmeta.SourceDefault},
		{name: "file empty", path: func(t *testing.T) string { return writeConfig(t, `{"otel":{"hostname":""}}`) }, wantSet: true, wantSource: configmeta.SourceFile},
		{name: "environment empty", env: map[string]string{"HARNESS_OTEL_HOSTNAME": ""}, wantSet: true, wantSource: configmeta.SourceEnvironment},
		{name: "flag empty", args: []string{"--otel-hostname="}, wantSet: true, wantSource: configmeta.SourceFlag},
	}
	var defaultProjection []byte
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := ""
			if test.path != nil {
				path = test.path(t)
			}
			result := load(t, test.args, test.env, path)
			if result.Config.OTel.Hostname != "" || result.Config.OTel.HostnameSet != test.wantSet {
				t.Fatalf("hostname = %q, HostnameSet = %t, want empty/%t", result.Config.OTel.Hostname, result.Config.OTel.HostnameSet, test.wantSet)
			}
			if got := result.Sources["otel.hostname"].Kind; got != test.wantSource {
				t.Fatalf("source kind = %v, want %v", got, test.wantSource)
			}
			projection, err := json.Marshal(Project(result, false))
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "default" {
				defaultProjection = projection
			} else if !bytes.Equal(projection, defaultProjection) {
				t.Fatalf("explicit marker changed config projection\ndefault:  %s\nexplicit: %s", defaultProjection, projection)
			}
			encoded, err := json.Marshal(result.Config.OTel)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "HostnameSet") || strings.Contains(string(encoded), "hostname_set") {
				t.Fatalf("HostnameSet was serialized: %s", encoded)
			}
		})
	}
}

func TestEmptyStandardOTelEndpointDoesNotOverrideFile(t *testing.T) {
	path := writeConfig(t, `{"otel":{"endpoint":"https://collector.example/v1"}}`)
	result := load(t, nil, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ""}, path)
	if result.Config.OTel.Endpoint != "https://collector.example/v1" {
		t.Fatalf("endpoint = %q, want file endpoint", result.Config.OTel.Endpoint)
	}
	if got := result.Sources["otel.endpoint"]; got != (configmeta.Source{Kind: configmeta.SourceFile, Name: path}) {
		t.Fatalf("source = %+v, want file %q", got, path)
	}
}

func TestNOColorUsesPresenceSemantics(t *testing.T) {
	result := load(t, nil, map[string]string{"NO_COLOR": ""}, "")
	if !result.Config.NoColor || result.Sources["no_color"] != (configmeta.Source{Kind: configmeta.SourceEnvironment, Name: "NO_COLOR"}) {
		t.Fatalf("NO_COLOR result = %+v, source = %+v", result.Config.NoColor, result.Sources["no_color"])
	}
}

func TestEveryPresentSourceIsValidated(t *testing.T) {
	path := writeConfig(t, `{"max_turns":4}`)
	_, err := Load(LoadOptions{Args: []string{"--max-turns=8"}, LookupEnv: lookup(map[string]string{"HARNESS_MAX_TURNS": "bad"}), DefaultConfigPath: path})
	if err == nil || !strings.Contains(err.Error(), "HARNESS_MAX_TURNS") {
		t.Fatalf("error=%v", err)
	}
	badFile := writeConfig(t, `{"color_theme":"bogus"}`)
	_, err = Load(LoadOptions{Args: []string{"--color-theme=dark"}, LookupEnv: lookup(nil), DefaultConfigPath: badFile})
	if err == nil || !strings.Contains(err.Error(), "color_theme") {
		t.Fatalf("error=%v", err)
	}

	badHooks := writeConfig(t, `{"hooks":[]}`)
	_, err = Load(LoadOptions{Args: []string{"--hooks", filepath.Join(t.TempDir(), "override.json")}, LookupEnv: lookup(nil), DefaultConfigPath: badHooks})
	if err == nil || !strings.Contains(err.Error(), "setting hooks") {
		t.Fatalf("hidden invalid hooks error=%v", err)
	}

	badLocal := writeConfig(t, `{"mcp":{"local":{"enable":true,"command":""}}}`)
	_, err = Load(LoadOptions{LookupEnv: lookup(map[string]string{"HARNESS_MCP_LOCAL_ENABLE": "false"}), DefaultConfigPath: badLocal})
	if err == nil || !strings.Contains(err.Error(), "mcp.local.command") {
		t.Fatalf("hidden invalid local MCP error=%v", err)
	}
}

func TestReasoningStrictAliasesAndExplicitDefault(t *testing.T) {
	for _, value := range []string{"", "default", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		result := load(t, []string{"--reasoning=" + value}, nil, "")
		if value == "" || value == "default" {
			if result.Config.Reasoning != "" {
				t.Fatalf("reasoning %q resolved to %q, want provider default", value, result.Config.Reasoning)
			}
		}
	}
	for _, alias := range []string{"off", "false", "disabled", "provider-default", "minimum", "min"} {
		if _, err := Load(LoadOptions{Args: []string{"--reasoning=" + alias}, LookupEnv: lookup(nil)}); err == nil {
			t.Fatalf("accepted removed reasoning alias %q", alias)
		}
	}
}

func TestRequiredPathRejectsExplicitEmpty(t *testing.T) {
	_, err := Load(LoadOptions{LookupEnv: lookup(map[string]string{"HARNESS_HISTFILE": ""})})
	if err == nil || !strings.Contains(err.Error(), "HARNESS_HISTFILE") {
		t.Fatalf("empty history path error = %v", err)
	}
}

func TestExplicitEmptySourceSemantics(t *testing.T) {
	path := writeConfig(t, `{"model_proxy_api_key":"file-secret","color_theme":"light"}`)
	result := load(t, []string{"--model-proxy-api-key="}, nil, path)
	if result.Config.ModelProxyAPIKey != "" || result.Sources["model_proxy_api_key"].Kind != configmeta.SourceFlag {
		t.Fatalf("API key=%q source=%+v", result.Config.ModelProxyAPIKey, result.Sources["model_proxy_api_key"])
	}
	_, err := Load(LoadOptions{LookupEnv: lookup(map[string]string{"HARNESS_COLOR_THEME": ""}), DefaultConfigPath: path})
	if err == nil || !strings.Contains(err.Error(), "HARNESS_COLOR_THEME") {
		t.Fatalf("empty enum error=%v", err)
	}
}

func TestRuntimeDefaultsAreDerived(t *testing.T) {
	result := load(t, nil, nil, "")
	checks := map[string]string{"model_proxy_url": "http://model", "mcp.proxy": "http://mcp", "histfile": "/state/history", "agent": "auto", "handoff_agent": "auto"}
	for key, want := range checks {
		if result.Sources[key].Kind != configmeta.SourceDerived {
			t.Errorf("%s source=%+v", key, result.Sources[key])
		}
		_ = want
	}
	if result.Config.ModelProxyURL != "http://model" || result.Config.MCP.Proxy != "http://mcp" || result.Config.HistFile != "/state/history" || result.Config.Agent != "auto" || result.Config.HandoffAgent != "auto" {
		t.Fatalf("defaults=%+v", result.Config)
	}
	result, err := Load(LoadOptions{LookupEnv: lookup(nil), Defaults: RuntimeDefaults{TmuxActive: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Config.DelegateTmux || result.Sources["delegate_tmux"].Kind != configmeta.SourceDerived {
		t.Fatalf("tmux=%v source=%+v", result.Config.DelegateTmux, result.Sources["delegate_tmux"])
	}
}

func TestConfigPathResolution(t *testing.T) {
	conventional := writeConfig(t, `{"max_turns":1}`)
	envPath := writeConfig(t, `{"max_turns":2}`)
	flagPath := writeConfig(t, `{"max_turns":3}`)
	result, err := Load(LoadOptions{Args: []string{"--config", flagPath}, LookupEnv: lookup(map[string]string{"HARNESS_CONFIG": envPath}), DefaultConfigPath: conventional})
	if err != nil {
		t.Fatal(err)
	}
	if result.ConfigPath != flagPath || result.Config.MaxTurns != 3 {
		t.Fatalf("path=%q max=%d", result.ConfigPath, result.Config.MaxTurns)
	}
	result, err = Load(LoadOptions{LookupEnv: lookup(map[string]string{"HARNESS_CONFIG": envPath}), DefaultConfigPath: conventional})
	if err != nil {
		t.Fatal(err)
	}
	if result.ConfigPath != envPath {
		t.Fatalf("path=%q", result.ConfigPath)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	result, err = Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: missing})
	if err != nil || result.ConfigPath != "" {
		t.Fatalf("implicit missing: result=%+v err=%v", result, err)
	}
	for _, options := range []LoadOptions{{Args: []string{"--config="}, LookupEnv: lookup(nil)}, {LookupEnv: lookup(map[string]string{"HARNESS_CONFIG": ""})}, {Args: []string{"--config", missing}, LookupEnv: lookup(nil)}} {
		if _, err := Load(options); err == nil {
			t.Fatalf("options %+v accepted", options)
		}
	}
}

func TestStrictJSONDecoder(t *testing.T) {
	cases := []string{
		`[]`, `null`, `{"unknown":1}`, `{"max_turns":null}`, `{"mcp":null}`,
		`{"mcp":{"future":true}}`, `{"agents":{"x":{"future":true}}}`,
		`{"lsp":{"servers":{"go":{"future":true}}}}`, `{"max_turns":1} {"max_turns":2}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			_, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: writeConfig(t, body)})
			if err == nil {
				t.Fatal("accepted invalid JSON")
			}
		})
	}
}

func TestRelativeAtFileReferences(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(path, []byte(`{"system_prompt":"@../prompts/system.txt","agents":{"review":{"prompt":"@../prompts/review.txt"},"literal":{"prompt":"@@literal"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result := load(t, nil, nil, path)
	if result.Config.SystemPrompt != "@"+filepath.Join(dir, "prompts/system.txt") || result.Config.Agents["review"].Prompt != "@"+filepath.Join(dir, "prompts/review.txt") || result.Config.Agents["literal"].Prompt != "@@literal" {
		t.Fatalf("normalized=%+v", result.Config)
	}
}

func TestRelativeConfigPathAbsolutizesNestedReferencesOnce(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "nested")
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"prompt.md":  "verify first\n",
		"hooks.json": `{"Stop":[{"hooks":[{"command":"true"}]}]}`,
		"config.json": `{
			"agents":{"verified":{"description":"Use for verified work.","prompt":"@prompt.md"}},
			"hook_configs":["hooks.json"]
		}`,
	} {
		if err := os.WriteFile(filepath.Join(configDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeConfigPath, err := filepath.Rel(workingDir, filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	absConfigPath, err := filepath.Abs(relativeConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: relativeConfigPath})
	if err != nil {
		t.Fatalf("load relative config: %v", err)
	}
	wantPrompt := "@" + filepath.Join(filepath.Dir(absConfigPath), "prompt.md")
	wantHooks := filepath.Join(filepath.Dir(absConfigPath), "hooks.json")
	if result.Config.Agents["verified"].Prompt != wantPrompt ||
		len(result.Config.HookConfigs) != 1 || result.Config.HookConfigs[0] != wantHooks ||
		result.Config.Hooks.Empty() {
		t.Fatalf("resolved nested references: prompt=%q hooks=%v empty=%t", result.Config.Agents["verified"].Prompt, result.Config.HookConfigs, result.Config.Hooks.Empty())
	}
}

func TestRunOptionsAreSeparateAndRepeatable(t *testing.T) {
	result := load(t, []string{"-p", "hi", "--image", "low:a.png", "--image", "b.png", "--format", "json", "-q"}, nil, "")
	if !result.Run.PromptSet || result.Run.Prompt != "hi" || !result.Run.Quiet || result.Run.OutputFormat != "json" || len(result.Run.Images) != 2 || result.Run.Images[0].Detail != "low" || result.Run.Images[1].Detail != "auto" {
		t.Fatalf("Run=%+v", result.Run)
	}
	if _, err := Load(LoadOptions{Args: []string{"-p", "x", "-i", "y"}, LookupEnv: lookup(nil)}); err == nil {
		t.Fatal("accepted conflicting prompt flags")
	}
}

func TestStructuredSettingsAndInterpolation(t *testing.T) {
	path := writeConfig(t, `{
		"agents":{"review":{"description":"Review","prompt":"do it"}},
		"mcp":{"enable":true,"headers":{"Authorization":"Bearer ${TOKEN}"},"disabled_servers":["browser"],"local":{"enable":true,"command":"proxy","args":["serve"],"env":{"KEY":"${MISSING:-fallback}"}}},
		"lsp":{"enable":true,"tools":["definition"],"servers":{"go":{"languages":["go"],"root_markers":["go.mod"],"command":["gopls"],"extensions":[".go"],"env":{"SECRET":"value"},"initialization_options":{"x":true}}},"serena":{"enable":true,"command":"uvx","args":["serena"],"env":{"TOKEN":"${TOKEN}"}}}
	}`)
	result := load(t, nil, map[string]string{"TOKEN": "secret"}, path)
	if !result.Config.MCP.Enable || result.Config.MCP.Headers["Authorization"] != "Bearer secret" || result.Config.MCP.Local.Env["KEY"] != "fallback" || !result.Config.LSP.Enable || result.Config.LSP.Servers["go"].Command[0] != "gopls" || result.Config.LSP.Serena.Env["TOKEN"] != "secret" {
		t.Fatalf("structured=%+v", result.Config)
	}
	for _, key := range []string{"agents", "mcp.headers", "mcp.disabled_servers", "mcp.local.args", "mcp.local.env", "lsp.tools", "lsp.servers", "lsp.serena.args", "lsp.serena.env"} {
		if result.Sources[key].Kind != configmeta.SourceFile {
			t.Errorf("%s source=%+v", key, result.Sources[key])
		}
	}
}

func TestLSPPrewarmDefaultsAndOverride(t *testing.T) {
	// Default is true when nothing is configured.
	result := load(t, nil, nil, "")
	if !result.Config.LSP.Prewarm {
		t.Fatalf("lsp.prewarm default = false, want true")
	}
	// Config file can disable it.
	result = load(t, nil, nil, writeConfig(t, `{"lsp":{"prewarm":false}}`))
	if result.Config.LSP.Prewarm {
		t.Fatalf("lsp.prewarm from file = true, want false")
	}
	if got := result.Sources["lsp.prewarm"]; got.Kind != configmeta.SourceFile {
		t.Fatalf("lsp.prewarm source = %+v, want file", got)
	}
	// Env var overrides the file.
	result = load(t, nil, map[string]string{"HARNESS_LSP_PREWARM": "true"}, writeConfig(t, `{"lsp":{"prewarm":false}}`))
	if !result.Config.LSP.Prewarm {
		t.Fatalf("lsp.prewarm env override = false, want true")
	}
}

func TestFileAgentInteractiveSelectablePreservesOmissionAndExplicitFalse(t *testing.T) {
	result := load(t, nil, nil, writeConfig(t, `{
		"agents": {
			"defaulted": {"description": "Defaulted custom agent"},
			"hidden": {"description": "Delegated-only custom agent", "interactive_selectable": false}
		}
	}`))
	if got := result.Config.Agents["defaulted"].InteractiveSelectable; got != nil {
		t.Fatalf("omitted interactive_selectable = %v, want nil", *got)
	}
	hidden := result.Config.Agents["hidden"].InteractiveSelectable
	if hidden == nil || *hidden {
		t.Fatalf("explicit false interactive_selectable = %v, want false", hidden)
	}
}

func TestSnapshotDoesNotAliasResolvedConfig(t *testing.T) {
	result := load(t, nil, nil, writeConfig(t, `{"agents":{"custom":{"description":"Custom agent","allowed_tools":["read"]}}}`))
	snapshot := Snapshot(result)
	snapshot.Sources["agents"] = configmeta.Source{Kind: configmeta.SourceFlag, Name: "--changed"}
	snapshot.Values["agents"].(map[string]FileAgentConfig)["custom"] = FileAgentConfig{Description: "Changed"}

	if result.Sources["agents"].Kind != configmeta.SourceFile {
		t.Fatalf("resolved source changed through snapshot: %+v", result.Sources["agents"])
	}
	if got := result.Config.Agents["custom"].Description; got != "Custom agent" {
		t.Fatalf("resolved agents changed through snapshot: %q", got)
	}
}

func TestProjectionRedactsAndIsVersioned(t *testing.T) {
	path := writeConfig(t, `{"model_proxy_api_key":"model-secret-value","mcp":{"api_key":"mcp-secret-value","headers":{"Authorization":"header-secret-value"},"local":{"env":{"TOKEN":"local-env-secret-value"}}},"lsp":{"servers":{"go":{"languages":[],"root_markers":[],"command":["gopls"],"extensions":[],"env":{"TOKEN":"server-env-secret-value"},"initialization_options":{"apiToken":"lsp-init-secret-value"}}}}}`)
	result := load(t, nil, nil, path)
	projection := Project(result, true)
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"model-secret-value", "mcp-secret-value", "header-secret-value", "local-env-secret-value", "server-env-secret-value", "lsp-init-secret-value"} {
		if strings.Contains(text, secret) {
			t.Fatalf("projection leaked %q: %s", secret, text)
		}
	}
	if projection.Version != 1 || !strings.Contains(text, "redacted") || len(projection.Sources) == 0 {
		t.Fatalf("projection=%s", text)
	}
	var output bytes.Buffer
	if err := WriteProjectionText(&output, result, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "SOURCE") {
		t.Fatalf("text=%s", output.String())
	}
}

func TestWritersStrictlyValidateExistingFile(t *testing.T) {
	for _, body := range []string{`{"future":true}`, `{"provider":"old"}`, `{"max_turns":null}`, `{"color_theme":"bogus"}`, `{"max_turns":1} {}`} {
		path := writeConfig(t, body)
		before, _ := os.ReadFile(path)
		if err := SaveSelectedModel(path, "openai:gpt-5", "high"); err == nil {
			t.Fatalf("accepted %s", body)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid file was modified")
		}
	}
	path := writeConfig(t, `{"agent":"plan","max_turns":7}`)
	if err := SaveSelectedModel(path, "openai:gpt-5", "HIGH"); err != nil {
		t.Fatal(err)
	}
	if err := SaveReplEditMode(path, "vi"); err != nil {
		t.Fatal(err)
	}
	result := load(t, nil, nil, path)
	if result.Config.Provider != "openai" || result.Config.Model != "gpt-5" || result.Config.Reasoning != "high" || result.Config.Agent != "plan" || result.Config.ReplEditMode != "vi" {
		t.Fatalf("saved=%+v", result.Config)
	}
	if err := SaveReplEditMode(path, "vim"); err == nil {
		t.Fatal("accepted removed vim alias")
	}
}

func TestLoadColorThemeUsesStrictFullDecoder(t *testing.T) {
	path := writeConfig(t, `{"color_theme":"light"}`)
	got, err := LoadColorTheme("", false, lookup(nil), path)
	if err != nil || got != "light" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	for _, body := range []string{`{"color_theme":"light","future":1}`, `{"color_theme":"light","model":123}`, `{"color_theme":null}`, `{"color_theme":"light"} []`, `{"color_theme":"light","compact_trigger_percent":50,"compact_target_percent":60}`} {
		if _, err := LoadColorTheme("", false, lookup(nil), writeConfig(t, body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestLoadColorThemeRejectsExplicitEmptyEnvironment(t *testing.T) {
	_, err := LoadColorTheme("", false, lookup(map[string]string{"HARNESS_COLOR_THEME": ""}), "")
	if err == nil || !strings.Contains(err.Error(), "HARNESS_COLOR_THEME") {
		t.Fatalf("LoadColorTheme error = %v, want explicit-empty environment error", err)
	}
}

func TestCompactToolResultLimitAllowsNegativeDisableSentinel(t *testing.T) {
	result := load(t, nil, nil, writeConfig(t, `{"compact_tool_result_max_bytes":-1}`))
	if result.Config.CompactToolResultMaxBytes != -1 {
		t.Fatalf("CompactToolResultMaxBytes = %d, want -1", result.Config.CompactToolResultMaxBytes)
	}
}

func TestCompactTimeoutDefaultsAndValidates(t *testing.T) {
	result := load(t, nil, nil, "")
	if result.Config.CompactTimeoutSeconds != 300 {
		t.Fatalf("CompactTimeoutSeconds = %d, want 300", result.Config.CompactTimeoutSeconds)
	}

	result = load(t, nil, nil, writeConfig(t, `{"compact_timeout_seconds":45}`))
	if result.Config.CompactTimeoutSeconds != 45 {
		t.Fatalf("configured CompactTimeoutSeconds = %d, want 45", result.Config.CompactTimeoutSeconds)
	}

	for _, value := range []string{"0", "-1"} {
		path := writeConfig(t, `{"compact_timeout_seconds":`+value+`}`)
		if _, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: path}); err == nil || !strings.Contains(err.Error(), "compact_timeout_seconds must be positive") {
			t.Fatalf("compact_timeout_seconds=%s error = %v", value, err)
		}
	}
}

func TestProjectionPreservesProviderQualifiedModel(t *testing.T) {
	result := load(t, nil, nil, writeConfig(t, `{"model":"openai:gpt-5"}`))
	values := Project(result, false).Values
	if got := values["model"]; got != "openai:gpt-5" {
		t.Fatalf("projected model = %#v, want provider-qualified setting", got)
	}
}

func TestRemovedOTelTracesConfigurationIsUnavailable(t *testing.T) {
	if _, ok := Catalog().Lookup("otel.traces.enabled"); ok {
		t.Fatal("obsolete otel.traces.enabled remains in parameter catalog")
	}
	if _, ok := LookupCLIFlag("otel.traces.enabled"); ok {
		t.Fatal("obsolete otel.traces.enabled remains in CLI catalog")
	}
	if flag := newFlagState().set.Lookup("otel-traces"); flag != nil {
		t.Fatalf("obsolete -otel-traces flag remains available: %+v", flag)
	}

	path := writeConfig(t, `{"otel":{"traces":{"enabled":true}}}`)
	if _, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: path}); err == nil || !strings.Contains(err.Error(), `unknown field "traces"`) {
		t.Fatalf("obsolete file setting error = %v, want strict unknown-field rejection", err)
	}
	if _, err := Load(LoadOptions{Args: []string{"--otel-traces"}, LookupEnv: lookup(nil)}); err == nil {
		t.Fatal("obsolete --otel-traces flag was accepted")
	}
}

func TestRemovedCompatibilityInputsAreRejected(t *testing.T) {
	for _, args := range [][]string{{"--show-config"}, {"--no-timestamps"}} {
		if _, err := Load(LoadOptions{Args: args, LookupEnv: lookup(nil)}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for name, value := range map[string]string{"HARNESS_NO_TIMESTAMPS": "true", "HARNESS_TIMESTAMPS": "long", "HARNESS_REPL_EDIT_MODE": "vim", "LOG_LEVEL": "debug"} {
		result, err := Load(LoadOptions{LookupEnv: lookup(map[string]string{name: value})})
		if name == "LOG_LEVEL" {
			if err != nil || result.Config.LogLevel != "info" {
				t.Fatalf("LOG_LEVEL should be ignored: %+v %v", result, err)
			}
		} else if name == "HARNESS_NO_TIMESTAMPS" {
			if err != nil || result.Config.TimestampMode != "short" {
				t.Fatalf("removed env should be ignored: %+v %v", result, err)
			}
		} else if err == nil {
			t.Fatalf("accepted %s=%s", name, value)
		}
	}
}

func TestRetentionSettingsPrecedenceAndDefaults(t *testing.T) {
	// Defaults: 4-turn retention age, 800-byte result head.
	result := load(t, nil, nil, "")
	if result.Config.RetentionKeepTurns != 4 || result.Config.RetentionResultHeadBytes != 800 {
		t.Fatalf("defaults = %d/%d, want 4/800", result.Config.RetentionKeepTurns, result.Config.RetentionResultHeadBytes)
	}

	// flags > env > config > defaults
	path := writeConfig(t, `{"retention_keep_turns":6,"retention_result_head_bytes":1200}`)
	result = load(t, nil, nil, path)
	if result.Config.RetentionKeepTurns != 6 || result.Config.RetentionResultHeadBytes != 1200 {
		t.Fatalf("file values = %d/%d, want 6/1200", result.Config.RetentionKeepTurns, result.Config.RetentionResultHeadBytes)
	}
	result = load(t, nil, map[string]string{"HARNESS_RETENTION_KEEP_TURNS": "7", "HARNESS_RETENTION_RESULT_HEAD_BYTES": "1400"}, path)
	if result.Config.RetentionKeepTurns != 7 || result.Config.RetentionResultHeadBytes != 1400 {
		t.Fatalf("env values = %d/%d, want 7/1400", result.Config.RetentionKeepTurns, result.Config.RetentionResultHeadBytes)
	}
	if result.Sources["retention_keep_turns"].Name != "HARNESS_RETENTION_KEEP_TURNS" {
		t.Fatalf("env source = %+v", result.Sources["retention_keep_turns"])
	}
	// Negative values are rejected at the file layer (the settings are
	// config/env only, like their compact_* siblings).
	badPath := writeConfig(t, `{"retention_keep_turns":-1}`)
	if _, err := Load(LoadOptions{LookupEnv: lookup(nil), DefaultConfigPath: badPath}); err == nil {
		t.Fatal("negative retention_keep_turns accepted")
	}
}

func TestStagnationNudgeDefaultsOnAndCanBeDisabled(t *testing.T) {
	result := load(t, nil, nil, "")
	if !result.Config.StagnationNudge || result.Sources["stagnation_nudge"].Kind != configmeta.SourceDefault {
		t.Fatalf("default stagnation nudge = %t source=%+v, want enabled default", result.Config.StagnationNudge, result.Sources["stagnation_nudge"])
	}

	path := writeConfig(t, `{"stagnation_nudge":false}`)
	result = load(t, nil, nil, path)
	if result.Config.StagnationNudge || result.Sources["stagnation_nudge"].Kind != configmeta.SourceFile {
		t.Fatalf("config stagnation nudge = %t source=%+v, want disabled global config", result.Config.StagnationNudge, result.Sources["stagnation_nudge"])
	}

	result = load(t, nil, map[string]string{"HARNESS_STAGNATION_NUDGE": "true"}, path)
	if !result.Config.StagnationNudge || result.Sources["stagnation_nudge"].Kind != configmeta.SourceEnvironment {
		t.Fatalf("environment stagnation nudge = %t source=%+v, want enabled environment", result.Config.StagnationNudge, result.Sources["stagnation_nudge"])
	}

	result = load(t, []string{"-stagnation-nudge=false"}, map[string]string{"HARNESS_STAGNATION_NUDGE": "true"}, path)
	if result.Config.StagnationNudge || result.Sources["stagnation_nudge"].Kind != configmeta.SourceFlag {
		t.Fatalf("flag stagnation nudge = %t source=%+v, want disabled flag", result.Config.StagnationNudge, result.Sources["stagnation_nudge"])
	}
}
