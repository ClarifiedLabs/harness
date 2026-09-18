package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"harness/internal/cli"
	"harness/internal/config"
	modelclient "harness/internal/modelproxy/client"
	"harness/internal/modelproxy/protocol"
	"harness/internal/ui"
)

func limitsCLIFlags(reset bool) []cli.Flag {
	flags := []cli.Flag{mustConfigCLIFlag("config"), mustConfigCLIFlag("model_proxy_url"), mustConfigCLIFlag("model_proxy_api_key"), valueCLIFlag("format", []string{"format"}, "format", "output format: text or json", "text")}
	if reset {
		flags = append(flags, valueCLIFlag("request_id", []string{"request-id"}, "id", "stable reset request ID; reuse for a retry", ""))
	}
	return flags
}

// Keep the shared parser's first-positional semantics unchanged for every other
// command, while allowing the documented trailing flags on limits commands.
func normalizeLimitsCommandArgs(args []string) []string {
	if len(args) == 0 || args[0] != "limits" {
		return args
	}
	out := []string{"limits"}
	var subcommand string
	var flags []string
	var positional []string
	for i := 1; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(token, "-") && token != "-" {
			flags = append(flags, token)
			name := strings.TrimLeft(token, "-")
			if !strings.Contains(name, "=") && name != "h" && name != "help" {
				if i+1 == len(args) {
					if subcommand != "" {
						out = append(out, subcommand)
					}
					return append(out, flags...)
				} // preserve the parser's missing-value error
				i++
				flags = append(flags, args[i])
			}
		} else if subcommand == "" && len(positional) == 0 && (token == "reset" || token == "resets") {
			subcommand = token
		} else {
			positional = append(positional, token)
		}
	}
	if subcommand != "" {
		out = append(out, subcommand)
	}
	out = append(out, flags...)
	out = append(out, "--")
	return append(out, positional...)
}

// JSON escapes ASCII controls itself, but not C1 terminal controls. Escape those
// too without changing decoded values or mixing human retry prose into stdout.
func writeLimitsJSON(w io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range string(data) {
		if r >= 0x80 && r <= 0x9f {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	b.WriteByte('\n')
	_, err = io.WriteString(w, b.String())
	return err
}

func runLimits(env environment, invocation cli.Invocation) int {
	format := strings.ToLower(strings.TrimSpace(cliLast(invocation.Flags, "format", "text")))
	if format != "text" && format != "json" {
		fmt.Fprintln(env.stderr, "harness: limits: -format must be text or json")
		return ui.ExitUsage
	}
	provider := ""
	if len(invocation.Args) > 0 {
		provider = invocation.Args[0]
	}
	// Any provider may be named for reset credits; the proxy validates that the
	// provider resolves to the codex quota profile.
	var request protocol.ResetRequest
	if invocation.CommandID == "limits.reset" {
		request = protocol.ResetRequest{Provider: provider, CreditID: invocation.Args[1], RequestID: cliLast(invocation.Flags, "request_id", "")}
		if !invocation.Flags.Has("request_id") {
			var err error
			request.RequestID, err = protocol.NewResetRequestID()
			if err != nil {
				fmt.Fprintln(env.stderr, "harness: limits: cannot generate request ID")
				return ui.ExitRuntime
			}
		}
		if err := request.Validate(); err != nil {
			fmt.Fprintf(env.stderr, "harness: limits: %v\n", err)
			return ui.ExitUsage
		}
	}
	cfg, err := config.LoadParsed(harnessLoadOptions(env, nil), invocation.Flags)
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: limits: %v\n", err)
		return ui.ExitUsage
	}
	client, err := modelclient.New(cfg.Config.ModelProxyURL, nil, modelclient.WithAPIKey(cfg.Config.ModelProxyAPIKey))
	if err != nil {
		fmt.Fprintln(env.stderr, "harness: limits: invalid model proxy URL")
		return ui.ExitUsage
	}
	ctx, stop, interrupted := signalCancelContext(env.sigCh)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, ui.LimitsCommandTimeout)
	defer cancel()
	now := time.Now
	if env.now != nil {
		now = env.now
	}
	var value any
	var text string
	code := ui.ExitOK
	switch invocation.CommandID {
	case "limits":
		var report protocol.LimitsReport
		report, err = client.Limits(ctx, provider)
		value = report
		if err == nil || report.Providers != nil {
			text = ui.FormatLimits(report, now())
		}
	case "limits.resets":
		var credits protocol.ResetCredits
		credits, err = client.ResetCredits(ctx, provider)
		value = credits
		if err == nil || credits.Provider != "" {
			text = ui.FormatResetCredits(credits)
		}
	case "limits.reset":
		var result protocol.ResetResult
		result, err = client.ResetLimits(ctx, request)
		result = ui.NormalizeResetResult(request, result, err)
		value, text = result, ui.FormatResetResult(result, now())
		code = ui.ResetExitCode(result)
		if format == "json" && ui.ResetIsIndeterminate(result) {
			fmt.Fprint(env.stderr, ui.ResetRetryCommands(request))
		}
	}
	if format == "json" {
		// Preserve structured request errors even when the proxy returned no data.
		if err != nil && text == "" {
			var detail *protocol.LimitsError
			if !errors.As(err, &detail) {
				detail = &protocol.LimitsError{Code: "proxy_error", Message: "model proxy limits request failed"}
			}
			value = struct {
				Error *protocol.LimitsError `json:"error"`
			}{Error: detail}
		}
		if writeErr := writeLimitsJSON(env.stdout, value); writeErr != nil {
			fmt.Fprintf(env.stderr, "harness: limits: write output: %v\n", writeErr)
			code = ui.ExitRuntime
		}
	} else if _, writeErr := fmt.Fprint(env.stdout, text); writeErr != nil {
		fmt.Fprintf(env.stderr, "harness: limits: write output: %v\n", writeErr)
		code = ui.ExitRuntime
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "harness: limits: %s\n", protocol.LimitsLabel(err.Error()))
		code = ui.ExitRuntime
	}
	if interrupted() {
		return ui.ExitInterrupt
	}
	return code
}
