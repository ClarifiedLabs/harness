package lspproxy

import "fmt"

// SupportedConfigVersion is the config schema version this build understands.
// The config envelope carries an integer "version"; bumping the schema in a
// backward-incompatible way bumps this constant so an old binary rejects a newer
// file with a clear message instead of silently misreading it.
const SupportedConfigVersion = 1

// SupportsConfigVersion reports whether v is a config version this build can load.
func SupportsConfigVersion(v int) bool {
	return v == SupportedConfigVersion
}

// ConfigVersionError reports a config whose declared version this build does not
// support. It mirrors the version-gating pattern used for the MCP protocol
// version (internal/mcp/version.go).
type ConfigVersionError struct {
	Got       int
	Supported int
}

func (e *ConfigVersionError) Error() string {
	return fmt.Sprintf("lspproxy: config version %d is unsupported (this build supports version %d)", e.Got, e.Supported)
}
