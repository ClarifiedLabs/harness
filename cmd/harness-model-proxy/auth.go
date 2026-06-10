package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/modelproxy/server"
)

func runAuth(env environment, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.stderr, "harness-model-proxy: auth requires login, logout, or status")
		usageAuth(env.stderr)
		return exitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		usageAuth(env.stdout)
		return exitOK
	case "login", "logout", "status":
		return runAuthAction(env, args[0], args[1:])
	default:
		fmt.Fprintf(env.stderr, "harness-model-proxy: unknown auth command %q\n", args[0])
		usageAuth(env.stderr)
		return exitUsage
	}
}

func runAuthAction(env environment, action string, args []string) int {
	fs := flag.NewFlagSet("auth "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usageAuthAction(env.stdout, action)
			return exitOK
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(env.stderr, "harness-model-proxy: auth %s requires exactly one provider\n", action)
		return exitUsage
	}
	providerName := fs.Arg(0)
	cfgPath := server.ConfigPath(*configPath, flagWasSet(fs, "config"), env.getenv)
	if cfgPath == "" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: no config file found; run harness-model-proxy --setup")
		return exitUsage
	}
	pc, configDir, err := authProviderConfig(cfgPath, providerName)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: %v\n", err)
		return exitRuntime
	}
	if pc.Auth == nil {
		fmt.Fprintf(env.stderr, "harness-model-proxy: provider %q has no auth config\n", providerName)
		return exitUsage
	}

	ctx, cancel, interrupted := signalCancelContext(env.sigCh)
	defer cancel()
	switch action {
	case "login":
		err = auth.Login(ctx, *pc.Auth, auth.LoginOptions{
			Name:      pc.Name,
			ConfigDir: configDir,
			Getenv:    env.getenv,
			Client:    http.DefaultClient,
			Stdout:    env.stdout,
			Stderr:    env.stderr,
		})
	case "logout":
		err = auth.Logout(*pc.Auth, configDir, pc.Name)
		if err == nil {
			fmt.Fprintf(env.stdout, "Removed OAuth token for %s\n", pc.Name)
		}
	case "status":
		err = auth.Status(*pc.Auth, configDir, pc.Name, env.stdout, time.Now())
	}
	if err != nil {
		if interrupted() || errors.Is(err, context.Canceled) {
			return exitInterrupt
		}
		fmt.Fprintf(env.stderr, "harness-model-proxy: auth %s: %v\n", action, err)
		return exitRuntime
	}
	return exitOK
}

func usageAuth(w io.Writer) {
	fmt.Fprintln(w, "harness-model-proxy auth — manage provider OAuth tokens.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  harness-model-proxy auth <login|logout|status> [-config path] <provider>")
	fmt.Fprintln(w, "  harness-model-proxy auth login [-config path] <provider>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  login   Start OAuth login for a configured provider.")
	fmt.Fprintln(w, "  logout  Remove the stored OAuth token for a provider.")
	fmt.Fprintln(w, "  status  Print whether a provider has a usable stored OAuth token.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Provider auth types:")
	fmt.Fprintln(w, "  oauth2       Browser PKCE and device-code OAuth flows.")
	fmt.Fprintln(w, "  codex_oauth  OpenAI Codex ChatGPT subscription device-code login.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Examples:")
	fmt.Fprintln(w, "  harness-model-proxy --setup")
	fmt.Fprintln(w, "  harness-model-proxy auth login openai-codex")
	fmt.Fprintln(w, "  harness-model-proxy auth status openai-codex")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	printAuthFlags(w)
}

func usageAuthAction(w io.Writer, action string) {
	switch action {
	case "login":
		fmt.Fprintln(w, "harness-model-proxy auth login — sign in a configured provider.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  harness-model-proxy auth login [-config path] <provider>")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Starts the provider's configured OAuth flow and stores the token under the")
		fmt.Fprintln(w, "model-proxy config directory. oauth2 providers may use browser PKCE or")
		fmt.Fprintln(w, "device-code login. codex_oauth providers use OpenAI Codex's ChatGPT")
		fmt.Fprintln(w, "subscription device-code flow.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Example:")
		fmt.Fprintln(w, "  harness-model-proxy auth login openai-codex")
	case "logout":
		fmt.Fprintln(w, "harness-model-proxy auth logout — remove a configured provider's OAuth token.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  harness-model-proxy auth logout [-config path] <provider>")
	case "status":
		fmt.Fprintln(w, "harness-model-proxy auth status — inspect a configured provider's OAuth token.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  harness-model-proxy auth status [-config path] <provider>")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	printAuthFlags(w)
}

func printAuthFlags(w io.Writer) {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.String("config", "", "config file path")
	fs.PrintDefaults()
}

func authProviderConfig(configPath, providerName string) (llm.ProviderConfig, string, error) {
	cfg, err := server.LoadConfig(configPath)
	if err != nil {
		return llm.ProviderConfig{}, "", err
	}
	configDir := filepath.Dir(configPath)
	_, providers, err := llm.LoadProviderConfigs(configDir, cfg.ProviderConfigs, nil)
	if err != nil {
		return llm.ProviderConfig{}, "", err
	}
	for _, pc := range providers {
		if pc.Name == providerName {
			return pc, configDir, nil
		}
	}
	return llm.ProviderConfig{}, "", fmt.Errorf("provider %q is not configured", providerName)
}
