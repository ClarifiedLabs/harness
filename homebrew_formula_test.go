package harness_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHomebrewFormulaeDisableGoWorkspace(t *testing.T) {
	tapDir := t.TempDir()
	cmd := exec.Command("bash", "scripts/release/homebrew-formula.sh")
	cmd.Env = append(os.Environ(),
		"TAG=v0.0.0",
		"SOURCE_SHA256="+strings.Repeat("0", 64),
		"TAP_DIR="+tapDir,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate Homebrew formulae: %v\n%s", err, output)
	}

	for _, name := range []string{"harness", "harness-model-proxy", "harness-mcp-proxy"} {
		t.Run(name, func(t *testing.T) {
			formula, err := os.ReadFile(filepath.Join(tapDir, "Formula", name+".rb"))
			if err != nil {
				t.Fatal(err)
			}
			// Disable workspace discovery at the start of install so Go cannot
			// pick up an unrelated go.work above Homebrew's build directory.
			const want = "  def install\n    ENV[\"GOWORK\"] = \"off\"\n"
			if !strings.Contains(string(formula), want) {
				t.Fatalf("formula should start install with GOWORK=off:\n%s", formula)
			}
			if !strings.Contains(string(formula), `doc.install "THIRD_PARTY_NOTICES.md"`) {
				t.Fatalf("formula should install dependency notices:\n%s", formula)
			}
		})
	}
}

func TestHomebrewBottles(t *testing.T) {
	// The release jobs run on Linux with these tools preinstalled. Keep the
	// regression test runnable without installing Homebrew or building bottles.
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required", tool)
		}
	}

	sha256sum, err := exec.LookPath("sha256sum")
	if err != nil {
		t.Skip("sha256sum is required")
	}

	for _, tc := range []struct {
		name, wantError string
		stage           bool
	}{
		{"all platforms", "", false},
		{"missing metadata", "Invalid or missing bottle metadata", false},
		{"bad archive", "Missing or mismatched bottle archive", false},
		{"checksum command failure", "Missing or mismatched bottle archive", false},
		{"incomplete merge", "Merged formulas do not advertise the expected bottle platforms, checksums and URLs", false},
		{"incorrect merged hash", "Merged formulas do not advertise the expected bottle platforms, checksums and URLs", false},
		{"incorrect merged URL", "Merged formulas do not advertise the expected bottle platforms, checksums and URLs", false},
		{"stage release filenames", "", true},
		{"invalid release filename", "Invalid or missing bottle metadata", true},
		{"release filename collision", "Release bottle archive already exists", true},
		{"invalid release archive", "Missing or mismatched bottle archive", true},
		{"public downloads resolve", "", false},
		{"public download unavailable", "Public release bottle unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dumpDir := filepath.Join(root, "artifacts")
			if err := os.Mkdir(dumpDir, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(name string, contents []byte, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(name, contents, mode); err != nil {
					t.Fatal(err)
				}
			}
			const rootURL = "https://github.com/ClarifiedLabs/harness/releases/download/v0.5.67"
			type object = map[string]any
			var brewInfo []object
			releaseArchives := map[string]string{}
			collision := filepath.Join(dumpDir, "harness-full-0.5.67.x86_64_linux.bottle.tar.gz")
			for _, formula := range []string{"harness", "harness-model-proxy", "harness-mcp-proxy", "harness-full"} {
				name := "clarifiedlabs/tap/" + formula
				files := object{}
				for _, platform := range []string{"arm64_tahoe", "arm64_linux", "x86_64_linux"} {
					base := formula + "--0.5.67." + platform + ".bottle"
					archive := base + ".tar.gz"
					published := formula + "-0.5.67." + platform + ".bottle.tar.gz"
					releaseArchives[published] = archive
					checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(archive)))
					last := formula == "harness-full" && platform == "x86_64_linux"
					tag := object{"sha256": checksum, "local_filename": archive, "filename": published}
					if last && tc.name == "invalid release filename" {
						tag["filename"] = "../" + published
					}
					if last && tc.name == "release filename collision" {
						write(collision, []byte("existing archive"), 0o644)
					}
					metadata := object{name: object{
						"formula": object{"pkg_version": "0.5.67"},
						"bottle":  object{"root_url": rootURL, "tags": object{platform: tag}},
					}}
					encoded, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					target := formula == "harness" && platform == "arm64_tahoe"
					if !target || tc.name != "missing metadata" {
						write(filepath.Join(dumpDir, base+".json"), encoded, 0o644)
					}
					contents := archive
					if (target && tc.name == "bad archive") || (last && tc.name == "invalid release archive") {
						contents = "not the checksummed bytes"
					}
					write(filepath.Join(dumpDir, archive), []byte(contents), 0o644)
					if target && tc.name == "incomplete merge" {
						continue
					}
					if target && tc.name == "incorrect merged hash" {
						checksum = strings.Repeat("0", 64)
					}
					url := rootURL + "/" + published
					if target && tc.name == "incorrect merged URL" {
						url = rootURL + "/" + archive
					}
					files[platform] = object{"sha256": checksum, "url": url}
				}
				brewInfo = append(brewInfo, object{
					"full_name": name,
					"bottle":    object{"stable": object{"root_url": rootURL, "files": files}},
				})
			}
			encoded, err := json.Marshal(object{"formulae": brewInfo})
			if err != nil {
				t.Fatal(err)
			}
			// This sha256sum accepts stdin but rejects GNU-only options.
			write(filepath.Join(root, "sha256sum"), []byte(`#!/usr/bin/env bash
set -euo pipefail
if (( $# != 0 )); then
  echo "usage: sha256sum [-bctwz] [files ...]" >&2
  exit 1
fi
"$REAL_SHA256SUM"
if [[ "$FAIL_CHECKSUM" == "true" ]]; then
  exit 1
fi
`), 0o755)
			write(filepath.Join(root, "brew"), []byte(`#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  bottle)
    [[ ! -e "$CALL_MARKER" && $# -eq 17 ]]
    [[ "$*" == "bottle --merge --write --no-commit --no-all-checks "* ]]
    touch "$CALL_MARKER"
    ;;
  info)
    [[ -e "$CALL_MARKER" && $# -eq 6 && "$2" == "--json=v2" ]]
    printf '%s\n' "$MOCK_BREW_INFO"
    ;;
  *) exit 1 ;;
esac
`), 0o755)
			downloadsLog := filepath.Join(root, "download-urls")
			verifyDownloads := "0"
			if strings.HasPrefix(tc.name, "public download") {
				verifyDownloads = "1"
			}
			write(filepath.Join(root, "curl"), []byte(`#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "--head" ]]
printf '%s\n' "${!#}" >> "$DOWNLOADS_LOG"
[[ "$FAIL_DOWNLOAD" == "false" ]]
`), 0o755)
			args := []string{"scripts/release/homebrew-bottles.sh"}
			if tc.stage {
				args = append(args, "--stage-release")
			}
			cmd := exec.Command("bash", append(args, dumpDir)...)
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TAP_DIR="+root, "TAG=v0.5.67", "GITHUB_REPOSITORY=ClarifiedLabs/harness",
				"CALL_MARKER="+filepath.Join(root, "merged"), "MOCK_BREW_INFO="+string(encoded),
				"REAL_SHA256SUM="+sha256sum, fmt.Sprintf("FAIL_CHECKSUM=%t", tc.name == "checksum command failure"),
				"DOWNLOADS_LOG="+downloadsLog, fmt.Sprintf("FAIL_DOWNLOAD=%t", tc.wantError != ""),
				"VERIFY_RELEASE_DOWNLOADS="+verifyDownloads)
			output, err := cmd.CombinedOutput()
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("prepare complete bottle set: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(string(output), tc.wantError) {
				t.Fatalf("wanted error %q, got %v\n%s", tc.wantError, err, output)
			}
			if verifyDownloads == "1" {
				urls, err := os.ReadFile(downloadsLog)
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Fields(string(urls))
				if tc.wantError != "" {
					if len(lines) != 1 {
						t.Fatalf("failed URL must abort the checks: %q", lines)
					}
					if _, err := os.Stat(filepath.Join(root, "merged")); !os.IsNotExist(err) {
						t.Fatalf("failed URL must prevent formula merge: %v", err)
					}
				} else {
					if len(lines) != len(releaseArchives) {
						t.Fatalf("expected %d downloads checked, got %d", len(releaseArchives), len(lines))
					}
					for _, url := range lines {
						published, ok := strings.CutPrefix(url, rootURL+"/")
						if !ok || releaseArchives[published] == "" {
							t.Fatalf("unexpected checked download URL %q", url)
						}
						delete(releaseArchives, published)
					}
				}
			}
			if !tc.stage {
				return
			}
			if _, err := os.Stat(filepath.Join(root, "merged")); !os.IsNotExist(err) {
				t.Fatalf("release staging must not run brew: %v", err)
			}
			for published, archive := range releaseArchives {
				localPath := filepath.Join(dumpDir, archive)
				publicPath := filepath.Join(dumpDir, published)
				if tc.wantError == "" {
					data, err := os.ReadFile(publicPath)
					if err != nil || string(data) != archive {
						t.Fatalf("%s: expected unchanged bytes at published name, got %q (%v)", published, data, err)
					}
					localPath, publicPath = publicPath, localPath
				}
				if _, err := os.Stat(localPath); err != nil {
					t.Fatalf("%s: expected archive to remain: %v", localPath, err)
				}
				if publicPath == collision && tc.name == "release filename collision" {
					data, err := os.ReadFile(publicPath)
					if err != nil || string(data) != "existing archive" {
						t.Fatalf("collision must not overwrite existing archive: %q (%v)", data, err)
					}
				} else if _, err := os.Stat(publicPath); !os.IsNotExist(err) {
					t.Fatalf("%s: expected archive to be absent: %v", publicPath, err)
				}
			}
		})
	}
}
