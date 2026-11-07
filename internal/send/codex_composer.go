package send

import "strings"

// Codex composer introspection for the composer-draft guard (issues #1409,
// #2536).
//
// Codex 0.15x draws its composer as the last column-0 "›" cell of the frame
// (see codexCells); Codex 0.167 and later draw the live composer with "»" and
// keep "›" for the echoed messages above it, so the composer is the last "»"
// row when one is drawn and the last "›" row otherwise. Wrapped and
// multi-line input continues on rows indented two columns, and the
// model/context footer under the composer is indented the same way, so the
// cell's final row is the footer, never input.
//
// An empty composer shows a placeholder in the input row, rendered with the
// SGR dim attribute ("Ask Codex to do anything", or one of the rotating
// suggestions on a fresh session). As with Claude's autosuggestion, the dim
// attribute is the discriminator, so this reader needs the raw capture with
// ANSI intact (tmux capture-pane -e). Because Codex only draws the
// placeholder while the input is empty, a dim input row means "no draft".

// codexDefaultPlaceholder is the placeholder Codex shows in an empty composer
// once a session is under way. It is also recognized without the dim
// attribute so an ANSI-less capture of an idle pane never reads as a draft.
const codexDefaultPlaceholder = "Ask Codex to do anything"

// CodexComposerDraft returns the operator draft in a Codex composer and
// whether a Codex composer is visible at all. Same raw/strip contract as
// ComposerDraft: raw keeps ANSI attributes, strip (pass tmux.StripANSI)
// removes them for text extraction.
func CodexComposerDraft(raw string, strip func(string) string) (draft string, composerVisible bool) {
	strip = orIdentity(strip)
	rawRows := strings.Split(raw, "\n")
	rows := strings.Split(strip(raw), "\n")

	start, glyph := codexComposerRow(rows)
	if start < 0 {
		return "", false
	}

	first := NormalizePromptText(strings.TrimPrefix(rows[start], glyph))
	if first != "" {
		if first == codexDefaultPlaceholder {
			return "", true
		}
		if start < len(rawRows) && len(rawRows) == len(rows) {
			if body, ok := composerMarkerBody(rawRows[start]); ok && bodyIsPlaceholder(body) {
				return "", true
			}
		}
	}

	// Continuation rows: indented rows, and blank rows inside a multi-line
	// draft, the same extent codexCells gives the composer cell.
	var cont []string
	for j := start + 1; j < len(rows); j++ {
		if startsIndented(rows[j]) && strings.TrimSpace(rows[j]) != "" {
			cont = append(cont, rows[j])
			continue
		}
		if strings.TrimSpace(rows[j]) == "" {
			next := j + 1
			for next < len(rows) && strings.TrimSpace(rows[next]) == "" {
				next++
			}
			if next < len(rows) && startsIndented(rows[next]) {
				j = next - 1
				continue
			}
		}
		break
	}
	// The last indented row of the cell is Codex's footer.
	if len(cont) > 0 {
		cont = cont[:len(cont)-1]
	}

	parts := append([]string{first}, cont...)
	return NormalizePromptText(strings.Join(parts, " ")), true
}

// codexComposerRow finds the composer's input row in ANSI-stripped rows and
// the glyph that opens it: the last column-0 "»" row (Codex 0.167 and later),
// else the last "›" row (Codex 0.15x, where the echoed messages and the
// composer share the glyph and the composer is the bottom-most). On 0.167 the
// last "›" row is the newest echoed message, which must never read as a draft.
func codexComposerRow(rows []string) (int, string) {
	for _, glyph := range []string{codexComposerGlyph, codexPromptGlyph} {
		for i := len(rows) - 1; i >= 0; i-- {
			if strings.HasPrefix(rows[i], glyph) {
				return i, glyph
			}
		}
	}
	return -1, ""
}
