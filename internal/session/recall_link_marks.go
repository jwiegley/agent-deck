package session

import "sync"

// recallLinkVersion names the binding a recall link was confirmed for: the
// id together with the durable version (incarnation, runtime generation and
// binding revision) that held it. The id alone is not enough. A binding can
// leave an id and come back to it (A, then a peer's B, then A again), and
// the peer's link for B demotes A's in between, so having linked an earlier
// A says nothing about the link of the A bound now.
type recallLinkVersion struct {
	incarnation string
	generation  uint64
	revision    uint64
	value       string
}

// recallLinkMarks remembers, per runtime binding kind, the last binding
// version whose authoritative recall link this process has confirmed, so a
// hook repeated on every status tick does not re-check the link each time.
// Every change of a binding, published here or adopted from a peer, moves
// its revision and so misses the mark. It carries its own lock rather than
// relying on Instance.mu: the Claude candidate retraction clears a mark both
// inside UpdateHookStatus, which holds i.mu, and from UpdateClaudeSession,
// which does not.
type recallLinkMarks struct {
	mu       sync.Mutex
	versions map[string]recallLinkVersion
}

func (m *recallLinkMarks) has(kind string, version recallLinkVersion) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	marked, ok := m.versions[kind]
	return ok && marked == version
}

func (m *recallLinkMarks) set(kind string, version recallLinkVersion) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.versions == nil {
		m.versions = make(map[string]recallLinkVersion)
	}
	m.versions[kind] = version
}

// clear forgets kind's mark only while it still names id.
func (m *recallLinkMarks) clear(kind, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if marked, ok := m.versions[kind]; ok && marked.value == id {
		delete(m.versions, kind)
	}
}
