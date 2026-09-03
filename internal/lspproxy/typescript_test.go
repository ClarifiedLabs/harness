package lspproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestManagerUsesProjectTypeScript7LSP(t *testing.T) {
	root := t.TempDir()
	bin := installTypeScript(t, root, "7.0.2", "bin/tsc")
	writeTestFile(t, filepath.Join(root, "tsconfig.json"), "{}")
	source := filepath.Join(root, "src", "main.ts")
	writeTestFile(t, source, "export const answer = 42\n")
	t.Chdir(root)

	server := defaultTypeScriptServer()
	m := newTypeScriptTestManager(server)
	m.lookPath = func(command string) (string, error) {
		if command == bin {
			return command, nil
		}
		return "", exec.ErrNotFound
	}
	m.computeAvailable()

	wantLanguages := slices.Clone(server.Languages)
	slices.Sort(wantLanguages)
	if got := m.InstalledLanguages(); !slices.Equal(got, wantLanguages) {
		t.Fatalf("InstalledLanguages() = %v, want %v", got, wantLanguages)
	}
	statuses := m.ServerStatuses()
	if len(statuses) != 1 || !statuses[0].Installed || statuses[0].Command != bin {
		t.Fatalf("ServerStatuses() = %+v, want installed command %q", statuses, bin)
	}

	var acquired ResolvedServer
	var acquiredRoot string
	m.acquireFn = func(_ context.Context, server ResolvedServer, root string) (*lspClient, error) {
		acquired = server
		acquiredRoot = root
		return nil, nil
	}
	if _, errResult := m.targetFor(context.Background(), source); errResult != nil {
		t.Fatalf("targetFor() error = %+v", errResult)
	}
	wantCommand := []string{bin, "--lsp", "--stdio"}
	if !slices.Equal(acquired.Command, wantCommand) {
		t.Fatalf("acquired command = %q, want %q", acquired.Command, wantCommand)
	}
	if acquiredRoot != root {
		t.Fatalf("acquired root = %q, want %q", acquiredRoot, root)
	}
}

func TestProjectTypeScript6KeepsLanguageServer(t *testing.T) {
	root := t.TempDir()
	installTypeScript(t, root, "6.0.1", "bin/tsc")

	server := defaultTypeScriptServer()
	m := newTypeScriptTestManager(server)
	m.lookPath = func(command string) (string, error) {
		switch command {
		case "typescript-language-server":
			return "/global/typescript-language-server", nil
		case "tsc":
			t.Fatal("global tsc must not replace a project-local TypeScript version")
		}
		return "", exec.ErrNotFound
	}

	got, installed := m.resolveServerCommand(server, root)
	if !installed {
		t.Fatal("resolveServerCommand() reported the legacy language server missing")
	}
	if !slices.Equal(got.Command, defaultTypeScriptServerCommand) {
		t.Fatalf("command = %q, want %q", got.Command, defaultTypeScriptServerCommand)
	}
}

func TestGlobalTypeScript7LSPFallback(t *testing.T) {
	globalRoot := t.TempDir()
	bin := installTypeScript(t, globalRoot, "7.0.2", "bin/tsc")
	installation, ok := typeScriptInstallationNearExecutable(bin)
	if !ok || installation.major != 7 {
		t.Fatalf("typeScriptInstallationNearExecutable(%q) = (%+v, %v)", bin, installation, ok)
	}
	server := defaultTypeScriptServer()
	m := newTypeScriptTestManager(server)
	m.lookPath = func(command string) (string, error) {
		if command == "tsc" {
			return bin, nil
		}
		if command == installation.bin {
			return installation.bin, nil
		}
		return "", exec.ErrNotFound
	}

	got, installed := m.resolveServerCommand(server, t.TempDir())
	if !installed {
		t.Fatal("resolveServerCommand() reported global TypeScript 7 missing")
	}
	want := []string{installation.bin, "--lsp", "--stdio"}
	if !slices.Equal(got.Command, want) {
		t.Fatalf("command = %q, want %q", got.Command, want)
	}
}

func TestGlobalTypeScript6FallsBackToLanguageServer(t *testing.T) {
	globalRoot := t.TempDir()
	bin := installTypeScript(t, globalRoot, "6.0.1", "bin/tsc")
	server := defaultTypeScriptServer()
	m := newTypeScriptTestManager(server)
	m.lookPath = func(command string) (string, error) {
		switch command {
		case "tsc":
			return bin, nil
		case "typescript-language-server":
			return "/global/bin/typescript-language-server", nil
		default:
			return "", exec.ErrNotFound
		}
	}

	got, installed := m.resolveServerCommand(server, t.TempDir())
	if !installed {
		t.Fatal("resolveServerCommand() reported the legacy language server missing")
	}
	if !slices.Equal(got.Command, defaultTypeScriptServerCommand) {
		t.Fatalf("command = %q, want %q", got.Command, defaultTypeScriptServerCommand)
	}
}

func TestExplicitTypeScriptCommandBypassesDiscovery(t *testing.T) {
	server := defaultTypeScriptServer()
	server.Command = []string{"custom-typescript-server", "--stdio"}
	m := newTypeScriptTestManager(server)
	m.lookPath = func(command string) (string, error) {
		if command != "custom-typescript-server" {
			t.Fatalf("unexpected lookup for %q", command)
		}
		return "/custom/typescript-server", nil
	}
	got, installed := m.resolveServerCommand(server, t.TempDir())
	if !installed {
		t.Fatal("resolveServerCommand() reported the custom server missing")
	}
	if !slices.Equal(got.Command, server.Command) {
		t.Fatalf("command = %q, want explicit command %q", got.Command, server.Command)
	}
}

func TestTypeScriptDefaultCommandMatchesEmbeddedConfig(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range cfg.Servers {
		if server.Name == defaultTypeScriptServerName {
			if !slices.Equal(server.Command, defaultTypeScriptServerCommand) {
				t.Fatalf("embedded command = %q, automatic-selection sentinel = %q", server.Command, defaultTypeScriptServerCommand)
			}
			return
		}
	}
	t.Fatalf("embedded config has no %q server", defaultTypeScriptServerName)
}

func TestPrewarmUsesProjectTypeScript7LSP(t *testing.T) {
	root := t.TempDir()
	bin := installTypeScript(t, root, "7.0.2", "bin/tsc")
	writeTestFile(t, filepath.Join(root, "tsconfig.json"), "{}")
	writeTestFile(t, filepath.Join(root, "src", "main.ts"), "export const answer = 42\n")
	t.Chdir(root)

	server := defaultTypeScriptServer()
	m := newTypeScriptTestManager(server)
	m.lookPath = func(command string) (string, error) {
		if command == bin {
			return bin, nil
		}
		return "", exec.ErrNotFound
	}
	var acquired ResolvedServer
	m.acquireFn = func(_ context.Context, server ResolvedServer, _ string) (*lspClient, error) {
		acquired = server
		return nil, nil
	}

	wantLanguages := slices.Clone(server.Languages)
	slices.Sort(wantLanguages)
	if got := m.Prewarm(context.Background()); !slices.Equal(got, wantLanguages) {
		t.Fatalf("Prewarm() = %v, want %v", got, wantLanguages)
	}
	wantCommand := []string{bin, "--lsp", "--stdio"}
	if !slices.Equal(acquired.Command, wantCommand) {
		t.Fatalf("prewarm command = %q, want %q", acquired.Command, wantCommand)
	}
}

func TestTypeScriptWindowsPackageCommandUsesNode(t *testing.T) {
	root := t.TempDir()
	bin := installTypeScript(t, root, "7.0.2", "bin/tsc")
	installation, found := findProjectTypeScript(root)
	if !found {
		t.Fatal("findProjectTypeScript() did not find package")
	}
	command, available := typeScriptPackageLSPCommand(installation, "windows", func(command string) (string, error) {
		if command == "node" {
			return `C:\Program Files\nodejs\node.exe`, nil
		}
		return "", exec.ErrNotFound
	})
	want := []string{`C:\Program Files\nodejs\node.exe`, bin, "--lsp", "--stdio"}
	if !available || !slices.Equal(command, want) {
		t.Fatalf("typeScriptPackageLSPCommand() = (%q, %v), want (%q, true)", command, available, want)
	}
}

func TestNonstandardGlobalTSCIsNotExecutedDuringDiscovery(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	tsc := filepath.Join(root, "tsc")
	writeTestFile(t, tsc, "#!/bin/sh\ntouch \""+marker+"\"\necho 'Version 7.0.2'\n")
	if err := os.Chmod(tsc, 0o755); err != nil {
		t.Fatal(err)
	}

	server := defaultTypeScriptServer()
	m := newTypeScriptTestManager(server)
	m.lookPath = func(command string) (string, error) {
		switch command {
		case "tsc":
			return tsc, nil
		case "typescript-language-server":
			return "/global/typescript-language-server", nil
		default:
			return "", exec.ErrNotFound
		}
	}
	got, installed := m.resolveServerCommand(server, t.TempDir())
	if !installed || !slices.Equal(got.Command, defaultTypeScriptServerCommand) {
		t.Fatalf("resolveServerCommand() = (%q, %v), want metadata-only legacy fallback", got.Command, installed)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("global tsc was executed during discovery; marker stat error = %v", err)
	}
}

func TestFindProjectTypeScriptContinuesAfterNonDirectoryNodeModules(t *testing.T) {
	root := t.TempDir()
	bin := installTypeScript(t, root, "7.0.2", "bin/tsc")
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(project, "node_modules"), "not a directory")

	installation, found := findProjectTypeScript(project)
	if !found || installation.bin != bin {
		t.Fatalf("findProjectTypeScript() = (%+v, %v), want ancestor bin %q", installation, found, bin)
	}
}

func TestFindProjectTypeScriptDoesNotCrossGitBoundary(t *testing.T) {
	parent := t.TempDir()
	installTypeScript(t, parent, "7.0.2", "bin/tsc")
	repo := filepath.Join(parent, "repo")
	project := filepath.Join(repo, "packages", "app")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	if installation, found := findProjectTypeScript(project); found {
		t.Fatalf("findProjectTypeScript() selected package above Git boundary: %+v", installation)
	}
}

func TestTypeScriptPackageBinCannotEscapePackage(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "node_modules", "typescript")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := map[string]any{
		"name":    "typescript",
		"version": "7.0.0",
		"bin":     map[string]string{"tsc": "../../outside"},
	}
	data, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	installation, found := findProjectTypeScript(root)
	if !found {
		t.Fatal("findProjectTypeScript() did not find package")
	}
	if installation.major != 7 || installation.bin != "" {
		t.Fatalf("installation = %+v, want major 7 with a rejected bin path", installation)
	}
}

func TestTypeScriptPackageBinSymlinkCannotEscapePackage(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "node_modules", "typescript")
	outside := filepath.Join(root, "outside-tsc")
	writeTestFile(t, outside, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(packageDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(packageDir, "bin", "tsc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	pkg := map[string]any{
		"name":    "typescript",
		"version": "7.0.0",
		"bin":     map[string]string{"tsc": "bin/tsc"},
	}
	data, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	installation, found := findProjectTypeScript(root)
	if !found || installation.major != 7 || installation.bin != "" {
		t.Fatalf("findProjectTypeScript() = (%+v, %v), want rejected escaping symlink", installation, found)
	}
}

func TestTypeScriptPackageJSONSymlinkIsRejected(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "node_modules", "typescript")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "package.json")
	writeTestFile(t, outside, `{"name":"typescript","version":"7.0.0"}`)
	if err := os.Symlink(outside, filepath.Join(packageDir, "package.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	installation, found := findProjectTypeScript(root)
	if !found || installation.major != 0 {
		t.Fatalf("findProjectTypeScript() = (%+v, %v), want rejected package.json symlink", installation, found)
	}
}

func TestParseTypeScriptMajor(t *testing.T) {
	for _, test := range []struct {
		version string
		major   int
		ok      bool
	}{
		{version: "7.0.2", major: 7, ok: true},
		{version: "7.0.0-dev.20260903", major: 7, ok: true},
		{version: "v6.1.0", major: 6, ok: true},
		{version: "7invalid", ok: false},
		{version: "not TypeScript", ok: false},
	} {
		t.Run(test.version, func(t *testing.T) {
			major, ok := parseTypeScriptMajor(test.version)
			if major != test.major || ok != test.ok {
				t.Fatalf("parseTypeScriptMajor(%q) = (%d, %v), want (%d, %v)", test.version, major, ok, test.major, test.ok)
			}
		})
	}
}

func defaultTypeScriptServer() ResolvedServer {
	return ResolvedServer{
		Name:        defaultTypeScriptServerName,
		Languages:   []string{"typescript", "typescriptreact", "javascript", "javascriptreact"},
		RootMarkers: []string{"tsconfig.json", "jsconfig.json", "package.json", ".git"},
		Command:     slices.Clone(defaultTypeScriptServerCommand),
	}
}

func newTypeScriptTestManager(server ResolvedServer) *Manager {
	return &Manager{
		cfg:       Config{Servers: []ResolvedServer{server}},
		logger:    slog.New(slog.DiscardHandler),
		instances: make(map[string]*serverInstance),
		docs:      make(map[openDocKey]*docState),
		present:   make(map[string]bool),
		commands:  make(map[string]string),
	}
}

func installTypeScript(t *testing.T, root, version, binRelative string) string {
	t.Helper()
	packageDir := filepath.Join(root, "node_modules", "typescript")
	bin := filepath.Join(packageDir, filepath.FromSlash(binRelative))
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := map[string]any{
		"name":    "typescript",
		"version": version,
		"bin":     map[string]string{"tsc": binRelative},
	}
	data, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, bin, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
