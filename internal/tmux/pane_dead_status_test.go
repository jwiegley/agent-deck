package tmux

import "testing"

// TestParsePaneDeadStatus covers the "#{pane_dead}|#{pane_dead_status}" parsing
// that lets status detection tell a clean one-shot exit (code 0) from a crash.
func TestParsePaneDeadStatus(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantCode int
		wantOK   bool
	}{
		{"clean exit 0", "1|0\n", 0, true},
		{"crash exit 1", "1|1", 1, true},
		{"crash exit 137", "1|137\n", 137, true},
		{"live pane", "0|", 0, false},
		{"live pane with stale status", "0|0", 0, false},
		{"dead pane, no remain-on-exit (empty status)", "1|", 0, false},
		{"dead pane, non-numeric status", "1|foo", 0, false},
		{"malformed, no separator", "1", 0, false},
		{"empty", "", 0, false},
		// A split window lists every pane; only the first (primary) counts.
		{"primary dead beside live split", "1|5\n0|\n", 5, true},
		{"primary live beside dead split", "0|\n1|3\n", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, ok := parsePaneDeadStatus(tt.raw)
			if code != tt.wantCode || ok != tt.wantOK {
				t.Errorf("parsePaneDeadStatus(%q) = (%d, %v), want (%d, %v)",
					tt.raw, code, ok, tt.wantCode, tt.wantOK)
			}
		})
	}
}
