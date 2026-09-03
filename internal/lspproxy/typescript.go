package lspproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

const (
	defaultTypeScriptServerName = "typescript-language-server"
	typeScriptPackageJSONLimit  = 1 << 20
)

var defaultTypeScriptServerCommand = []string{"typescript-language-server", "--stdio"}

// usesAutomaticTypeScriptCommand identifies the built-in TypeScript server by
// its stable name and argv. A custom command remains authoritative and bypasses
// project/global TypeScript discovery.
func usesAutomaticTypeScriptCommand(s ResolvedServer) bool {
	return s.Name == defaultTypeScriptServerName && slices.Equal(s.Command, defaultTypeScriptServerCommand)
}

type typeScriptInstallation struct {
	packageDir string
	major      int
	bin        string
}

type typeScriptPackageJSON struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Bin     json.RawMessage `json:"bin"`
}

// resolveServerCommand resolves the executable for one server at a workspace
// root and reports whether that executable is currently runnable. Most servers
// use their configured argv unchanged. The built-in TypeScript entry prefers a
// workspace TypeScript 7+ tsc, then a metadata-identifiable global TypeScript 7+
// tsc, and otherwise retains typescript-language-server for TypeScript 6 and
// earlier.
func (m *Manager) resolveServerCommand(s ResolvedServer, root string) (ResolvedServer, bool) {
	if len(s.Command) == 0 {
		return s, false
	}
	if !usesAutomaticTypeScriptCommand(s) {
		return s, m.commandAvailable(s.Command[0])
	}

	if local, found := findProjectTypeScript(root); found {
		// A local installation is authoritative even when malformed or incomplete:
		// do not silently replace the project's selected compiler with a global one.
		if local.major >= 7 {
			return m.resolveTypeScriptPackageCommand(s, local)
		}
		return s, m.commandAvailable(s.Command[0])
	}

	if m.lookPath != nil {
		if tsc, err := m.lookPath("tsc"); err == nil {
			if global, ok := typeScriptInstallationNearExecutable(tsc); ok && global.major >= 7 {
				return m.resolveTypeScriptPackageCommand(s, global)
			}
		}
	}
	return s, m.commandAvailable(s.Command[0])
}

func (m *Manager) resolveTypeScriptPackageCommand(s ResolvedServer, installation typeScriptInstallation) (ResolvedServer, bool) {
	command, available := typeScriptPackageLSPCommand(installation, runtime.GOOS, m.lookPath)
	if len(command) == 0 {
		// Preserve the expected package path in the eventual launch error for an
		// incomplete installation.
		command = []string{filepath.Join(installation.packageDir, "bin", "tsc"), "--lsp", "--stdio"}
	}
	s.Command = command
	return s, available
}

func typeScriptPackageLSPCommand(installation typeScriptInstallation, goos string, lookPath func(string) (string, error)) ([]string, bool) {
	bin := installation.bin
	if bin == "" {
		bin = confinedPackagePath(installation.packageDir, filepath.Join("bin", "tsc"))
	}
	if bin == "" || lookPath == nil {
		return nil, false
	}
	if goos == "windows" {
		// The npm package's extensionless tsc bin is a Node script; npm's .cmd
		// wrapper is not safely executable via os/exec without a shell.
		node, err := lookPath("node")
		if err != nil {
			return []string{bin, "--lsp", "--stdio"}, false
		}
		return []string{node, bin, "--lsp", "--stdio"}, true
	}
	if _, err := lookPath(bin); err != nil {
		return []string{bin, "--lsp", "--stdio"}, false
	}
	return []string{bin, "--lsp", "--stdio"}, true
}

func (m *Manager) commandAvailable(command string) bool {
	if command == "" || m.lookPath == nil {
		return false
	}
	_, err := m.lookPath(command)
	return err == nil
}

// findProjectTypeScript follows Node's ancestor lookup shape within the trusted
// workspace: at each directory from root through the nearest Git root, inspect
// node_modules/typescript/package.json and stop at the nearest installation. If
// there is no Git boundary, only root is searched. This handles normal and
// hoisted installs without selecting packages from an unrelated shared parent.
func findProjectTypeScript(root string) (typeScriptInstallation, bool) {
	if strings.TrimSpace(root) == "" {
		return typeScriptInstallation{}, false
	}
	dir, err := filepath.Abs(root)
	if err != nil {
		return typeScriptInstallation{}, false
	}
	ceiling := typeScriptSearchCeiling(dir)
	for {
		packageDir := filepath.Join(dir, "node_modules", "typescript")
		if info, statErr := os.Stat(packageDir); statErr == nil && info.IsDir() {
			return readTypeScriptInstallation(packageDir), true
		}
		if dir == ceiling {
			return typeScriptInstallation{}, false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return typeScriptInstallation{}, false
		}
		dir = parent
	}
}

func typeScriptSearchCeiling(root string) string {
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return root
		}
	}
}

func readTypeScriptInstallation(packageDir string) typeScriptInstallation {
	installation := typeScriptInstallation{packageDir: packageDir}
	data, err := readBoundedRegularFile(filepath.Join(packageDir, "package.json"), typeScriptPackageJSONLimit)
	if err != nil {
		return installation
	}
	var pkg typeScriptPackageJSON
	if json.Unmarshal(data, &pkg) != nil || pkg.Name != "typescript" {
		return installation
	}
	major, ok := parseTypeScriptMajor(pkg.Version)
	if !ok {
		return installation
	}
	installation.major = major
	if rel := typeScriptBin(pkg.Bin); rel != "" {
		installation.bin = confinedPackagePath(packageDir, rel)
	}
	return installation
}

func typeScriptBin(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var bins map[string]string
	if json.Unmarshal(raw, &bins) == nil {
		return bins["tsc"]
	}
	return ""
}

// confinedPackagePath requires the declared bin to remain inside the real
// package directory after symlink evaluation. Resolving the package directory
// first preserves pnpm/Yarn installs where node_modules/typescript is a symlink.
func confinedPackagePath(packageDir, rel string) string {
	if rel == "" || filepath.IsAbs(rel) {
		return ""
	}
	path := filepath.Clean(filepath.Join(packageDir, filepath.FromSlash(rel)))
	within, err := filepath.Rel(packageDir, path)
	if err != nil || pathEscapes(within) {
		return ""
	}
	realPackage, err := filepath.EvalSymlinks(packageDir)
	if err != nil {
		return ""
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	within, err = filepath.Rel(realPackage, realPath)
	if err != nil || pathEscapes(within) {
		return ""
	}
	return path
}

func pathEscapes(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// readBoundedRegularFile rejects a final symlink or special file before opening
// it, then verifies the opened descriptor to avoid blocking on workspace FIFOs
// and consuming input through device links. The size limit bounds regular files.
func readBoundedRegularFile(path string, max int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	f, err := openTypeScriptPackageFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

func parseTypeScriptMajor(version string) (int, bool) {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "v")
	end := 0
	for end < len(version) && version[end] >= '0' && version[end] <= '9' {
		end++
	}
	if end == 0 || end < len(version) && version[end] != '.' && version[end] != '-' {
		return 0, false
	}
	major, err := strconv.Atoi(version[:end])
	return major, err == nil
}

// typeScriptInstallationNearExecutable derives a global npm installation from
// a resolved tsc path (normally a node_modules/.bin symlink) without executing
// the candidate. Nonstandard shims can be configured explicitly.
func typeScriptInstallationNearExecutable(path string) (typeScriptInstallation, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	dir := filepath.Dir(resolved)
	for range 4 {
		if installation := readTypeScriptInstallation(dir); installation.major > 0 {
			return installation, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return typeScriptInstallation{}, false
}
