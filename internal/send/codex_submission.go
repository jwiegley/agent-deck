package send

import (
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Codex's own acknowledgement that it took a message (issue #2424).
//
// Codex 0.15x draws its composer and every user message in the transcript the
// same way: "›" at column 0, wrapped and multi-line continuations indented two
// columns. The composer is the last such cell; the cells above it are the
// transcript. Codex 0.167 (captured on hera, 2026-11-07) draws the live
// composer with "»" at column 0 instead and keeps "›" for the echoed messages,
// so a "»" cell is the composer wherever it sits, and the bottom-most "›" cell
// is the composer only when no "»" cell is drawn. When Codex takes a message
// it moves it out of the composer into a transcript cell and, while the turn
// runs, shows its live status row
// ("• Working (6s • esc to interrupt)") above the composer. A lane that was
// already working shows that row too, which is why the transcript cell and
// the live row are each read together with "the composer no longer holds
// this send's body" and never on their own.
//
// A message Codex parks in its "• Queued follow-up inputs" block ("  ↳ ")
// has not entered a turn yet and is not read as submitted here.

const (
	codexPromptGlyph   = "›"
	codexComposerGlyph = "»"
)

// codexCell is one column-0 "›" cell of a Codex frame.
type codexCell struct {
	// text is the cell's body, glyph removed and whitespace-collapsed.
	text string
	// followedByTranscript reports a column-0 transcript row (a "• " agent
	// or status row, a "─ Worked for … ─" rule) between this cell and the
	// next one. Codex draws one between its newest user message and the
	// composer; a picker row drawn right under the composer has none.
	followedByTranscript bool
	// composer reports a "»" cell: the live composer of Codex 0.167 and later.
	composer bool
}

// codexCellGlyph reports the column-0 glyph that opens a cell on row, if any.
func codexCellGlyph(row string) (string, bool) {
	for _, glyph := range []string{codexComposerGlyph, codexPromptGlyph} {
		if strings.HasPrefix(row, glyph) {
			return glyph, true
		}
	}
	return "", false
}

// codexComposerIndex returns the index of the composer cell: the last "»"
// cell when Codex draws one (0.167 and later), else the last cell (0.15x,
// where the composer is the bottom-most "›" cell). -1 without cells.
func codexComposerIndex(cells []codexCell) int {
	for i := len(cells) - 1; i >= 0; i-- {
		if cells[i].composer {
			return i
		}
	}
	return len(cells) - 1
}

// codexCells splits an ANSI-stripped Codex frame into its column-0 "›" and
// "»" cells, top to bottom. A cell continues through rows indented two
// columns, and through blank rows when the next non-blank row is still
// indented (a blank line inside a message). The composer cell therefore also takes in the
// model/context footer below it, which never carries message text.
func codexCells(content string) []codexCell {
	rows := strings.Split(content, "\n")
	var cells []codexCell
	for i := 0; i < len(rows); i++ {
		glyph, ok := codexCellGlyph(rows[i])
		if !ok {
			if len(cells) > 0 && strings.TrimSpace(rows[i]) != "" && !startsIndented(rows[i]) {
				cells[len(cells)-1].followedByTranscript = true
			}
			continue
		}
		body := []string{strings.TrimPrefix(rows[i], glyph)}
		j := i + 1
		for j < len(rows) {
			if startsIndented(rows[j]) && strings.TrimSpace(rows[j]) != "" {
				body = append(body, rows[j])
				j++
				continue
			}
			if strings.TrimSpace(rows[j]) == "" {
				next := j + 1
				for next < len(rows) && strings.TrimSpace(rows[next]) == "" {
					next++
				}
				if next < len(rows) && startsIndented(rows[next]) {
					j = next
					continue
				}
			}
			break
		}
		cells = append(cells, codexCell{text: collapse(strings.Join(body, " ")), composer: glyph == codexComposerGlyph})
		i = j - 1
	}
	return cells
}

func startsIndented(row string) bool {
	return strings.HasPrefix(row, "  ")
}

// codexTranscriptCopies counts transcript cells (every cell but the
// composer) that begin with token and are followed by transcript rows.
func codexTranscriptCopies(content, token string) int {
	if token == "" {
		return 0
	}
	cells := codexCells(content)
	composer := codexComposerIndex(cells)
	n := 0
	for i := range cells {
		if i == composer {
			continue
		}
		if cells[i].followedByTranscript && strings.HasPrefix(cells[i].text, token) {
			n++
		}
	}
	return n
}

// codexQueuedHolds reports whether Codex's queued follow-up inputs list
// ("  ↳ <message>" rows with four-column continuations) holds token.
func codexQueuedHolds(content, token string) bool {
	rows := strings.Split(content, "\n")
	for i, row := range rows {
		if !strings.HasPrefix(row, "  ↳ ") {
			continue
		}
		entry := []string{strings.TrimPrefix(row, "  ↳ ")}
		for j := i + 1; j < len(rows) && strings.HasPrefix(rows[j], "    ") && strings.TrimSpace(rows[j]) != ""; j++ {
			entry = append(entry, rows[j])
		}
		if strings.HasPrefix(collapse(strings.Join(entry, " ")), token) {
			return true
		}
	}
	return false
}

// codexTookMessage reports whether an ANSI-stripped Codex frame shows Codex
// took this send's message. A new copy of the body must be on screen (copies
// above baselineCopies, the whole-frame token counts after and before the
// send) and the composer (the "»" cell, or the last "›" cell on Codex 0.15x)
// must not hold it. Then either a new
// transcript cell carries it (more than baselineCells), or it is not parked
// in the queued list and Codex's live status row shows a turn running.
func codexTookMessage(content, token string, copies, baselineCopies, baselineCells int) bool {
	if token == "" || copies <= baselineCopies {
		return false
	}
	cells := codexCells(content)
	if len(cells) == 0 {
		return false
	}
	if composer := cells[codexComposerIndex(cells)].text; strings.Contains(composer, token) || CountPasteMarkers(composer) > 0 {
		return false
	}
	if codexTranscriptCopies(content, token) > baselineCells {
		return true
	}
	return !codexQueuedHolds(content, token) && tmux.CodexTurnRunning(content)
}
