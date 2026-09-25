package harness_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func requirePackageRepoTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("required tool %q not in PATH; skipping", tool)
		}
	}
}

// newPackageTestKey creates an isolated GNUPGHOME holding a single
// passphrase-less RSA signing key and returns an environment for commands
// that must use it.
func newPackageTestKey(t *testing.T) []string {
	t.Helper()
	gnupgHome := filepath.Join(t.TempDir(), "gnupg")
	if err := os.Mkdir(gnupgHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Stop the agent holding sockets inside the temp directory.
		_ = exec.Command("gpgconf", "--homedir", gnupgHome, "--kill", "gpg-agent").Run()
	})
	env := append(os.Environ(), "GNUPGHOME="+gnupgHome)
	params := `Key-Type: RSA
Key-Length: 2048
Name-Real: Harness Test Packages
Name-Email: harness-packages-test@example.invalid
Expire-Date: 0
%no-protection
%commit
`
	cmd := exec.Command("gpg", "--batch", "--gen-key")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(params)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate test signing key: %v\n%s", err, output)
	}
	return env
}

// writeTestDistributions seeds a scratch reprepro base directory with the same
// conf/distributions content as the real linux-packages repository.
func writeTestDistributions(t *testing.T, repoDir string) {
	t.Helper()
	confDir := filepath.Join(repoDir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const distributions = `Origin: Clarified Labs, Inc.
Label: harness
Suite: stable
Codename: stable
Components: main
Architectures: amd64 arm64
SignWith: yes
`
	if err := os.WriteFile(filepath.Join(confDir, "distributions"), []byte(distributions), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildDummyDeb(t *testing.T, distDir, name, version, arch string) {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\necho "+name+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	debianDir := filepath.Join(root, "DEBIAN")
	if err := os.Mkdir(debianDir, 0o755); err != nil {
		t.Fatal(err)
	}
	control := "Package: " + name + "\n" +
		"Version: " + version + "\n" +
		"Section: utils\n" +
		"Priority: optional\n" +
		"Architecture: " + arch + "\n" +
		"Maintainer: Harness Test Packages <harness-packages-test@example.invalid>\n" +
		"Description: test package " + name + "\n"
	if err := os.WriteFile(filepath.Join(debianDir, "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	debPath := filepath.Join(distDir, name+"_"+version+"_"+arch+".deb")
	cmd := exec.Command("dpkg-deb", "--build", "--root-owner-group", root, debPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dpkg-deb --build %s: %v\n%s", name, err, output)
	}
}

func countFilesWithSuffix(t *testing.T, root, suffix string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, suffix) {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func repreproListStable(t *testing.T, repoDir string) string {
	t.Helper()
	cmd := exec.Command("reprepro", "-b", repoDir, "list", "stable")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reprepro list stable: %v\n%s", err, output)
	}
	return string(output)
}

func TestAptRepoUpdateScript(t *testing.T) {
	requirePackageRepoTools(t, "reprepro", "gpg", "dpkg-deb")
	env := newPackageTestKey(t)

	workDir := t.TempDir()
	repoRoot := filepath.Join(workDir, "linux-packages")
	repoDir := filepath.Join(repoRoot, "deb")
	writeTestDistributions(t, repoDir)

	distDir := filepath.Join(workDir, "dist")
	if err := os.Mkdir(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	buildDummyDeb(t, distDir, "harness-test-one", "0.0.0", "amd64")
	buildDummyDeb(t, distDir, "harness-test-two", "0.0.0", "arm64")

	runScript := func() {
		cmd := exec.Command("bash", "scripts/release/apt-repo-update.sh")
		cmd.Env = append(env, "REPO_DIR="+repoDir, "DIST_DIR="+distDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("apt-repo-update.sh: %v\n%s", err, output)
		}
	}
	runScript()

	// The signed InRelease metadata verifies against the test key.
	verify := exec.Command("gpg", "--batch", "--verify", filepath.Join(repoDir, "dists", "stable", "InRelease"))
	verify.Env = env
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("gpg --verify InRelease: %v\n%s", err, output)
	}

	check := exec.Command("reprepro", "-b", repoDir, "check")
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("reprepro check: %v\n%s", err, output)
	}

	list := repreproListStable(t, repoDir)
	for _, want := range []string{
		"stable|main|amd64: harness-test-one 0.0.0",
		"stable|main|arm64: harness-test-two 0.0.0",
	} {
		if !strings.Contains(list, want) {
			t.Fatalf("reprepro list stable missing %q:\n%s", want, list)
		}
	}

	if got := countFilesWithSuffix(t, filepath.Join(repoDir, "pool"), ".deb"); got != 2 {
		t.Fatalf("expected 2 debs in pool, got %d", got)
	}

	// The repo root gained the exported keyring and the Pages marker.
	if info, err := os.Stat(filepath.Join(repoRoot, "harness-archive-keyring.asc")); err != nil || info.Size() == 0 {
		t.Fatalf("harness-archive-keyring.asc missing or empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".nojekyll")); err != nil {
		t.Fatalf(".nojekyll missing: %v", err)
	}

	// Re-running is a no-op: no error, no duplicates.
	runScript()
	if got := countFilesWithSuffix(t, filepath.Join(repoDir, "pool"), ".deb"); got != 2 {
		t.Fatalf("expected 2 debs in pool after re-run, got %d", got)
	}
	list = repreproListStable(t, repoDir)
	if got := strings.Count(list, "harness-test-one"); got != 1 {
		t.Fatalf("expected exactly one harness-test-one entry after re-run, got %d:\n%s", got, list)
	}
	if got := strings.Count(list, "harness-test-two"); got != 1 {
		t.Fatalf("expected exactly one harness-test-two entry after re-run, got %d:\n%s", got, list)
	}
}

func TestPackageRpmSignsAndRepoUpdate(t *testing.T) {
	requirePackageRepoTools(t, "rpmbuild", "rpm", "createrepo_c", "gpg")
	env := newPackageTestKey(t)

	workDir := t.TempDir()
	stageDir := filepath.Join(workDir, "stage")
	if err := os.Mkdir(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binaries := []string{"harness", "harness-model-proxy", "harness-mcp-proxy"}
	for _, binary := range binaries {
		stub := "#!/bin/sh\necho \"" + binary + " v0.0.0\"\n"
		if err := os.WriteFile(filepath.Join(stageDir, binary), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	distDir := filepath.Join(workDir, "dist")
	build := exec.Command("bash", "scripts/release/package-rpm.sh")
	build.Env = append(env,
		"VERSION=v0.0.0",
		"GOARCH=amd64",
		"RPM_GPG_SIGN=1",
		"STAGE_DIR="+stageDir,
		"DIST_DIR="+distDir,
		"WORK_DIR="+filepath.Join(workDir, "rpmbuild-work"),
	)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("package-rpm.sh: %v\n%s", err, output)
	}

	rpmNames := []string{
		"harness-0.0.0-1.x86_64.rpm",
		"harness-model-proxy-0.0.0-1.x86_64.rpm",
		"harness-mcp-proxy-0.0.0-1.x86_64.rpm",
	}
	for _, rpmName := range rpmNames {
		rpmPath := filepath.Join(distDir, rpmName)
		if _, err := os.Stat(rpmPath); err != nil {
			t.Fatalf("expected rpm %s: %v", rpmPath, err)
		}
		cmd := exec.Command("rpm", "-qpi", rpmPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("rpm -qpi %s: %v\n%s", rpmName, err, output)
		}
		signature := ""
		for line := range strings.Lines(string(output)) {
			if strings.HasPrefix(line, "Signature") {
				signature = line
			}
		}
		if signature == "" || strings.Contains(signature, "(none)") {
			t.Fatalf("%s is not signed (Signature line %q):\n%s", rpmName, signature, output)
		}
	}

	repoRoot := filepath.Join(workDir, "linux-packages")
	runUpdate := func() {
		cmd := exec.Command("bash", "scripts/release/rpm-repo-update.sh")
		cmd.Env = append(env, "REPO_DIR="+repoRoot, "DIST_DIR="+distDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rpm-repo-update.sh: %v\n%s", err, output)
		}
	}
	runUpdate()

	repomdPath := filepath.Join(repoRoot, "rpm", "x86_64", "repodata", "repomd.xml")
	for _, path := range []string{repomdPath, repomdPath + ".asc"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}
	verify := exec.Command("gpg", "--batch", "--verify", repomdPath+".asc", repomdPath)
	verify.Env = env
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("gpg --verify repomd.xml: %v\n%s", err, output)
	}

	// rpm -K against an isolated rpmdb holding only the test public key.
	exportKey := exec.Command("gpg", "--batch", "--armor", "--export")
	exportKey.Env = env
	pubKey, err := exportKey.Output()
	if err != nil {
		t.Fatalf("export test public key: %v", err)
	}
	pubKeyPath := filepath.Join(workDir, "pubkey.asc")
	if err := os.WriteFile(pubKeyPath, pubKey, 0o644); err != nil {
		t.Fatal(err)
	}
	rpmDBPath := filepath.Join(workDir, "rpmdb")
	if err := os.Mkdir(rpmDBPath, 0o755); err != nil {
		t.Fatal(err)
	}
	importKey := exec.Command("rpm", "--dbpath", rpmDBPath, "--import", pubKeyPath)
	if output, err := importKey.CombinedOutput(); err != nil {
		t.Fatalf("rpm --import: %v\n%s", err, output)
	}
	for _, rpmName := range rpmNames {
		repoRPM := filepath.Join(repoRoot, "rpm", "x86_64", rpmName)
		cmd := exec.Command("rpm", "--dbpath", rpmDBPath, "-K", repoRPM)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("rpm -K %s: %v\n%s", repoRPM, err, output)
		}
		if !strings.Contains(string(output), "signatures OK") {
			t.Fatalf("rpm -K %s did not report signatures OK:\n%s", repoRPM, output)
		}
	}

	// Re-running the repo update is a no-op: repomd.xml is untouched.
	repomdHash := sha256File(t, repomdPath)
	runUpdate()
	if after := sha256File(t, repomdPath); after != repomdHash {
		t.Fatal("repomd.xml changed on no-op re-run")
	}
}

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
		"RPM_GPG_SIGN: ${{ startsWith(github.ref, 'refs/tags/v') && '1' || '0' }}",
		"packages-publish-dry-run:",
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
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("release workflow should not contain %q", forbidden)
		}
	}
}
