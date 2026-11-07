package tmux

import "testing"

// Codex 0.167 (captured on hera, 2026-11-07) draws the live composer with "»"
// and keeps "›" for the echoed messages above it.

func TestCodex0167PromptRegexMatchesTheComposer(t *testing.T) {
	resolved, err := CompilePatterns(DefaultRawPatterns("codex"))
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
	for _, content := range []string{"» Ask Codex to do anything", "» ", "  » reload only 7a2fef0"} {
		matched := false
		for _, re := range resolved.PromptRegexps {
			if re.MatchString(content) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("prompt regexps did not match %q", content)
		}
	}
}

func TestCodex0167BannerScanTreatsTheComposerGlyph(t *testing.T) {
	const banner = "■ You've hit your usage limit. To continue using Codex, try again at Oct 10th, 2026 8:03 AM."
	const footer = "  GPT-6.1-Sol ultra · ~/work/lane-a · Main [default]"
	current := banner + "\n\n» Ask Codex to do anything\n" + footer + "\n"
	if kind, _ := scanCodexErrorBanner(current); kind == "" {
		t.Fatalf("a banner directly above the » composer must still be an error")
	}
	// An echoed message between the banner and the composer proves a later
	// turn was submitted; before 0.167 support that message read as the
	// composer and the stale banner was reported.
	history := banner + "\n\n› retry now\n\n» Ask Codex to do anything\n" + footer + "\n"
	if kind, detail := scanCodexErrorBanner(history); kind != "" {
		t.Fatalf("a banner above an echoed message is history, got %q (%q)", kind, detail)
	}
}
