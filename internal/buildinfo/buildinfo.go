// Package buildinfo holds release metadata injected by build flags.
package buildinfo

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// Version is the application version. Release builds set this to the git tag.
var Version = "dev"

// Commit is the source commit for the build.
var Commit = ""

// Date is the build timestamp.
var Date = ""

// Metadata is the machine-readable build identity persisted with sessions.
type Metadata struct {
	Version  string
	Commit   string
	Date     string
	Modified bool
}

// Current returns linker-provided release metadata, supplemented by Go VCS
// settings for local development builds.
func Current() Metadata {
	meta := Metadata{Version: Version, Commit: Commit, Date: Date}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return meta
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if meta.Commit == "" {
				meta.Commit = setting.Value
			}
		case "vcs.time":
			if meta.Date == "" {
				meta.Date = setting.Value
			}
		case "vcs.modified":
			meta.Modified, _ = strconv.ParseBool(setting.Value)
		}
	}
	return meta
}

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
