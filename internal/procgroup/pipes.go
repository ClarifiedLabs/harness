package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Pipes are caller-owned stdio endpoints. In particular, Wait does not close
// output readers before the protocol/log readers have consumed buffered bytes.
type Pipes struct {
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
}

func (p *Pipes) Close() {
	_ = p.Stdin.Close()
	_ = p.Stdout.Close()
	_ = p.Stderr.Close()
}

// StartPiped starts a command with unconfigured stdio using caller-owned pipes.
// The caller closes Stdin to request shutdown and closes each output reader
// after draining it (or when abandoning the connection).
func StartPiped(cmd *exec.Cmd) (*Process, *Pipes, error) {
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, nil, errors.New("procgroup: StartPiped requires unconfigured stdio")
	}
	var files []*os.File
	started := false
	defer func() {
		if !started {
			for _, f := range files {
				_ = f.Close()
			}
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	stdinR, stdin, err := pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, stdoutW, err := pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, stderrW, err := pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	process, err := Start(cmd)
	if err != nil {
		return nil, nil, err
	}
	started = true
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	return process, &Pipes{Stdin: stdin, Stdout: stdout, Stderr: stderr}, nil
}
