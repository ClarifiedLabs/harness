// Package buildinfo holds release metadata injected by build flags.
package buildinfo

import "strings"

// Version is the application version. Release builds set this to the git tag.
var Version = "dev"

// Commit is the source commit for the build.
var Commit = ""

// Date is the build timestamp.
var Date = ""

// Line returns a single human-readable version line for name.
func Line(name string) string {
	line := name + " " + Version
	var extra []string
	if Commit != "" {
		extra = append(extra, "commit "+Commit)
	}
	if Date != "" {
		extra = append(extra, "built "+Date)
	}
	if len(extra) > 0 {
		line += " (" + strings.Join(extra, ", ") + ")"
	}
	return line
}
