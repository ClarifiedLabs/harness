package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"harness/internal/auth"
	"harness/internal/cli"
	"harness/internal/llm"
	"harness/internal/modelproxy/server"
)

func runAuthAction(env environment, invocation cli.Invocation) int {
	action := strings.TrimPrefix(invocation.CommandID, "auth.")
	providerName := invocation.Args[0]
	configPath, _ := invocation.Flags.Last("config")
	cfgPath := server.ConfigPath(configPath, invocation.Flags.Has("config"), env.getenv)
	if cfgPath == "" {
		fmt.Fprintln(env.stderr, "harness-model-proxy: no config file found; run harness-model-proxy setup")
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
		if err == nil {
			if refreshErr := refreshProviderAfterLogin(ctx, env, cfgPath, pc); refreshErr != nil {
				fmt.Fprintf(env.stderr, "harness-model-proxy: auth login: warning: provider model refresh failed for %q: %v; login remains valid\n", pc.Name, refreshErr)
			}
		}
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
