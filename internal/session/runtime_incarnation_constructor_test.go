package session

import "testing"

func TestRuntimeLifecycle_NewInstancesPreMintUniquePersistenceIncarnations(t *testing.T) {
	plain := NewInstance("plain", t.TempDir())
	tool := NewInstanceWithTool("tool", t.TempDir(), "pi")
	if plain.PersistenceIncarnation() == "" || tool.PersistenceIncarnation() == "" {
		t.Fatalf("new instance tokens must be non-empty: plain=%q tool=%q",
			plain.PersistenceIncarnation(), tool.PersistenceIncarnation())
	}
	if plain.PersistenceIncarnation() == tool.PersistenceIncarnation() {
		t.Fatalf("new instances reused persistence incarnation %q", plain.PersistenceIncarnation())
	}
}
