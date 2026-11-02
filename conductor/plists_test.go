package conductor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The launchd plists that setup.sh renders run their jobs as ProcessType
// Interactive and set no LowPriorityIO, as the plists that agent-deck conductor
// setup generates do (TestConductorPlists_InteractiveWithoutLowPriorityIO in
// internal/session). Agent sessions inherit the launchd limits of whichever job
// starts the default tmux server, so a job that sets LowPriorityIO, or no
// ProcessType, makes every session it starts run clamped.
func TestStaticPlists_InteractiveWithoutLowPriorityIO(t *testing.T) {
	interactive := regexp.MustCompile(`<key>ProcessType</key>\s*<string>Interactive</string>`)
	for _, path := range []string{
		"com.agentdeck.conductor-bridge.plist",
		"gh-watcher/com.agentdeck.gh-watcher.plist",
	} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			plist := string(data)
			if n := len(interactive.FindAllString(plist, -1)); n != 1 {
				t.Errorf("%s has %d ProcessType Interactive entries, want 1", path, n)
			}
			if n := strings.Count(plist, "<key>ProcessType</key>"); n != 1 {
				t.Errorf("%s has %d ProcessType keys, want 1", path, n)
			}
			if strings.Contains(plist, "<key>LowPriorityIO</key>") {
				t.Errorf("%s sets LowPriorityIO, want it absent", path)
			}
		})
	}
}
