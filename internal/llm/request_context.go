package llm

import "strings"

// RequestContextText renders per-request context that is visible to the model
// but not part of the persisted local transcript.
func RequestContextText(context []string) string {
	var parts []string
	for _, ctx := range context {
		if s := strings.TrimSpace(ctx); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "[hook context]\n" + strings.Join(parts, "\n\n")
}
