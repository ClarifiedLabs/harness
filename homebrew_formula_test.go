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

func TestHomebrewBottleMerge(t *testing.T) {
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
	}{
		{"all platforms", ""},
		{"missing metadata", "Invalid or missing bottle metadata"},
		{"bad archive", "Missing or mismatched bottle archive"},
		{"checksum command failure", "Missing or mismatched bottle archive"},
		{"incomplete merge", "Merged formulas do not advertise the expected bottle platforms and checksums"},
		{"incorrect merged hash", "Merged formulas do not advertise the expected bottle platforms and checksums"},
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
			for _, formula := range []string{"harness", "harness-model-proxy", "harness-mcp-proxy", "harness-full"} {
				name := "clarifiedlabs/tap/" + formula
				files := object{}
				for _, platform := range []string{"arm64_tahoe", "arm64_linux", "x86_64_linux"} {
					base := formula + "--0.5.67." + platform + ".bottle"
					archive := base + ".tar.gz"
					checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(archive)))
					tag := object{"sha256": checksum, "local_filename": archive}
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
					if target && tc.name == "bad archive" {
						contents = "not the checksummed bytes"
					}
					write(filepath.Join(dumpDir, archive), []byte(contents), 0o644)
					if target && tc.name == "incomplete merge" {
						continue
					}
					if target && tc.name == "incorrect merged hash" {
						checksum = strings.Repeat("0", 64)
					}
					files[platform] = object{"sha256": checksum}
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
			cmd := exec.Command("bash", "scripts/release/homebrew-bottles.sh", dumpDir)
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TAP_DIR="+root, "TAG=v0.5.67", "GITHUB_REPOSITORY=ClarifiedLabs/harness",
				"CALL_MARKER="+filepath.Join(root, "merged"), "MOCK_BREW_INFO="+string(encoded),
				"REAL_SHA256SUM="+sha256sum, fmt.Sprintf("FAIL_CHECKSUM=%t", tc.name == "checksum command failure"))
			output, err := cmd.CombinedOutput()
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("merge complete bottle set: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(string(output), tc.wantError) {
				t.Fatalf("wanted error %q, got %v\n%s", tc.wantError, err, output)
			}
		})
	}
}
