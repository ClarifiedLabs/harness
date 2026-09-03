//go:build !unix

package lspproxy

import "os"

// Non-Unix platforms do not expose portable nonblocking/no-follow open flags.
// The caller validates both the path before opening and the resulting descriptor.
func openTypeScriptPackageFile(path string) (*os.File, error) {
	return os.Open(path)
}
