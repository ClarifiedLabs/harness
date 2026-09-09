package llm

import (
	"strings"
	"testing"
)

// Policy arithmetic lives here; dialect tests cover wire fields and policy inputs.
func TestResolveMaxTokensPolicy(t *testing.T) {
	tests := []struct {
		name          string
		req           Request
		contextWindow int
		outputLimit   int
		want          int
	}{
		{name: "unknown limits omit"},
		{name: "known output unknown context omits", req: Request{EstimatedInputTokens: 1000}, outputLimit: 64_000},
		{name: "unknown output limit", req: Request{EstimatedInputTokens: 1000}, contextWindow: 128_000, want: 32_000},
		{name: "small window default", contextWindow: 20_000, want: 5_000},
		{name: "large window default", contextWindow: 1_000_000, want: 250_000},
		{name: "default cap", contextWindow: 8_000_000, want: 1_000_000},
		{name: "catalog 128k ceiling", contextWindow: 1_000_000, outputLimit: 128_000, want: 128_000},
		{name: "catalog 100k ceiling", req: Request{EstimatedInputTokens: 1000}, contextWindow: 1_000_000, outputLimit: 100_000, want: 100_000},
		{name: "catalog 64k ceiling", contextWindow: 1_000_000, outputLimit: 64_000, want: 64_000},
		{name: "small catalog ceiling", contextWindow: 1_000_000, outputLimit: 8_000, want: 8_000},
		{name: "full window output limit keeps quarter default", req: Request{EstimatedInputTokens: 4_436}, contextWindow: 262_144, outputLimit: 262_144, want: 65_536},
		{name: "explicit value", req: Request{MaxTokens: 333}, contextWindow: 1_000_000, want: 333},
		{name: "explicit value unknown context", req: Request{MaxTokens: 333}, want: 333},
		{name: "explicit below catalog ceiling", req: Request{MaxTokens: 333}, contextWindow: 1_000_000, outputLimit: 64_000, want: 333},
		{name: "catalog caps explicit value", req: Request{MaxTokens: 100_000}, contextWindow: 1_000_000, outputLimit: 64_000, want: 64_000},
		{name: "explicit clamped to remaining window", req: Request{MaxTokens: 100_000, EstimatedInputTokens: 90_000}, contextWindow: 100_000, want: 7_000}, // 100000 - 90000 - 3000 reserve
		{name: "tiny remaining window", req: Request{EstimatedInputTokens: 99_999}, contextWindow: 100_000, outputLimit: 64_000, want: 1},
		{name: "estimate input when unset", req: Request{System: strings.Repeat("x", 360_000)}, contextWindow: 100_000, want: 7_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveMaxTokens(tc.req, tc.contextWindow, tc.outputLimit); got != tc.want {
				t.Fatalf("ResolveMaxTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestResolveMaxTokensLeavesProviderAccountingHeadroom(t *testing.T) {
	req := Request{EstimatedInputTokens: 48_325}
	got := ResolveMaxTokens(req, 262_144, 262_144)
	const actualProviderInput = 52_762
	if actualProviderInput+got > 262_144 {
		t.Fatalf("ResolveMaxTokens = %d leaves actual request at %d, want <= 262144", got, actualProviderInput+got)
	}
}

func TestEstimateInputTokensCountsToolResultImagesWithoutBase64Text(t *testing.T) {
	image := ContentBlock{
		Kind:           BlockImage,
		ImageMediaType: "image/png",
		ImageData:      "YWJj",
		ImageDetail:    "high",
		ImageName:      "screen.png",
	}
	base := Request{Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Kind: BlockToolResult, ResultForID: "call_1", ResultText: "attached"}}}}}
	rich := base
	rich.Messages = []Message{{Role: RoleUser, Content: []ContentBlock{{Kind: BlockToolResult, ResultForID: "call_1", ResultText: "attached", ResultContent: []ContentBlock{image}}}}}

	baseTokens := EstimateInputTokens(base)
	richTokens := EstimateInputTokens(rich)
	metadataBytes := len(image.Kind) + len(image.ImageMediaType) + len(image.ImageDetail) + len(image.ImageName)
	wantDelta := metadataBytes/estimateBytesPerToken + EstimatedImageTokens
	// Integer division happens after all request bytes, so one token of rounding
	// drift is possible relative to dividing the image metadata separately.
	if delta := richTokens - baseTokens; delta < wantDelta-1 || delta > wantDelta+1 {
		t.Fatalf("rich image token delta = %d, want about %d", delta, wantDelta)
	}

	rich.Messages[0].Content[0].ResultContent[0].ImageData = strings.Repeat("A", 1<<20)
	if got := EstimateInputTokens(rich); got != richTokens {
		t.Fatalf("base64 counted as text: got %d after data growth, want %d", got, richTokens)
	}
}

func TestEffectiveContextWindow(t *testing.T) {
	if got := EffectiveContextWindow(262_144, 128_000); got != 128_000 {
		t.Fatalf("smaller hint = %d, want 128000", got)
	}
	if got := EffectiveContextWindow(128_000, 262_144); got != 128_000 {
		t.Fatalf("larger hint = %d, want 128000", got)
	}
	if got := EffectiveContextWindow(0, 64_000); got != 64_000 {
		t.Fatalf("hint only = %d, want 64000", got)
	}
}
