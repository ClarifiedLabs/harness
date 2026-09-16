package ui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"harness/internal/modelproxy/protocol"
)

// LimitsCommandTimeout includes a reset and its best-effort status refreshes.
const LimitsCommandTimeout = 45 * time.Second

// FormatLimits is shared by CLI and REPL; unknown values stay unknown.
func FormatLimits(report protocol.LimitsReport, now time.Time) string {
	var b strings.Builder
	providers := append([]protocol.ProviderLimits(nil), report.Providers...)
	sort.SliceStable(providers, func(i, j int) bool { return providers[i].Provider < providers[j].Provider })
	if len(providers) == 0 {
		b.WriteString("No supported subscription providers configured.\n")
	}
	for _, p := range providers {
		fmt.Fprintf(&b, "%s", protocol.LimitsLabel(p.Provider))
		if p.Plan != "" {
			fmt.Fprintf(&b, " — %s", protocol.LimitsLabel(p.Plan))
		}
		fmt.Fprintf(&b, " (fetched %s)\n", limitsTime(p.FetchedAt))
		limitsErrors(&b, p.Error, p.Warnings)
		for _, pool := range p.Pools {
			fmt.Fprintf(&b, "  %s", limitsName(pool.Name, pool.ID))
			if pool.Allowed != nil {
				fmt.Fprintf(&b, "; allowed: %t", *pool.Allowed)
			}
			if pool.LimitReached != nil {
				fmt.Fprintf(&b, "; limit reached: %t", *pool.LimitReached)
			}
			b.WriteByte('\n')
			for _, w := range pool.Windows {
				fmt.Fprintf(&b, "    %s:", limitsName(w.Name, w.ID))
				known := false
				for _, f := range []struct {
					name   string
					value  *float64
					suffix string
				}{
					{"used", w.UsedPercent, "%"}, {"remaining", w.RemainingPercent, "%"},
					{"used", w.Used, " " + limitsName(w.Unit, "provider units")}, {"remaining", w.Remaining, " " + limitsName(w.Unit, "provider units")}, {"limit", w.Limit, " " + limitsName(w.Unit, "provider units")},
				} {
					if f.value != nil {
						fmt.Fprintf(&b, " %s %g%s;", f.name, *f.value, f.suffix)
						known = true
					}
				}
				if !known {
					b.WriteString(" usage unknown;")
				}
				if w.DurationSeconds != nil {
					fmt.Fprintf(&b, " window %ds;", *w.DurationSeconds)
				}
				if w.ResetAt != nil {
					fmt.Fprintf(&b, " resets %s (%s)", limitsTime(*w.ResetAt), limitsCountdown(*w.ResetAt, now))
				} else if w.ResetAfterSeconds != nil {
					fmt.Fprintf(&b, " resets in %ds (provider reported at fetch)", *w.ResetAfterSeconds)
				} else {
					b.WriteString(" reset unknown")
				}
				b.WriteByte('\n')
			}
		}
		if p.ResetCredits != nil {
			fmt.Fprintf(&b, "  reset credits available: %s\n", limitsCount(p.ResetCredits.AvailableCount))
		}
	}
	return b.String()
}

func FormatResetCredits(credits protocol.ResetCredits) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s reset credits (fetched %s)\navailable: %s\n", protocol.LimitsLabel(credits.Provider), limitsTime(credits.FetchedAt), limitsCount(credits.AvailableCount))
	limitsErrors(&b, credits.Error, credits.Warnings)
	for _, c := range credits.Credits {
		fmt.Fprintf(&b, "  %s: %s; scope %s", protocol.LimitsLabel(c.ID), protocol.LimitsLabel(c.Status), protocol.LimitsLabel(c.ResetType))
		if c.Redeemable != nil {
			fmt.Fprintf(&b, "; redeemable: %t", *c.Redeemable)
		}
		if c.ExpiresAt != nil {
			fmt.Fprintf(&b, "; expires %s", limitsTime(*c.ExpiresAt))
		} else {
			b.WriteString("; expiry unknown")
		}
		if c.GrantedAt != nil {
			fmt.Fprintf(&b, "; granted %s", limitsTime(*c.GrantedAt))
		}
		if c.Title != "" {
			fmt.Fprintf(&b, "; %s", protocol.LimitsLabel(c.Title))
		}
		if c.Description != "" {
			fmt.Fprintf(&b, "; %s", protocol.LimitsLabel(c.Description))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func FormatResetResult(result protocol.ResetResult, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s reset: %s\ncredit: %s\nrequest ID: %s\nfetched: %s\n", protocol.LimitsLabel(result.Provider), protocol.LimitsLabel(result.Outcome), protocol.LimitsLabel(result.CreditID), protocol.LimitsLabel(result.RequestID), limitsTime(result.FetchedAt))
	limitsErrors(&b, result.Error, result.Warnings)
	if result.WindowsReset != nil {
		fmt.Fprintf(&b, "windows reset: %d\n", *result.WindowsReset)
	}
	if result.Status != nil {
		b.WriteString(FormatLimits(protocol.LimitsReport{Providers: []protocol.ProviderLimits{*result.Status}}, now))
	}
	if result.Credits != nil {
		b.WriteString(FormatResetCredits(*result.Credits))
	}
	if ResetIsIndeterminate(result) {
		b.WriteString(ResetRetryCommands(protocol.ResetRequest{Provider: result.Provider, CreditID: result.CreditID, RequestID: result.RequestID}))
	}
	return b.String()
}

// ResetRetryCommands prints both complete public-surface commands. Validation
// ensures opaque identifiers cannot introduce shell syntax or control bytes.
func ResetRetryCommands(request protocol.ResetRequest) string {
	if request.Validate() != nil {
		return ""
	}
	return fmt.Sprintf("The credit may have been consumed. Retry only with the same provider, credit, and request ID:\n  harness limits reset %s %s -request-id %s\n  /limits reset %s %s %s\n", request.Provider, request.CreditID, request.RequestID, request.Provider, request.CreditID, request.RequestID)
}

func ResetIsIndeterminate(result protocol.ResetResult) bool {
	if result.Error != nil && result.Error.Indeterminate {
		return true
	}
	switch result.Outcome {
	case "reset", "already_redeemed", "nothing_to_reset", "no_credit":
		return false
	case "":
		return result.Error == nil
	default:
		return true
	}
}

// NormalizeResetResult covers callback/transport failures as well as future
// outcomes. It never changes the caller's retry identity or hides an outcome.
func NormalizeResetResult(request protocol.ResetRequest, result protocol.ResetResult, err error) protocol.ResetResult {
	result.Provider, result.CreditID, result.RequestID = request.Provider, request.CreditID, request.RequestID
	if result.Error == nil && err != nil {
		var detail *protocol.LimitsError
		if errors.As(err, &detail) {
			copy := *detail
			result.Error = &copy
		} else {
			result.Error = &protocol.LimitsError{Code: "transport", Message: "reset request failed; the credit may have been consumed", Indeterminate: true}
		}
	}
	if ResetIsIndeterminate(result) {
		if result.Error == nil {
			result.Error = &protocol.LimitsError{Code: "indeterminate", Message: "reset outcome is unknown; the credit may have been consumed"}
		}
		copy := *result.Error
		copy.Indeterminate = true
		result.Error = &copy
		if result.Outcome == "" {
			result.Outcome = "indeterminate"
		}
	}
	return result
}

func ResetExitCode(result protocol.ResetResult) int {
	if result.Error != nil || ResetIsIndeterminate(result) {
		return ExitRuntime
	}
	switch result.Outcome {
	case "reset", "already_redeemed", "nothing_to_reset":
		return ExitOK
	}
	return ExitRuntime
}

func limitsErrors(b *strings.Builder, err *protocol.LimitsError, warnings []protocol.LimitsError) {
	if err != nil {
		fmt.Fprintf(b, "  error [%s]: %s\n", protocol.LimitsLabel(err.Code), protocol.LimitsLabel(err.Message))
	}
	for _, w := range warnings {
		fmt.Fprintf(b, "  warning [%s]: %s\n", protocol.LimitsLabel(w.Code), protocol.LimitsLabel(w.Message))
	}
}
func limitsName(name, fallback string) string {
	if name == "" {
		name = fallback
	}
	return protocol.LimitsLabel(name)
}
func limitsCount(n *int64) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprint(*n)
}
func limitsTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339Nano)
}
func limitsCountdown(reset, now time.Time) string {
	if !reset.After(now) {
		return "due"
	}
	return "in " + reset.Sub(now).Round(time.Second).String()
}

func (app *App) limitsCommand(arg string) {
	args := strings.Fields(arg)
	usage := func() {
		fmt.Fprintln(app.Errw, "usage: /limits [provider] | /limits resets openai-codex | /limits reset openai-codex <credit-id> [request-id]")
	}
	action, provider := "status", ""
	if len(args) > 0 {
		switch args[0] {
		case "resets":
			if len(args) != 2 || args[1] != "openai-codex" {
				usage()
				return
			}
			action, provider = "resets", args[1]
		case "reset":
			if len(args) < 3 || len(args) > 4 {
				usage()
				return
			}
			action, provider = "reset", args[1]
		default:
			if len(args) != 1 {
				usage()
				return
			}
			provider = args[0]
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), LimitsCommandTimeout)
	defer cancel()
	if app.Interrupt != nil {
		app.Interrupt.BeginPrompt(cancel)
		defer app.Interrupt.EndPrompt()
	}
	var err error
	switch action {
	case "status":
		if app.Limits == nil {
			fmt.Fprintln(app.Errw, "[subscription limits unavailable]")
			return
		}
		var report protocol.LimitsReport
		report, err = app.Limits(ctx, provider)
		if err == nil || report.Providers != nil {
			fmt.Fprint(app.Errw, FormatLimits(report, app.clock()()))
		}
	case "resets":
		if app.ResetCredits == nil {
			fmt.Fprintln(app.Errw, "[reset credits unavailable]")
			return
		}
		var credits protocol.ResetCredits
		credits, err = app.ResetCredits(ctx, provider)
		if err == nil || credits.Provider != "" {
			fmt.Fprint(app.Errw, FormatResetCredits(credits))
		}
	case "reset":
		if app.ResetLimits == nil {
			fmt.Fprintln(app.Errw, "[reset credits unavailable]")
			return
		}
		request := protocol.ResetRequest{Provider: provider, CreditID: args[2]}
		if len(args) == 4 {
			request.RequestID = args[3]
		}
		if pending := app.pendingLimitReset; pending != nil {
			if request.Provider != pending.Provider || request.CreditID != pending.CreditID || (request.RequestID != "" && request.RequestID != pending.RequestID) {
				fmt.Fprintln(app.Errw, "[resolve the pending reset before selecting another credit or request ID]")
				fmt.Fprint(app.Errw, ResetRetryCommands(*pending))
				return
			}
			request = *pending
		} else if request.RequestID == "" {
			request.RequestID, err = protocol.NewResetRequestID()
			if err != nil {
				break
			}
		}
		if err = request.Validate(); err != nil {
			usage()
			break
		}
		var result protocol.ResetResult
		result, err = app.ResetLimits(ctx, request)
		result = NormalizeResetResult(request, result, err)
		if ResetIsIndeterminate(result) {
			app.pendingLimitReset = &request
		} else {
			app.pendingLimitReset = nil
		}
		fmt.Fprint(app.Errw, FormatResetResult(result, app.clock()()))
		return
	}
	if err != nil {
		fmt.Fprintf(app.Errw, "[limits: %s]\n", protocol.LimitsLabel(err.Error()))
	}
}
