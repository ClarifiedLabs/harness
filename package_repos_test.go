package harness_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func filepathGlobScripts() ([]string, error) {
	return filepath.Glob(filepath.Join("scripts", "release", "*.sh"))
}

// The repository-update scripts live in ClarifiedLabs/linux-packages; the
// release workflow runs them from a checkout of that repository, and the
// published names are the org-wide ones (clarifiedlabs-*, Label
// clarifiedlabs).
func TestReleaseWorkflowPublishesPackageRepos(t *testing.T) {
	workflow, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)

	for _, want := range []string{
		"client-id: ${{ secrets.PACKAGES_APP_CLIENT_ID }}",
		"private-key: ${{ secrets.PACKAGES_APP_PRIVATE_KEY }}",
		"PACKAGES_GPG_PRIVATE_KEY: ${{ secrets.PACKAGES_GPG_PRIVATE_KEY }}",
		"repository: ClarifiedLabs/linux-packages",
		"RPM_GPG_SIGN: '1'",
		"rpm --dbpath \"$rpmdb\" --import",
		"packages-publish-dry-run:",
		"linux-packages/clarifiedlabs-archive-keyring.asc",
		"linux-packages/scripts/apt-repo-update.sh",
		"linux-packages/scripts/rpm-repo-update.sh",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("release workflow should contain %q", want)
		}
	}

	publishStart := strings.Index(text, "\n  packages-publish:\n")
	if publishStart < 0 {
		t.Fatal("release workflow should define a packages-publish job")
	}
	publishEnd := len(text)
	jobBoundary := regexp.MustCompile(`\n  [a-z0-9-]+:\n`)
	if loc := jobBoundary.FindStringIndex(text[publishStart+1:]); loc != nil {
		publishEnd = publishStart + 1 + loc[0]
	}
	publishJob := text[publishStart:publishEnd]

	if !strings.Contains(publishJob, "if: ${{ startsWith(github.ref, 'refs/tags/v') }}") {
		t.Fatal("packages-publish job should be gated on v* tags")
	}
	if !strings.Contains(publishJob, "needs: publish") {
		t.Fatal("packages-publish job should wait for the release publish job")
	}

	for _, forbidden := range []string{
		"vars.PACKAGES_APP_CLIENT_ID",
		"app-id: ${{ secrets.PACKAGES_APP_CLIENT_ID }}",
		// Dry runs sign with the production key, like tag builds.
		"RPM_GPG_SIGN: ${{ startsWith(github.ref, 'refs/tags/v') && '1' || '0' }}",
		"gpg --batch --gen-key",
		"packages-dry-run@example.invalid",
		// Non-root rpm -K cannot see keys imported via sudo (per-user rpmdb).
		"sudo rpm --import",
		// Old harness-specific public names are gone org-wide.
		"harness-archive-keyring.asc",
		"scripts/release/apt-repo-update.sh",
		"scripts/release/rpm-repo-update.sh",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("release workflow should not contain %q", forbidden)
		}
	}

	// build-linux imports the signing key and signs RPMs on every run so
	// release-ci exercises the production signing path.
	buildLinuxStart := strings.Index(text, "\n  build-linux:\n")
	if buildLinuxStart < 0 {
		t.Fatal("release workflow should define a build-linux job")
	}
	buildLinuxEnd := len(text)
	if loc := jobBoundary.FindStringIndex(text[buildLinuxStart+1:]); loc != nil {
		buildLinuxEnd = buildLinuxStart + 1 + loc[0]
	}
	buildLinux := text[buildLinuxStart:buildLinuxEnd]
	importIdx := strings.Index(buildLinux, "- name: Import package signing key")
	packageIdx := strings.Index(buildLinux, "- name: Package archives")
	if importIdx < 0 || packageIdx < 0 || importIdx > packageIdx {
		t.Fatal("build-linux should import the package signing key before packaging")
	}
	importStep := buildLinux[importIdx:packageIdx]
	if strings.Contains(importStep, "refs/tags/v") {
		t.Fatal("build-linux signing key import should not be gated on tags; dry runs sign too")
	}

	// The dry-run publish job uses the production key (no throwaway key) and
	// fetches the org-wide keyring from the published Pages repository.
	dryRunStart := strings.Index(text, "\n  packages-publish-dry-run:\n")
	if dryRunStart < 0 {
		t.Fatal("release workflow should define a packages-publish-dry-run job")
	}
	dryRun := text[dryRunStart:]
	if !strings.Contains(dryRun, "PACKAGES_GPG_PRIVATE_KEY: ${{ secrets.PACKAGES_GPG_PRIVATE_KEY }}") {
		t.Fatal("packages-publish-dry-run should import the production signing key")
	}
	if !strings.Contains(dryRun, "clarifiedlabs.github.io/linux-packages/clarifiedlabs-archive-keyring.asc") &&
		!strings.Contains(dryRun, "raw.githubusercontent.com/ClarifiedLabs/linux-packages/main/clarifiedlabs-archive-keyring.asc") {
		t.Fatal("packages-publish-dry-run should verify against the published org-wide keyring")
	}
	if !strings.Contains(dryRun, "Label: clarifiedlabs") {
		t.Fatal("packages-publish-dry-run scratch distributions should use the org-wide Label")
	}
	if strings.Contains(dryRun, "export GNUPGHOME") {
		t.Fatal("packages-publish-dry-run should use the default keyring with the production key")
	}
}

// The release scripts the workflow invokes directly have to stay executable in
// git; the two repository-update scripts moved to linux-packages and must not
// linger here.
func TestReleaseScripts(t *testing.T) {
	scripts, err := filepathGlobScripts()
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) == 0 {
		t.Fatal("no scripts/release/*.sh files found")
	}
	for _, script := range scripts {
		info, err := os.Stat(script)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (mode %v)", script, info.Mode().Perm())
		}
	}
	if _, err := os.Stat("scripts/release/apt-repo-update.sh"); err == nil {
		t.Error("scripts/release/apt-repo-update.sh should live in ClarifiedLabs/linux-packages")
	}
	if _, err := os.Stat("scripts/release/rpm-repo-update.sh"); err == nil {
		t.Error("scripts/release/rpm-repo-update.sh should live in ClarifiedLabs/linux-packages")
	}
}
