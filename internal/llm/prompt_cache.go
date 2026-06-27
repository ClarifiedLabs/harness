package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	PromptCacheKeyFieldAuto           = "auto"
	PromptCacheKeyFieldNone           = "none"
	PromptCacheKeyFieldPromptCacheKey = "prompt_cache_key"
	PromptCacheKeyFieldSessionID      = "session_id"

	openRouterSessionIDMaxLen = 256
)

// ResolvePromptCacheKeyField maps a provider config and endpoint to the request
// body field that should receive Request.PromptCacheKey.
func ResolvePromptCacheKeyField(providerName, apiType, baseURL string, cfg PromptCacheConfig) string {
	field := strings.ToLower(strings.TrimSpace(cfg.KeyField))
	switch field {
	case "", PromptCacheKeyFieldAuto:
		base := strings.ToLower(baseURL)
		if strings.EqualFold(providerName, "openrouter") || strings.Contains(base, "openrouter.ai") {
			return PromptCacheKeyFieldSessionID
		}
		if strings.EqualFold(providerName, "openai") || strings.Contains(base, "api.openai.com") {
			return PromptCacheKeyFieldPromptCacheKey
		}
		return PromptCacheKeyFieldNone
	case PromptCacheKeyFieldNone, PromptCacheKeyFieldPromptCacheKey, PromptCacheKeyFieldSessionID:
		return field
	default:
		return PromptCacheKeyFieldNone
	}
}

// PromptCacheSessionID returns a stable value suitable for OpenRouter's
// session_id/x-session-id limit.
func PromptCacheSessionID(key string) string {
	return promptCacheLimitedValue(key, openRouterSessionIDMaxLen)
}

// ApplyPromptCacheAffinityHeaders puts the stable prompt cache key into
// configured affinity headers without allowing cache configuration to replace
// authentication headers.
func ApplyPromptCacheAffinityHeaders(h http.Header, names []string, key string) {
	if key == "" {
		return
	}
	value := PromptCacheSessionID(key)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || promptCacheProtectedHeader(name) {
			continue
		}
		h.Set(name, value)
	}
}

func promptCacheLimitedValue(value string, maxLen int) string {
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "harness-sha256-" + hex.EncodeToString(sum[:])
}

func promptCacheProtectedHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "openai-api-key", "anthropic-api-key":
		return true
	default:
		return false
	}
}
