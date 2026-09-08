package session

import "testing"

// insertTestInstances seeds logical parents through the production insert-only
// boundary. SaveWithGroups is deliberately update-only, so using it to create
// fixtures would make the test pass without putting any session in the DB.
func insertTestInstances(t testing.TB, storage *Storage, instances []*Instance, tree *GroupTree) {
	t.Helper()
	for index, inst := range instances {
		var groups *GroupTree
		if index == len(instances)-1 {
			groups = tree
		}
		if err := storage.InsertSessionAndVerify(inst, groups); err != nil {
			t.Fatalf("InsertSessionAndVerify(%s): %v", inst.ID, err)
		}
	}
}
