package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"harness/internal/configmeta"
	"harness/internal/hooks"
)

type resolveContext struct {
	options LoadOptions
	lookup  func(string) (string, bool)
	flags   *flagState
	file    fileConfig
	path    string
	result  *Result
}

func (context *resolveContext) fileError(setting string, err error) error {
	return fmt.Errorf("config %q setting %s: %w", context.path, setting, err)
}
func (context *resolveContext) fileSource(key string) {
	context.result.Sources[key] = configmeta.Source{Kind: configmeta.SourceFile, Name: context.path}
}
func (context *resolveContext) defaultSource(key string) {
	context.result.Sources[key] = configmeta.Source{Kind: configmeta.SourceDefault, Name: "built-in"}
}

// Load strictly validates every present source and resolves one Result.
func Load(options LoadOptions) (Result, error) {
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	flags := newFlagState()
	if err := flags.set.Parse(options.Args); err != nil {
		return Result{}, err
	}
	meta, err := resolveMetaRunOptions(flags)
	if err != nil {
		return Result{}, err
	}
	if meta.Help || meta.Version {
		return Result{Run: meta}, nil
	}
	path, err := resolveConfigPath(flags, lookup, options.DefaultConfigPath)
	if err != nil {
		return Result{}, err
	}
	file, err := decodeConfigFile(path)
	if err != nil {
		return Result{}, err
	}
	if err := validateFileConfig(file, path); err != nil {
		return Result{}, err
	}
	result := Result{Sources: make(map[string]configmeta.Source), ConfigPath: path, fileReferences: collectFileReferences(file)}
	context := &resolveContext{options: options, lookup: lookup, flags: flags, file: file, path: path, result: &result}
	for _, definition := range allDefinitions {
		if err := definition.resolve(context); err != nil {
			return Result{}, err
		}
	}
	if provider, model, ok := SplitProviderModel(result.Config.Model); ok {
		result.Config.Provider, result.Config.Model = provider, model
	}
	result.Config.MCP.Local.EnableSet = result.Sources["mcp.local.enable"].Kind != configmeta.SourceDefault
	result.Config.LSP.Serena.EnableSet = result.Sources["lsp.serena.enable"].Kind != configmeta.SourceDefault
	if err := validateResolved(result.Config); err != nil {
		return Result{}, err
	}
	if err := resolveRunOptions(context); err != nil {
		return Result{}, err
	}
	return result, nil
}

// ResolveConfigPath applies the canonical --config > HARNESS_CONFIG > existing
// conventional-path precedence without loading settings.
func ResolveConfigPath(options LoadOptions) (string, error) {
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	flags := newFlagState()
	if err := flags.set.Parse(options.Args); err != nil {
		return "", err
	}
	return resolveConfigPath(flags, lookup, options.DefaultConfigPath)
}

func resolveConfigPath(flags *flagState, lookup func(string) (string, bool), conventional string) (string, error) {
	if values := flags.invocation["config"]; len(values) > 0 {
		for _, value := range values {
			if strings.TrimSpace(value.value) == "" {
				return "", fmt.Errorf("flag --%s requires a non-empty path", value.name)
			}
		}
		path := values[len(values)-1].value
		if err := requireConfigFile(path, "flag --config"); err != nil {
			return "", err
		}
		return path, nil
	}
	if path, present := lookup("HARNESS_CONFIG"); present {
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("environment HARNESS_CONFIG requires a non-empty path")
		}
		if err := requireConfigFile(path, "environment HARNESS_CONFIG"); err != nil {
			return "", err
		}
		return path, nil
	}
	if conventional == "" {
		return "", nil
	}
	_, err := os.Stat(conventional)
	if err == nil {
		return conventional, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return "", fmt.Errorf("inspect default config %q: %w", conventional, err)
}
func requireConfigFile(path, source string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s config %q: %w", source, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s config %q is a directory", source, path)
	}
	return nil
}

func validateFileConfig(file fileConfig, path string) error {
	for _, definition := range allDefinitions {
		if err := definition.validateFile(file, path); err != nil {
			return err
		}
	}
	if file.MCP.Set && file.MCP.Value.Local.Set && file.MCP.Value.Local.Value.Enable.Set && file.MCP.Value.Local.Value.Enable.Value && (!file.MCP.Value.Local.Value.Command.Set || strings.TrimSpace(file.MCP.Value.Local.Value.Command.Value) == "") {
		return fmt.Errorf("config %q setting mcp.local.command: required when mcp.local.enable is true", path)
	}
	trigger := optionalValue(file.CompactTriggerPercent, defaultCompactTriggerPercent)
	target := optionalValue(file.CompactTargetPercent, defaultCompactTargetPercent)
	idleAfter := optionalValue(file.CompactIdleAfterSeconds, 0)
	idleTrigger := optionalValue(file.CompactIdleTriggerPercent, defaultCompactIdleTrigger)
	if err := validateCompactionRelationships(trigger, target, idleAfter, idleTrigger); err != nil {
		return fmt.Errorf("config %q: %w", path, err)
	}
	return nil
}

func optionalValue[T any](value optional[T], fallback T) T {
	if value.Set {
		return value.Value
	}
	return fallback
}

func collectFileReferences(file fileConfig) []configFileReference {
	var references []configFileReference
	if file.SystemPrompt.Set {
		references = append(references, configFileReference{setting: "system_prompt", value: file.SystemPrompt.Value})
	}
	if file.Agents.Set {
		names := make([]string, 0, len(file.Agents.Value))
		for name := range file.Agents.Value {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			references = append(references, configFileReference{setting: fmt.Sprintf("agent %q prompt", name), value: file.Agents.Value[name].Prompt})
		}
	}
	return references
}

func resolveMCPHeaders(context *resolveContext) error {
	if context.file.MCP.Set && context.file.MCP.Value.Headers.Set {
		values, err := expandStringMap(context.file.MCP.Value.Headers.Value, context.lookup, "mcp.headers")
		if err != nil {
			return context.fileError("mcp.headers", err)
		}
		context.result.Config.MCP.Headers = values
		context.fileSource("mcp.headers")
	} else {
		context.defaultSource("mcp.headers")
	}
	return nil
}
func resolveMCPLocalArgs(context *resolveContext) error {
	if context.file.MCP.Set && context.file.MCP.Value.Local.Set && context.file.MCP.Value.Local.Value.Args.Set {
		context.result.Config.MCP.Local.Args = append([]string(nil), context.file.MCP.Value.Local.Value.Args.Value...)
		context.fileSource("mcp.local.args")
	} else {
		context.defaultSource("mcp.local.args")
	}
	return nil
}
func resolveMCPLocalEnv(context *resolveContext) error {
	if context.file.MCP.Set && context.file.MCP.Value.Local.Set && context.file.MCP.Value.Local.Value.Env.Set {
		values, err := expandStringMap(context.file.MCP.Value.Local.Value.Env.Value, context.lookup, "mcp.local.env")
		if err != nil {
			return context.fileError("mcp.local.env", err)
		}
		context.result.Config.MCP.Local.Env = values
		context.fileSource("mcp.local.env")
	} else {
		context.defaultSource("mcp.local.env")
	}
	return nil
}
func resolveLSPTools(context *resolveContext) error {
	if context.file.LSP.Set && context.file.LSP.Value.Tools.Set {
		context.result.Config.LSP.Tools = append([]string(nil), context.file.LSP.Value.Tools.Value...)
		context.fileSource("lsp.tools")
	} else {
		context.defaultSource("lsp.tools")
	}
	return nil
}
func resolveLSPServers(context *resolveContext) error {
	if context.file.LSP.Set && context.file.LSP.Value.Servers.Set {
		context.result.Config.LSP.Servers = cloneLSPServers(context.file.LSP.Value.Servers.Value)
		context.fileSource("lsp.servers")
	} else {
		context.defaultSource("lsp.servers")
	}
	return nil
}
func resolveSerenaArgs(context *resolveContext) error {
	context.result.Config.LSP.Serena.Args = DefaultSerenaArgs()
	context.defaultSource("lsp.serena.args")
	if context.file.LSP.Set && context.file.LSP.Value.Serena.Set && context.file.LSP.Value.Serena.Value.Args.Set {
		context.result.Config.LSP.Serena.Args = append([]string(nil), context.file.LSP.Value.Serena.Value.Args.Value...)
		context.fileSource("lsp.serena.args")
	}
	return nil
}
func resolveSerenaEnv(context *resolveContext) error {
	if context.file.LSP.Set && context.file.LSP.Value.Serena.Set && context.file.LSP.Value.Serena.Value.Env.Set {
		values, err := expandStringMap(context.file.LSP.Value.Serena.Value.Env.Value, context.lookup, "lsp.serena.env")
		if err != nil {
			return context.fileError("lsp.serena.env", err)
		}
		context.result.Config.LSP.Serena.Env = values
		context.fileSource("lsp.serena.env")
	} else {
		context.defaultSource("lsp.serena.env")
	}
	return nil
}

func resolveHooks(context *resolveContext) error {
	if override, present := context.flags.lastInvocation("hooks_override"); present {
		if strings.TrimSpace(override.value) == "" {
			return fmt.Errorf("--hooks requires a path")
		}
		loaded, err := hooks.LoadFile("", override.value)
		if err != nil {
			return fmt.Errorf("--hooks: %w", err)
		}
		context.result.Config.Hooks = loaded
		source := configmeta.Source{Kind: configmeta.SourceFlag, Name: "--hooks"}
		context.result.Sources["hooks"] = source
		context.result.Sources["hook_configs"] = source
		return nil
	}
	context.defaultSource("hooks")
	context.defaultSource("hook_configs")
	if context.file.Hooks.Set {
		inline, err := hooks.DecodeEventMap(context.file.Hooks.Value)
		if err != nil {
			return context.fileError("hooks", err)
		}
		context.result.Config.Hooks.Append(inline)
		context.fileSource("hooks")
	}
	if context.file.HookConfigs.Set {
		external, err := hooks.LoadFiles(filepath.Dir(context.path), context.file.HookConfigs.Value)
		if err != nil {
			return context.fileError("hook_configs", err)
		}
		context.result.Config.Hooks.Append(external)
		context.result.Config.HookConfigs = append([]string(nil), context.file.HookConfigs.Value...)
		context.fileSource("hook_configs")
	}
	return nil
}

// LoadColorTheme resolves only the theme, but always uses the complete strict
// schema decoder so malformed or unknown unrelated settings are not hidden.
func LoadColorTheme(flagValue string, flagSet bool, lookup func(string) (string, bool), configPath string) (string, error) {
	file, err := decodeConfigFile(configPath)
	if err != nil {
		return "", err
	}
	if err := validateFileConfig(file, configPath); err != nil {
		return "", err
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value := ColorThemeDark
	if file.ColorTheme.Set {
		value, err = canonicalColorTheme(file.ColorTheme.Value)
		if err != nil {
			return "", fmt.Errorf("config %q setting color_theme: %w", configPath, err)
		}
	}
	if env, present := lookup("HARNESS_COLOR_THEME"); present {
		value, err = canonicalColorTheme(env)
		if err != nil {
			return "", fmt.Errorf("environment HARNESS_COLOR_THEME for color_theme: %w", err)
		}
	}
	if flagSet {
		value, err = canonicalColorTheme(flagValue)
		if err != nil {
			return "", fmt.Errorf("flag --color-theme: %w", err)
		}
	}
	return value, nil
}
