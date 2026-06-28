// Package reasoningprofile defines the harness-wide portable reasoning profile
// vocabulary used at the CLI/config boundary and by the model proxy mapper.
package reasoningprofile

import (
	"fmt"
	"strings"
)

var profiles = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Profiles returns the concrete portable reasoning profiles, excluding the
// empty provider-default profile.
func Profiles() []string {
	return append([]string(nil), profiles...)
}

// Choices returns the user-facing profile list, including default.
func Choices() []string {
	return append([]string{"default"}, profiles...)
}

// ChoicesLabel returns the slash-separated user-facing profile list.
func ChoicesLabel() string {
	return strings.Join(Choices(), "/")
}

// Normalize canonicalizes a user-supplied reasoning profile. It returns an empty
// profile for provider default.
func Normalize(profile string) (string, bool) {
	profile = strings.ToLower(strings.TrimSpace(profile))
	switch profile {
	case "", "default", "provider-default":
		return "", true
	case "off", "false", "disabled", "disable":
		return "none", true
	case "minimum", "min":
		return "minimal", true
	}
	for _, known := range profiles {
		if profile == known {
			return profile, true
		}
	}
	return "", false
}

// Canonicalize returns the normalized profile or a usage-oriented error.
func Canonicalize(profile string) (string, error) {
	out, ok := Normalize(profile)
	if ok {
		return out, nil
	}
	return "", fmt.Errorf("invalid reasoning profile %q (want %s)", profile, ChoicesLabel())
}

// Label formats a canonical profile for display.
func Label(profile string) string {
	if strings.TrimSpace(profile) == "" {
		return "provider default"
	}
	return profile
}
