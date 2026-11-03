package tmux

import (
	"testing"
	"time"
)

// SetCachedSubstateForTest seeds the cached substate so tests can exercise
// layers that read it without classifying a real pane. It lives in a _test.go
// file so it never ships in the binary (issue #2986 item 6).
func (s *Session) SetCachedSubstateForTest(sub Substate, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSubstate, s.lastSubstateDetail = sub, detail
}

// ResetDefaultServerSessionsForTest drops the foreign-server guard's cached
// default-server listing (foreign_server.go), now and at cleanup, so a test
// that stages a default server is not answered from a listing up to
// defaultServerSessionsTTL old.
func ResetDefaultServerSessionsForTest(t testing.TB) {
	t.Helper()
	reset := func() {
		defaultServerSessions.Lock()
		defaultServerSessions.names, defaultServerSessions.err, defaultServerSessions.at = nil, nil, time.Time{}
		defaultServerSessions.Unlock()
	}
	reset()
	t.Cleanup(reset)
}
