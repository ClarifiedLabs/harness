package ui

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/modelproxy"
)

func TestLimitsFormattingPreservesUnknownAndAuthoritativeFlags(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 123, time.UTC)
	reset := now.Add(time.Hour)
	zero := 0.0
	hundred := 100.0
	allowed := true
	report := modelproxy.LimitsReport{Providers: []modelproxy.ProviderLimits{
		{Provider: "zai-coding-plan", Error: &modelproxy.LimitsError{Code: "timeout", Message: "unavailable"}},
		{Provider: "openai-codex", Plan: "pro", FetchedAt: now, ResetCredits: &modelproxy.ResetCreditSummary{}, Pools: []modelproxy.LimitPool{{ID: "primary", Allowed: &allowed, Windows: []modelproxy.LimitWindow{{Name: "weekly", UsedPercent: &hundred, Remaining: &zero, Unit: "credits", ResetAt: &reset}, {Name: "unknown"}}}}},
	}}
	text := FormatLimits(report, now)
	for _, want := range []string{"openai-codex — pro", "allowed: true", "used 100%", "remaining 0 credits", "usage unknown", "reset unknown", "reset credits available: unknown", reset.Format(time.RFC3339Nano), "in 1h0m0s", "error [timeout]: unavailable"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Index(text, "openai-codex") > strings.Index(text, "zai-coding-plan") || report.Providers[0].Provider != "zai-coding-plan" {
		t.Fatal("order or mutation")
	}
	if strings.Contains(FormatLimits(modelproxy.LimitsReport{Providers: []modelproxy.ProviderLimits{{Provider: "\x1b[31mred\x1b[0m"}}}, now), "\x1b") {
		t.Fatal("ANSI leaked")
	}
}

func TestResetFormattingAndOutcomes(t *testing.T) {
	request := modelproxy.ResetRequest{Provider: "openai-codex", CreditID: "credit", RequestID: "stable"}
	zero := int64(0)
	if text := FormatResetResult(modelproxy.ResetResult{Outcome: "nothing_to_reset", WindowsReset: &zero}, time.Now()); !strings.Contains(text, "windows reset: 0") {
		t.Fatal(text)
	}
	if text := FormatResetResult(modelproxy.ResetResult{Outcome: "no_credit"}, time.Now()); strings.Contains(text, "windows reset:") {
		t.Fatal("unknown windows reset became zero")
	}
	for _, tc := range []struct {
		outcome   string
		code      int
		ambiguous bool
	}{{"reset", 0, false}, {"already_redeemed", 0, false}, {"nothing_to_reset", 0, false}, {"no_credit", 1, false}, {"new_outcome", 1, true}, {"indeterminate", 1, true}} {
		result := NormalizeResetResult(request, modelproxy.ResetResult{Outcome: tc.outcome, Warnings: []modelproxy.LimitsError{{Code: "refresh", Message: "refresh unavailable"}}}, nil)
		if ResetExitCode(result) != tc.code || ResetIsIndeterminate(result) != tc.ambiguous {
			t.Fatalf("%+v", result)
		}
		text := FormatResetResult(result, time.Now())
		if !strings.Contains(text, "refresh unavailable") || (tc.ambiguous && (!strings.Contains(text, "harness limits reset openai-codex credit -request-id stable") || !strings.Contains(text, "/limits reset openai-codex credit stable"))) {
			t.Fatal(text)
		}
	}
	expired := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	no := false
	text := FormatResetCredits(modelproxy.ResetCredits{Provider: request.Provider, Credits: []modelproxy.ResetCredit{{ID: "credit", ResetType: "codex_rate_limits", Status: "redeeming", ExpiresAt: &expired, Redeemable: &no}}})
	for _, want := range []string{"credit: redeeming", "scope codex_rate_limits", "redeemable: false", "expires 2025", "available: unknown"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q: %s", want, text)
		}
	}
}

func TestLimitsREPLRetryAndNoSessionMutation(t *testing.T) {
	var output bytes.Buffer
	app := &App{Errw: &output, Model: "unchanged", System: "system", SessionPath: "do-not-write", PromptNumber: 7, pendingAPIContinuation: &apiContinuationState{}}
	continuation := app.pendingAPIContinuation
	usage := app.usage
	var requests []modelproxy.ResetRequest
	app.ResetLimits = func(ctx context.Context, request modelproxy.ResetRequest) (modelproxy.ResetResult, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded context")
		}
		requests = append(requests, request)
		if len(requests) == 1 {
			return modelproxy.ResetResult{}, errors.New("connection lost")
		}
		return modelproxy.ResetResult{Outcome: "already_redeemed"}, nil
	}
	result := app.command("/limits reset openai-codex credit", nil)
	if result.continueAfterAPIError || result.prompt != "" || len(requests) != 1 || app.pendingLimitReset == nil {
		t.Fatalf("result=%+v requests=%v", result, requests)
	}
	first := requests[0]
	if first.RequestID == "" || !strings.Contains(output.String(), "-request-id "+first.RequestID) {
		t.Fatal(output.String())
	}
	app.command("/limits reset openai-codex other", nil)
	app.command("/limits reset openai-codex credit different", nil)
	if len(requests) != 1 {
		t.Fatal("changed retry identity")
	}
	app.command("/limits reset openai-codex credit", nil)
	if len(requests) != 2 || requests[0] != requests[1] || app.pendingLimitReset != nil {
		t.Fatalf("requests=%+v pending=%+v", requests, app.pendingLimitReset)
	}
	if app.Model != "unchanged" || app.System != "system" || app.SessionPath != "do-not-write" || app.PromptNumber != 7 || app.pendingAPIContinuation != continuation || !reflect.DeepEqual(app.usage, usage) {
		t.Fatal("session mutation")
	}
}

func TestLimitsREPLDispatchAndSyntax(t *testing.T) {
	var output bytes.Buffer
	status, credits, resets := 0, 0, 0
	app := &App{Errw: &output, Limits: func(ctx context.Context, p string) (modelproxy.LimitsReport, error) {
		status++
		if p != "kimi-code-plan-cn" {
			t.Error(p)
		}
		return modelproxy.LimitsReport{Providers: []modelproxy.ProviderLimits{{Provider: p}}}, nil
	}, ResetCredits: func(ctx context.Context, p string) (modelproxy.ResetCredits, error) {
		credits++
		return modelproxy.ResetCredits{Provider: p}, nil
	}, ResetLimits: func(ctx context.Context, r modelproxy.ResetRequest) (modelproxy.ResetResult, error) {
		resets++
		return modelproxy.ResetResult{Outcome: "no_credit"}, nil
	}}
	for _, line := range []string{"/limits kimi-code-plan-cn", "/limits resets openai-codex", "/limits reset openai-codex credit stable", "/limits reset", "/limits resets kimi-code-plan-cn", "/limits reset openai-codex bad/id id", "/limits one two"} {
		app.command(line, nil)
	}
	if status != 1 || credits != 1 || resets != 1 {
		t.Fatalf("calls=%d/%d/%d", status, credits, resets)
	}
	if !strings.Contains(output.String(), "usage: /limits") || !strings.Contains(helpText, "/limits") {
		t.Fatal(output.String())
	}
}
