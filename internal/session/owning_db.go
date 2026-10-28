package session

import (
	"fmt"
	"os"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// withOwningStateDB runs a targeted write against the state database of the
// profile this instance was loaded from or saved to, never against another
// profile's.
//
// Targeted writes (last access, native session bindings, recall links, the
// restart outcome) used to go to statedb.GetGlobal(), the database of the
// profile the process started with. An instance loaded from another profile
// then had its write land in the wrong store, or vanish as a zero-row UPDATE
// (#2206 for the restart outcome). Every such write goes through here,
// whether or not the process has a global handle:
//
//   - the owning handle when it is open (a read-only owning handle is used
//     as is, so its write fails instead of bypassing the read-only load);
//   - when a short-lived Storage closed it, before or during the write, the
//     global handle only if it is the same file, otherwise the instance's
//     saved owning path reopened with its profile stamp checked, so a path
//     that now holds another profile's store is refused. The path is never
//     created: a profile deleted in the meantime stays deleted;
//   - with no saved owning path, the closed handle, so the write fails.
//
// An instance never loaded or saved through a Storage has no owner yet; it
// keeps the old behaviour and uses the global handle, or returns
// errNoStateDB when there is none.
//
// owningDB is guarded by i.mu, so a caller that already holds i.mu uses
// withOwningStateDBLocked instead.
func (i *Instance) withOwningStateDB(write func(*statedb.StateDB) error) error {
	i.mu.RLock()
	owner := i.owningDB
	i.mu.RUnlock()
	return i.withOwningStateDBHandle(owner, write)
}

// withOwningStateDBLocked is withOwningStateDB for a caller holding i.mu.
func (i *Instance) withOwningStateDBLocked(write func(*statedb.StateDB) error) error {
	return i.withOwningStateDBHandle(i.owningDB, write)
}

func (i *Instance) withOwningStateDBHandle(owner *statedb.StateDB, write func(*statedb.StateDB) error) error {
	if owner != nil && !owner.ReadOnly() && owner.DB().Ping() == nil {
		err := write(owner)
		if err == nil || owner.DB().Ping() == nil {
			return err
		}
		// Closed between the ping and the write: retry below.
	}
	db, release, err := i.resolveOwningStateDB(owner, i.storageSnapshot)
	if err != nil {
		return err
	}
	defer release()
	if db == nil {
		return errNoStateDB
	}
	return write(db)
}

// resolveOwningStateDB picks the handle a targeted write uses, by the rules
// withOwningStateDB documents, for writers that run several statements on one
// handle (the fork's runtime binding publication and recall links). snapshot
// is the instance's storage snapshot, which holds the saved owning path. It
// returns nil, with no error, for an instance that has no owner when the
// process has no global handle either. release closes a handle it reopened.
func (i *Instance) resolveOwningStateDB(owner *statedb.StateDB, snapshot *instanceStorageSnapshot) (db *statedb.StateDB, release func(), err error) {
	release = func() {}
	if owner == nil {
		return statedb.GetGlobal(), release, nil
	}
	if owner.ReadOnly() || owner.DB().Ping() == nil {
		return owner, release, nil
	}
	if g := statedb.GetGlobal(); g != nil && !g.ReadOnly() && owner.Path() != "" &&
		g.Path() == owner.Path() && g.DB().Ping() == nil {
		return g, release, nil
	}
	if snapshot == nil || snapshot.dbPath == "" {
		return owner, release, nil
	}
	path := snapshot.dbPath
	if _, err := os.Stat(path); err != nil {
		return nil, release, fmt.Errorf("owning state database %s: %w", path, err)
	}
	reopened, err := statedb.Open(path)
	if err != nil {
		return nil, release, err
	}
	if p := owner.Profile(); p != "" {
		if err := reopened.BindProfile(p); err != nil {
			_ = reopened.Close()
			return nil, release, err
		}
	}
	return reopened, func() { _ = reopened.Close() }, nil
}
