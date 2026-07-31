package highlight

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestThemePalettesExactAndContrastSafe(t *testing.T) {
	themes := []struct {
		name       string
		theme      Theme
		foreground []string
		addedBG    string
		removedBG  string
	}{
		{
			name:  "dark",
			theme: ThemeDark,
			foreground: []string{
				"\x1b[38;2;129;184;139m", "\x1b[38;2;101;169;224m",
				"\x1b[38;2;206;145;120m", "\x1b[38;2;181;206;168m",
				"\x1b[38;2;78;201;176m", "\x1b[38;2;220;220;170m",
				"\x1b[38;2;137;209;133m", "\x1b[38;2;244;135;113m",
			},
			addedBG:   "\x1b[48;2;33;58;43m",
			removedBG: "\x1b[48;2;74;34;29m",
		},
		{
			name:  "light",
			theme: ThemeLight,
			foreground: []string{
				"\x1b[38;2;0;119;0m", "\x1b[38;2;0;0;255m",
				"\x1b[38;2;163;21;21m", "\x1b[38;2;8;122;80m",
				"\x1b[38;2;35;117;141m", "\x1b[38;2;121;94;38m",
				"\x1b[38;2;0;119;0m", "\x1b[38;2;163;21;21m",
			},
			addedBG:   "\x1b[48;2;218;251;225m",
			removedBG: "\x1b[48;2;255;235;233m",
		},
	}

	for _, tt := range themes {
		t.Run(tt.name, func(t *testing.T) {
			p := paletteFor(tt.theme)
			gotFG := []string{p.comment, p.keyword, p.string, p.number, p.builtin, p.function, p.added, p.removed}
			for i := range tt.foreground {
				if gotFG[i] != tt.foreground[i] {
					t.Errorf("foreground %d = %q, want %q", i, gotFG[i], tt.foreground[i])
				}
			}
			if p.addedBackground != tt.addedBG || p.removedBackground != tt.removedBG {
				t.Errorf("backgrounds = %q, %q; want %q, %q", p.addedBackground, p.removedBackground, tt.addedBG, tt.removedBG)
			}

			backgrounds := [][3]int{parseThemeRGB(t, p.addedBackground, 48), parseThemeRGB(t, p.removedBackground, 48)}
			for i, sgr := range gotFG {
				fg := parseThemeRGB(t, sgr, 38)
				for j, bg := range backgrounds {
					if ratio := contrastRatio(fg, bg); ratio < 4.5 {
						t.Errorf("foreground %d against background %d contrast = %.2f, want >= 4.5", i, j, ratio)
					}
				}
			}
		})
	}
}

func TestThemedConstructorsAndSemanticRoles(t *testing.T) {
	line := `func main() string { return "x" + 42 // note }`
	for _, tt := range []struct {
		name  string
		theme Theme
	}{
		{"dark", ThemeDark},
		{"light", ThemeLight},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := paletteFor(tt.theme)
			got := NewWithTheme("go", tt.theme).Line(line)
			for role, want := range map[string]string{
				"comment": p.comment + "// note", "keyword": p.keyword + "func",
				"string": p.string + `"x"`, "number": p.number + "42",
				"builtin": p.builtin + "string", "function": p.function + "main",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %s style %q in %q", role, want, got)
				}
			}

			d := NewDiffWithTheme("go", tt.theme)
			if got := d.Line("+value"); !strings.HasPrefix(got, p.addedBackground+p.added+"+") {
				t.Errorf("added diff does not use selected palette: %q", got)
			}
			if got := d.Line("-value"); !strings.HasPrefix(got, p.removedBackground+p.removed+"-") {
				t.Errorf("removed diff does not use selected palette: %q", got)
			}
		})
	}

	if got, want := New("go").Line(line), NewWithTheme("go", ThemeDark).Line(line); got != want {
		t.Errorf("New did not default to dark:\n got %q\nwant %q", got, want)
	}
	if got, want := NewWithTheme("go", Theme(255)).Line(line), NewWithTheme("go", ThemeDark).Line(line); got != want {
		t.Errorf("unknown theme did not fall back to dark:\n got %q\nwant %q", got, want)
	}
	if NewWithTheme("unknown", ThemeLight) != nil || NewWithTheme("", ThemeLight) != nil {
		t.Error("unknown and absent fenced languages must remain nil")
	}
}

func TestEveryLanguageSourceRecoveryUnderBothThemes(t *testing.T) {
	for _, theme := range []Theme{ThemeDark, ThemeLight} {
		for name, lines := range corpus {
			s := NewWithTheme(name, theme)
			for i, line := range lines {
				if got := stripANSI(s.Line(line)); got != line {
					t.Errorf("theme %d, %s line %d altered source: got %q, want %q", theme, name, i+1, got, line)
				}
			}
		}
	}
}

func TestThemeSurvivesMultilineAndDiffFileResets(t *testing.T) {
	for _, theme := range []Theme{ThemeDark, ThemeLight} {
		p := paletteFor(theme)
		s := NewWithTheme("go", theme)
		s.Line("/* open")
		if got := s.Line("close */"); !strings.HasPrefix(got, p.comment) || stripANSI(got) != "close */" {
			t.Errorf("theme %d multiline comment = %q", theme, got)
		}
		s = NewWithTheme("go", theme)
		s.Line("s := `open")
		if got := s.Line("close`"); !strings.HasPrefix(got, p.string) || stripANSI(got) != "close`" {
			t.Errorf("theme %d multiline raw string = %q", theme, got)
		}

		d := NewDiffWithTheme("go", theme)
		for _, line := range []string{
			"--- a/one.go", "+++ b/one.go", "@@ -1 +1 @@", "-/* old", "+/* new",
			"--- a/two.go", "+++ b/two.go", "@@ -1 +1 @@",
		} {
			d.Line(line)
		}
		for _, line := range []string{"-func old() {}", "+func new() {}"} {
			got := d.Line(line)
			if strings.Contains(got, p.comment+"func") || !strings.Contains(got, p.keyword+"func") {
				t.Errorf("theme %d was lost or lexical state leaked after file reset: %q", theme, got)
			}
		}
	}
}

func TestDiffKeepsThemedOldAndNewLexicalStatesIndependent(t *testing.T) {
	for _, theme := range []Theme{ThemeDark, ThemeLight} {
		p := paletteFor(theme)
		d := NewDiffWithTheme("go", theme)
		for _, line := range []string{"--- a/state.go", "+++ b/state.go", "@@ -1,3 +1,2 @@"} {
			d.Line(line)
		}
		if got := d.Line("-/* old"); !strings.Contains(got, p.comment+"/* old") {
			t.Fatalf("theme %d removed line did not open old-side state: %q", theme, got)
		}
		got := d.Line("+replacement()")
		body := strings.TrimPrefix(got, p.addedBackground+p.added+"+"+styleReset+p.addedBackground)
		if strings.Contains(body, p.comment) || !strings.Contains(body, p.function+"replacement") {
			t.Errorf("theme %d old-side state contaminated added line: %q", theme, got)
		}
		if got := d.Line(" shared()"); strings.Contains(got, p.comment) || !strings.Contains(got, p.function+"shared") {
			t.Errorf("theme %d old-side state contaminated new-side context: %q", theme, got)
		}
		if got := d.Line("-*/"); !strings.Contains(got, p.comment+"*/") {
			t.Errorf("theme %d old-side state was not retained: %q", theme, got)
		}
	}
}

func TestRecognizedDiffSyntaxPreservesSelectedBCE(t *testing.T) {
	for _, theme := range []Theme{ThemeDark, ThemeLight} {
		p := paletteFor(theme)
		for _, tc := range []struct {
			line, background string
		}{
			{`+func f() string { return "x" } // done`, p.addedBackground},
			{`-func f() string { return "x" } // done`, p.removedBackground},
		} {
			got := NewDiffWithTheme("go", theme).Line(tc.line)
			if !strings.HasPrefix(got, tc.background) || !strings.HasSuffix(got, eraseToEOL+styleReset) {
				t.Errorf("theme %d recognized row lost background/BCE: %q", theme, got)
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(got, tc.background), eraseToEOL+styleReset)
			inner = strings.ReplaceAll(inner, styleReset+tc.background, "")
			if strings.Contains(inner, styleReset) {
				t.Errorf("theme %d reset punched through recognized row background: %q", theme, got)
			}
			if strings.Count(got, styleReset+tc.background) < 4 {
				t.Errorf("theme %d row did not exercise multiple reset reapplications: %q", theme, got)
			}
			if stripped := stripANSI(got); stripped != tc.line {
				t.Errorf("theme %d diff altered source: got %q, want %q", theme, stripped, tc.line)
			}
		}
	}
}

func TestUnknownDiffLanguageUsesSelectedRowsAndPreservesBCE(t *testing.T) {
	for _, theme := range []Theme{ThemeDark, ThemeLight} {
		p := paletteFor(theme)
		for _, tc := range []struct {
			line, foreground, background string
		}{
			{"+plain", p.added, p.addedBackground},
			{"-plain", p.removed, p.removedBackground},
		} {
			got := NewDiffWithTheme("unknown", theme).Line(tc.line)
			if !strings.HasPrefix(got, tc.background+tc.foreground+tc.line[:1]) {
				t.Errorf("theme %d unknown diff row = %q", theme, got)
			}
			if !strings.HasSuffix(got, eraseToEOL+styleReset) {
				t.Errorf("theme %d unknown diff row lost BCE/final reset: %q", theme, got)
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(got, tc.background), eraseToEOL+styleReset)
			inner = strings.ReplaceAll(inner, styleReset+tc.background, "")
			if strings.Contains(inner, styleReset) {
				t.Errorf("theme %d reset punched through row background: %q", theme, got)
			}
			if stripped := stripANSI(got); stripped != tc.line {
				t.Errorf("theme %d diff altered source: got %q, want %q", theme, stripped, tc.line)
			}
		}
	}
}

func parseThemeRGB(t *testing.T, sgr string, plane int) [3]int {
	t.Helper()
	var gotPlane, r, g, b int
	if _, err := fmt.Sscanf(sgr, "\x1b[%d;2;%d;%d;%dm", &gotPlane, &r, &g, &b); err != nil || gotPlane != plane {
		t.Fatalf("parse SGR %q: plane=%d err=%v", sgr, gotPlane, err)
	}
	return [3]int{r, g, b}
}

func contrastRatio(a, b [3]int) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func relativeLuminance(rgb [3]int) float64 {
	linear := func(v int) float64 {
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(rgb[0]) + 0.7152*linear(rgb[1]) + 0.0722*linear(rgb[2])
}
