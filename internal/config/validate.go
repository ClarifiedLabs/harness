package config

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"harness/internal/logging"
	"harness/internal/reasoningprofile"
	"harness/internal/replprompt"
)

func parseString(value string) (string, error) { return value, nil }

func parseInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("must be an integer: %w", err)
	}
	return parsed, nil
}

func parseFloat(value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		if err == nil {
			err = fmt.Errorf("must be finite")
		}
		return 0, fmt.Errorf("must be a number: %w", err)
	}
	return parsed, nil
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("must be true or false")
	}
}

func identity[T any](value T) (T, error) { return value, nil }

func canonicalChoice(setting, value string, accepted ...string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range accepted {
		if value == candidate {
			return value, nil
		}
	}
	return "", fmt.Errorf("%s must be %s", setting, strings.Join(accepted, ", "))
}

func canonicalNonEmpty(setting, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s must not be empty", setting)
	}
	return value, nil
}

func canonicalLowerNonEmpty(setting, value string) (string, error) {
	value, err := canonicalNonEmpty(setting, value)
	return strings.ToLower(value), err
}

func canonicalImageDetail(value string) (string, error) {
	return canonicalChoice("image_detail", value, "auto", "low", "high", "original")
}

func canonicalReasoning(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "default" {
		return "", nil
	}
	for _, profile := range reasoningprofile.Profiles() {
		if value == profile {
			return value, nil
		}
	}
	return "", fmt.Errorf("invalid reasoning profile %q (want %s)", value, reasoningprofile.ChoicesLabel())
}

func canonicalReasoningSummary(value string) (string, error) {
	return canonicalChoice("reasoning_summary", value, "auto", "concise", "detailed", "none")
}

func canonicalWebSearch(value string) (string, error) {
	return canonicalChoice("web_search", value, "off", "auto")
}

func canonicalRetentionPolicy(value string) (string, error) {
	return canonicalChoice("retention_policy", value, "auto", "age", "pressure", "disabled")
}

func canonicalReplEditMode(value string) (string, error) {
	return canonicalChoice("repl_edit_mode", value, "emacs", "vi")
}

func canonicalColorTheme(value string) (string, error) {
	return canonicalChoice("color_theme", value, ColorThemeDark, ColorThemeLight)
}

func NormalizeTimestampMode(value string) (string, error) {
	return canonicalChoice("timestamps", value, TimestampShort, TimestampFull, TimestampNone)
}

func canonicalLogLevel(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("log_level must not be empty")
	}
	return logging.CanonicalLevel(value)
}

func canonicalReplPrompt(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("repl_prompt must not be empty")
	}
	if err := replprompt.Validate(value); err != nil {
		return "", fmt.Errorf("repl_prompt: %w", err)
	}
	return value, nil
}

func nonNegative(setting string) func(int) (int, error) {
	return func(value int) (int, error) {
		if value < 0 {
			return 0, fmt.Errorf("%s must be non-negative", setting)
		}
		return value, nil
	}
}

func positive(setting string) func(int) (int, error) {
	return func(value int) (int, error) {
		if value <= 0 {
			return 0, fmt.Errorf("%s must be positive", setting)
		}
		return value, nil
	}
}

func percent(setting string) func(int) (int, error) {
	return func(value int) (int, error) {
		if value < 1 || value > 99 {
			return 0, fmt.Errorf("%s must be between 1 and 99", setting)
		}
		return value, nil
	}
}

func canonicalOTelEndpoint(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("otel.endpoint must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("otel.endpoint must be an absolute http(s) URL")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("otel.endpoint must be an absolute http(s) URL")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("otel.endpoint must not contain user info")
	}
	if parsed.Fragment != "" {
		return "", fmt.Errorf("otel.endpoint must not contain a fragment")
	}
	return value, nil
}

func canonicalOTelProtocol(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "http/json", nil
	}
	return canonicalChoice("otel.protocol", value, "http/json")
}

func canonicalOTelServiceName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "harness", nil
	}
	return canonicalNonEmpty("otel.service_name", value)
}

func canonicalOTelHostname(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.Contains(value, ".") {
		value = strings.SplitN(value, ".", 2)[0]
		value = strings.TrimSpace(value)
		if value == "" {
			return "", fmt.Errorf("otel.hostname must not be empty")
		}
	}
	if strings.ContainsAny(value, " \t\n\r") {
		return "", fmt.Errorf("otel.hostname must not contain whitespace")
	}
	if len(value) > 253 {
		return "", fmt.Errorf("otel.hostname must be at most 253 characters")
	}
	return value, nil
}

func oTelTimeoutSeconds(value int) (int, error) {
	if value == 0 {
		return 5, nil
	}
	if value < 1 || value > 30 {
		return 0, fmt.Errorf("otel.timeout_seconds must be between 1 and 30")
	}
	return value, nil
}

func validateResolved(config Config) error {
	if config.MCP.Local.Enable && strings.TrimSpace(config.MCP.Local.Command) == "" {
		return fmt.Errorf("mcp.local.command is required when mcp.local.enable is true")
	}
	if config.OTel.Enabled && strings.TrimSpace(config.OTel.Endpoint) == "" {
		return fmt.Errorf("otel.endpoint is required when otel.enabled is true")
	}
	if err := validateCompactionRelationships(config.CompactTriggerPercent, config.CompactTargetPercent, config.CompactIdleAfterSeconds, config.CompactIdleTriggerPercent); err != nil {
		return err
	}
	return nil
}

func validateCompactionRelationships(trigger, target, idleAfter, idleTrigger int) error {
	if target >= trigger {
		return fmt.Errorf("compaction percentages must satisfy compact_target_percent < compact_trigger_percent")
	}
	if idleAfter > 0 && idleTrigger >= trigger {
		return fmt.Errorf("compact_idle_trigger_percent must be less than compact_trigger_percent when idle compaction is enabled")
	}
	return nil
}

// SplitProviderModel splits provider:model when the provider is a conservative
// identifier. Model IDs containing a colon without such a prefix remain intact.
func SplitProviderModel(model string) (provider, bareModel string, ok bool) {
	provider, bareModel, ok = strings.Cut(strings.TrimSpace(model), ":")
	if !ok || provider == "" || bareModel == "" {
		return "", "", false
	}
	for _, char := range provider {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
			continue
		}
		return "", "", false
	}
	return strings.ToLower(provider), bareModel, true
}

func parseImageAttachment(spec, defaultDetail string) (ImageAttachment, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ImageAttachment{}, fmt.Errorf("--image requires a path")
	}
	detail := defaultDetail
	if before, after, ok := strings.Cut(spec, ":"); ok && after != "" {
		if parsed, err := canonicalImageDetail(before); err == nil {
			detail, spec = parsed, after
		}
	}
	return ImageAttachment{Path: spec, Detail: detail}, nil
}
