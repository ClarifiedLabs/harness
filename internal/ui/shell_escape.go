package ui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func (app *App) runShellEscape(command string) {
	err := app.runShellEscapeWithHandoff(command)
	if err != nil {
		fmt.Fprintf(app.Errw, "[shell failed: %v]\n", err)
	}
}

func (app *App) runShellEscapeWithHandoff(command string) error {
	if app.BeforeEditor != nil {
		app.BeforeEditor()
	}
	err := app.runShellCommand(command)
	if app.AfterEditor != nil {
		app.AfterEditor()
	}
	return err
}

func (app *App) runShellCommand(command string) error {
	run := app.RunShellCommand
	if run == nil {
		run = app.defaultRunShellCommand
	}
	return run(command)
}

func (app *App) defaultRunShellCommand(line string) error {
	cmd := shellEscapeCommand(line)
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err == nil {
		defer tty.Close()
		cmd.Stdin = tty
		cmd.Stdout = tty
		cmd.Stderr = tty
	} else {
		cmd.Stdout = app.Errw
		cmd.Stderr = app.Errw
	}
	err = cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}

func shellEscapeCommand(line string) *exec.Cmd {
	if shell := os.Getenv("SHELL"); shell != "" {
		if resolved, err := exec.LookPath(shell); err == nil {
			return exec.Command(resolved, "-lc", line) // nosemgrep: dangerous-exec-command
		}
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		return exec.Command(bash, "-lc", line) // nosemgrep: dangerous-exec-command
	}
	return exec.Command("sh", "-c", line) // nosemgrep: dangerous-exec-command
}
