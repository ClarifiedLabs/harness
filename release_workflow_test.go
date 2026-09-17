package harness_test

import (
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
		})
	}
}

func TestReleaseWorkflowUsesSecretForHomebrewTapAppClientID(t *testing.T) {
	workflow, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)

	const want = "client-id: ${{ secrets.HOMEBREW_TAP_APP_CLIENT_ID }}"
	if !strings.Contains(text, want) {
		t.Fatalf("release workflow should use %q", want)
	}

	for _, forbidden := range []string{
		"vars.HOMEBREW_TAP_APP_CLIENT_ID",
		"app-id: ${{ secrets.HOMEBREW_TAP_APP_CLIENT_ID }}",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("release workflow should not contain %q", forbidden)
		}
	}
}
