package send

import (
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Codex 0.167 (captured on hera, 2026-11-07) draws the live composer with "»"
// and keeps "›" for the messages echoed above it. With the newest echoed
// message still on screen, the pre-0.167 reader took that message for the
// composer: every send to an idle lane was refused as "composer is occupied"
// by the draft guard, and a message Codex had taken was never seen as taken
// by the submission check, because its echoed copy read as the composer still
// holding it.

const codex0167Footer = "  GPT-6.1-Sol ultra · ~/work/lane-a · Main [default]"

// codex0167Frame is the bottom of an idle 0.167 pane: the newest echoed
// message, the reply, the status rows, then the composer rows given.
func codex0167Frame(composer ...string) string {
	rows := append(lines(
		"› I've seen several false alarms today, so that sounds like something you need to refine.",
		"",
		"• Agreed. I froze a source candidate for review.",
		"  Worked for 15m 35s • 12:35 PM",
		"",
		"  1 background terminal running · /ps to view · /stop to close",
	), composer...)
	return strings.Join(rows, "\n") + "\n"
}

func TestCodex0167ComposerDraft(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantDraft   string
		wantVisible bool
	}{
		{"idle placeholder under an echoed message", codex0167Frame("» Ask Codex to do anything", "", codex0167Footer), "", true},
		{"placeholder with the captured attributes", codex0167Frame("\x1b[1m\x1b[38;2;159;111;219m»\x1b[0m\x1b[48;2;30;30;30m \x1b[2mAsk Codex to do anything\x1b[0m", "", codex0167Footer), "", true},
		{"rotating placeholder dim", codex0167Frame("» \x1b[2mWrite tests for @filename\x1b[22m", "", codex0167Footer), "", true},
		{"typed draft", codex0167Frame("» reload only 7a2fef0 now", "", codex0167Footer), "reload only 7a2fef0 now", true},
		{"wrapped draft", codex0167Frame("» first visual row of a long", "  operator draft that wraps", "", codex0167Footer), "first visual row of a long operator draft that wraps", true},
		{"empty input row, footer only", codex0167Frame("» ", "", codex0167Footer), "", true},
		{"0.15x layout still read", issue2536Frame("› halfway through a refactor plan", "", codexFooter), "halfway through a refactor plan", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			draft, visible := CodexComposerDraft(tc.raw, tmux.StripANSI)
			if draft != tc.wantDraft || visible != tc.wantVisible {
				t.Fatalf("CodexComposerDraft = (%q, %v), want (%q, %v)", draft, visible, tc.wantDraft, tc.wantVisible)
			}
		})
	}
}

func TestCodex0167CellsTakeTheComposerFromItsGlyph(t *testing.T) {
	frame := codex0167Frame("» Ask Codex to do anything", "", codex0167Footer)
	cells := codexCells(frame)
	if len(cells) != 2 {
		t.Fatalf("codexCells = %d cells, want 2: %+v", len(cells), cells)
	}
	if got := codexComposerIndex(cells); got != 1 || !cells[1].composer {
		t.Fatalf("composer index = %d (composer flag %v), want the » cell at 1", got, cells[1].composer)
	}
	if cells[0].composer || !cells[0].followedByTranscript {
		t.Fatalf("echoed message cell = %+v, want a transcript cell followed by transcript rows", cells[0])
	}
	old := issue2536Frame("› halfway through a refactor plan", "", codexFooter)
	if cells := codexCells(old); codexComposerIndex(cells) != len(cells)-1 || cells[len(cells)-1].composer {
		t.Fatalf("0.15x layout: composer must stay the last › cell: %+v", cells)
	}
}

// codex0167IdleComposer is the empty 0.167 composer with its footer.
var codex0167IdleComposer = lines("", "» Ask Codex to do anything", "", codex0167Footer)

// renderCodex0167Composer renders msg held in the 0.167 composer: "» " at
// column 0, wrapped rows indented two columns, as renderCodexCell does for
// the echoed transcript copy.
func renderCodex0167Composer(msg string) []string {
	rows := renderCodexCell(msg)
	rows[0] = "» " + strings.TrimPrefix(rows[0], "› ")
	return rows
}

func TestCodex0167PaneEvidence(t *testing.T) {
	msg := issue2424Message
	cell := renderCodexCell(msg)
	idle := codexFrame(codexEarlierWork, lines("─ Worked for 2m 07s ─────────────────────────────"), codex0167IdleComposer)
	busy := codexFrame(codexEarlierWork, codexWorking("3m 30s"), codex0167IdleComposer)

	cases := []struct {
		name     string
		baseline string
		frame    string
		want     Outcome
	}{
		{
			// The echoed copy under a running turn, the » composer empty: taken.
			// Before 0.167 support the echoed › cell read as the composer still
			// holding the body, and the send was never confirmed.
			name:     "message in transcript, Working (6s)",
			baseline: idle,
			frame:    codexFrame(codexEarlierWork, cell, lines(""), codexWorking("6s"), codex0167IdleComposer),
			want:     OutcomeConfirmed,
		},
		{
			name:     "message in transcript, turn already over",
			baseline: idle,
			frame:    codexFrame(codexEarlierWork, cell, lines("", "• Done: fix/lane-a-retry is at 4f2c1e0, suite green.", "", "─ Worked for 3s ─────────────────────────────"), codex0167IdleComposer),
			want:     OutcomeConfirmed,
		},
		{
			// Enter swallowed: the body sits in the » composer of an idle lane.
			name:     "body still in the composer, idle",
			baseline: idle,
			frame:    codexFrame(codexEarlierWork, lines("─ Worked for 2m 07s ─────────────────────────────", ""), renderCodex0167Composer(msg), lines("", codex0167Footer)),
			want:     OutcomeFailed,
		},
		{
			name:     "body still in the composer while Working",
			baseline: busy,
			frame:    codexFrame(codexEarlierWork, codexWorking("3m 32s"), lines(""), renderCodex0167Composer(msg), lines("", codex0167Footer)),
			want:     OutcomeFailed,
		},
		{
			// A newer echoed message above the composer must not count as a
			// draft either: the lane is idle with an empty » composer.
			name:     "repeated message, earlier copy in transcript, new copy in composer",
			baseline: codexFrame(cell, lines("", "• Done.", ""), codex0167IdleComposer),
			frame:    codexFrame(cell, lines("", "• Done.", "", ""), renderCodex0167Composer(msg), lines("", codex0167Footer)),
			want:     OutcomeFailed,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := NewObserver("codex", msg, Captured(c.baseline))
			o.CodexLike = true
			v := replay(t, o, Captured(c.frame))
			if v.Outcome != c.want {
				t.Fatalf("outcome = %q (%s), want %q", v.Outcome, v.Message, c.want)
			}
		})
	}
}
