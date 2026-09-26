package session

import "sync"

// recallLinkMarks remembers, per runtime binding kind, the last id whose
// authoritative recall link this process has confirmed, so a hook repeated
// on every status tick does not re-check the link each time. It carries its
// own lock rather than relying on Instance.mu: the Claude candidate
// retraction clears a mark both inside UpdateHookStatus, which holds i.mu,
// and from UpdateClaudeSession, which does not.
type recallLinkMarks struct {
	mu  sync.Mutex
	ids map[string]string
}

func (m *recallLinkMarks) has(kind, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ids[kind] == id
}

func (m *recallLinkMarks) set(kind, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ids == nil {
		m.ids = make(map[string]string)
	}
	m.ids[kind] = id
}

// clear forgets kind's mark only while it still names id.
func (m *recallLinkMarks) clear(kind, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ids[kind] == id {
		delete(m.ids, kind)
	}
}
