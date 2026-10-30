package harness_test

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseTarballIncludesDependencyNotices(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "bin")
	if err := os.Mkdir(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"harness", "harness-model-proxy", "harness-mcp-proxy"} {
		if err := os.WriteFile(filepath.Join(stage, name), []byte("test binary\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "scripts/release/package-tarball.sh")
	cmd.Env = append(os.Environ(), "VERSION=v0.0.0", "GOOS=linux", "GOARCH=arm64",
		"STAGE_DIR="+stage, "DIST_DIR="+root, "WORK_DIR="+filepath.Join(root, "work"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package tarball: %v\n%s", err, output)
	}
	f, err := os.Open(filepath.Join(root, "harness_v0.0.0_linux_arm64.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(header.Name)
		if name != "LICENSE" && name != "THIRD_PARTY_NOTICES.md" {
			continue
		}
		got, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("archive %s differs from source", name)
		}
		found[name] = true
	}
	for _, name := range []string{"LICENSE", "THIRD_PARTY_NOTICES.md"} {
		if !found[name] {
			t.Errorf("archive missing %s", name)
		}
	}
}

func TestReleasePackagesInstallDependencyNotices(t *testing.T) {
	for script, want := range map[string]string{
		"package-deb.sh": `install -m 0644 THIRD_PARTY_NOTICES.md "${pkgroot}/usr/share/doc/${name}/THIRD_PARTY_NOTICES.md"`,
		"package-rpm.sh": `%license /usr/share/licenses/${name}/THIRD_PARTY_NOTICES.md`,
		"package-pkg.sh": `install -m 0644 THIRD_PARTY_NOTICES.md "${payload}/usr/local/share/doc/harness/THIRD_PARTY_NOTICES.md"`,
	} {
		t.Run(script, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("scripts", "release", script))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), want) {
				t.Fatalf("package script missing notice installation: %s", want)
			}
		})
	}
	notices, err := os.ReadFile("THIRD_PARTY_NOTICES.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github.com/ClarifiedLabs/mdcli", "Permission is hereby granted", "THE SOFTWARE IS PROVIDED"} {
		if !strings.Contains(string(notices), want) {
			t.Errorf("dependency notices missing %q", want)
		}
	}
}
