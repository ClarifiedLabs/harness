// Package benchutil contains process and artifact helpers shared by benchmark
// drivers under scripts.
package benchutil

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// RunCommand captures stdout and stderr while running cmd.
func RunCommand(cmd *exec.Cmd) (stdout, stderr []byte, err error) {
	var stdoutBuffer, stderrBuffer bytes.Buffer
	cmd.Stdout = &stdoutBuffer
	cmd.Stderr = &stderrBuffer
	err = cmd.Run()
	return append([]byte(nil), stdoutBuffer.Bytes()...), append([]byte(nil), stderrBuffer.Bytes()...), err
}

// ExitStatus returns zero for success, the process exit code for an ExitError,
// and -1 when the command could not report an exit status.
func ExitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// WriteOutputFiles persists captured process output and returns one reason for
// each artifact that could not be written.
func WriteOutputFiles(stdoutPath, stderrPath string, stdout, stderr []byte) []string {
	var reasons []string
	if err := os.WriteFile(stdoutPath, stdout, 0o644); err != nil {
		reasons = append(reasons, fmt.Sprintf("write stdout artifact %s: %v", stdoutPath, err))
	}
	if err := os.WriteFile(stderrPath, stderr, 0o644); err != nil {
		reasons = append(reasons, fmt.Sprintf("write stderr artifact %s: %v", stderrPath, err))
	}
	return reasons
}
