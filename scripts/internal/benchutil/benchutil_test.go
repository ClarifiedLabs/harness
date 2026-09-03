package benchutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCommandCapturesOutputAndExitStatus(t *testing.T) {
	cmd := exec.Command("sh", "-c", "printf stdout; printf stderr >&2; exit 7")
	stdout, stderr, err := RunCommand(cmd)
	if string(stdout) != "stdout" || string(stderr) != "stderr" {
		t.Fatalf("captured output = %q / %q", stdout, stderr)
	}
	if got := ExitStatus(err); got != 7 {
		t.Fatalf("ExitStatus = %d, want 7", got)
	}
}

func TestWriteOutputFilesReportsArtifactFailures(t *testing.T) {
	dir := t.TempDir()
	stderrPath := filepath.Join(dir, "stderr.txt")
	reasons := WriteOutputFiles(dir, stderrPath, []byte("stdout"), []byte("stderr"))
	if len(reasons) != 1 || !strings.Contains(reasons[0], "write stdout artifact") {
		t.Fatalf("reasons = %v, want stdout write failure", reasons)
	}
	got, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "stderr" {
		t.Fatalf("stderr artifact = %q", got)
	}
}
