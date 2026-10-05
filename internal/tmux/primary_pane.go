package tmux

import "strings"

// primaryPaneLine returns the first line of a list-panes result. list-panes
// expands a window target to every pane in that window, in pane-index order,
// so the first line describes the lowest-index pane: the primary pane, as the
// list-panes -a cache also defines it (parseListPanesOutput). That is the
// pane the session was created with unless the user split before it (-b) or
// swapped panes.
func primaryPaneLine(raw string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(raw), "\n")
	return strings.TrimSpace(line)
}
