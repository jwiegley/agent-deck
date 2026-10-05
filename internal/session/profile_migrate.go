package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// ProfileMigrateOptions controls a cross-profile migration (issue #928).
type ProfileMigrateOptions struct {
	// Force bypasses the "session is running" safety check. Without this, a
	// running session is refused with a hint to stop it first.
	Force bool
}

// ProfileMigrateResult summarizes what a migration moved. All counts include
// rows that were already present at the destination (skipped idempotently).
type ProfileMigrateResult struct {
	// MovedSessionIDs are the IDs that ended up in the destination DB. This
	// includes IDs that were already there (idempotent re-run).
	MovedSessionIDs []string
	// SkippedIdempotent lists session IDs that were already in the destination
	// (the migration only needed to clean up the source side, if any).
	SkippedIdempotent []string
	// MovedCostEvents is the count of cost_events rows inserted at dst.
	MovedCostEvents int
	// MovedWatcherEvents is the count of watcher_events rows inserted at dst.
	MovedWatcherEvents int
	// CreatedGroups lists group_path values that did not exist in dst and were
	// created by the migration.
	CreatedGroups []string
	// MetaUpdated indicates the conductor meta.json was rewritten with the new
	// profile (conductor migrations only).
	MetaUpdated bool
}

// ErrSessionRunning is returned when a session is in "running" status and
// Force is false. CLI handlers translate this to a non-zero exit with a hint.
var ErrSessionRunning = errors.New("session is running; stop it first or pass --force")

// ErrProfileMissing is returned when the destination profile has no state.db
// (i.e., it has never been used). Profiles are otherwise lazily created.
var ErrProfileMissing = errors.New("target profile does not exist")

// ErrSameProfile is returned when source == target.
var ErrSameProfile = errors.New("source and target profile are the same")

// ErrProfileRuntimeConflict prevents a partial migration retry from deleting
// a source whose destination core metadata, runtime, or bindings differ.
var ErrProfileRuntimeConflict = errors.New("target profile snapshot differs from source")

var (
	profileMigrateBeforeSourceDeleteFn   = func(string) {}
	profileMigrateBeforeTargetRollbackFn = func(string) {}
)

// MigrateSessionsToProfile moves the listed session rows from sourceProfile to
// targetProfile. All associated rows (cost_events, watcher_events linked via
// session_id or triage_session_id) are moved alongside. The session's group
// row is created in the target if missing.
//
// The algorithm is target-write-then-source-delete, with a best-effort
// rollback on source-delete failure. Two SQLite databases cannot share a
// transaction, so we accept a tiny window where a crash between phases would
// leave the row in both DBs — re-running the command cleans this up.
//
// Refuses running sessions unless opts.Force is set. Idempotent: a session
// already in dst is treated as already-migrated and only the source side is
// reconciled.
func MigrateSessionsToProfile(sourceProfile, targetProfile string, sessionIDs []string, opts ProfileMigrateOptions) (*ProfileMigrateResult, error) {
	if sourceProfile == "" {
		sourceProfile = DefaultProfile
	}
	if targetProfile == "" {
		targetProfile = DefaultProfile
	}
	if sourceProfile == targetProfile {
		return nil, ErrSameProfile
	}
	if err := requireProfileExists(targetProfile); err != nil {
		return nil, err
	}

	srcStorage, err := NewStorageWithProfile(sourceProfile)
	if err != nil {
		return nil, fmt.Errorf("open source profile %q: %w", sourceProfile, err)
	}
	defer srcStorage.Close()

	dstStorage, err := NewStorageWithProfile(targetProfile)
	if err != nil {
		return nil, fmt.Errorf("open target profile %q: %w", targetProfile, err)
	}
	defer dstStorage.Close()

	result := &ProfileMigrateResult{}
	for _, id := range sessionIDs {
		if err := migrateOneSession(srcStorage.GetDB(), dstStorage.GetDB(), id, opts, result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// MigrateConductorToProfile moves a conductor session AND every child session
// (where parent_session_id == conductor.ID) from src to dst, then atomically
// rewrites ~/.agent-deck/conductor/<name>/meta.json with the new profile.
//
// Per CLAUDE.md and issue #928: workers MUST travel with their conductor — a
// split would orphan the children's parent_session_id reference.
func MigrateConductorToProfile(name, sourceProfile, targetProfile string, opts ProfileMigrateOptions) (*ProfileMigrateResult, error) {
	if err := ValidateConductorName(name); err != nil {
		return nil, err
	}
	if sourceProfile == "" {
		sourceProfile = DefaultProfile
	}
	if targetProfile == "" {
		targetProfile = DefaultProfile
	}
	if sourceProfile == targetProfile {
		return nil, ErrSameProfile
	}
	if err := requireProfileExists(targetProfile); err != nil {
		return nil, err
	}

	srcStorage, err := NewStorageWithProfile(sourceProfile)
	if err != nil {
		return nil, fmt.Errorf("open source profile %q: %w", sourceProfile, err)
	}
	defer srcStorage.Close()
	dstStorage, err := NewStorageWithProfile(targetProfile)
	if err != nil {
		return nil, fmt.Errorf("open target profile %q: %w", targetProfile, err)
	}
	defer dstStorage.Close()

	src := srcStorage.GetDB()
	dst := dstStorage.GetDB()

	// Find the conductor session in src by title + is_conductor.
	conductorTitle := ConductorSessionTitle(name)
	srcConductor, err := findConductorByTitle(src, conductorTitle)
	if err != nil {
		return nil, err
	}
	dstConductor, err := findConductorByTitle(dst, conductorTitle)
	if err != nil {
		return nil, err
	}
	if srcConductor == nil && dstConductor == nil {
		return nil, fmt.Errorf("conductor %q not found in profile %q", name, sourceProfile)
	}

	// Determine the conductor ID used to look up worker children — fall back
	// to dst when the conductor row itself has already been migrated, so a
	// partial prior run (conductor moved but some workers stranded) still
	// completes on re-run rather than leaving children in the source profile.
	conductorID := ""
	if srcConductor != nil {
		conductorID = srcConductor.ID
	} else {
		conductorID = dstConductor.ID
	}

	// Workers first, conductor last in the migration order. If we ever crash
	// after the conductor row leaves src but before all workers do, a re-run
	// hits the dstConductor != nil branch above with conductorID == dstConductor.ID
	// and still sweeps the stranded workers.
	srcChildren, err := src.LoadInstanceChildren(conductorID)
	if err != nil {
		return nil, fmt.Errorf("load conductor children from src: %w", err)
	}
	ids := make([]string, 0, len(srcChildren)+1)
	for _, c := range srcChildren {
		ids = append(ids, c.ID)
	}
	if srcConductor != nil {
		ids = append(ids, srcConductor.ID)
	}

	result := &ProfileMigrateResult{}
	if dstConductor != nil && srcConductor == nil {
		// Conductor itself is already migrated. Report it as
		// SkippedIdempotent so CLI output reflects what happened.
		result.SkippedIdempotent = append(result.SkippedIdempotent, dstConductor.ID)
		result.MovedSessionIDs = append(result.MovedSessionIDs, dstConductor.ID)
	}
	for _, id := range ids {
		if err := migrateOneSession(src, dst, id, opts, result); err != nil {
			return result, err
		}
	}

	// Rows are now in dst. Update meta.json to point at the new profile.
	// If this fails, the rows are still migrated — the user can re-run the
	// command (it is idempotent) or hand-edit meta.json.
	if err := updateConductorMetaProfile(name, targetProfile); err != nil {
		return result, fmt.Errorf("update conductor meta.json (rows already migrated): %w", err)
	}
	result.MetaUpdated = true
	return result, nil
}

// MigrateGroupToProfile moves every session whose group_path matches groupPath
// from src to dst. The group row itself (preserving expanded/order/default_path)
// is also created in dst. Empty groups are refused.
func MigrateGroupToProfile(groupPath, sourceProfile, targetProfile string, opts ProfileMigrateOptions) (*ProfileMigrateResult, error) {
	if groupPath == "" {
		groupPath = DefaultGroupPath
	}
	if sourceProfile == "" {
		sourceProfile = DefaultProfile
	}
	if targetProfile == "" {
		targetProfile = DefaultProfile
	}
	if sourceProfile == targetProfile {
		return nil, ErrSameProfile
	}
	if err := requireProfileExists(targetProfile); err != nil {
		return nil, err
	}

	srcStorage, err := NewStorageWithProfile(sourceProfile)
	if err != nil {
		return nil, fmt.Errorf("open source profile %q: %w", sourceProfile, err)
	}
	defer srcStorage.Close()
	dstStorage, err := NewStorageWithProfile(targetProfile)
	if err != nil {
		return nil, fmt.Errorf("open target profile %q: %w", targetProfile, err)
	}
	defer dstStorage.Close()

	src := srcStorage.GetDB()
	dst := dstStorage.GetDB()

	rows, err := src.LoadInstancesByGroup(groupPath)
	if err != nil {
		return nil, fmt.Errorf("load source group %q: %w", groupPath, err)
	}
	// Group paths in the DB are sanitized at creation (CreateGroup, line ~698),
	// which replaces "/" with "-" inside a single-name group. Accept the
	// human form ("work/api") by falling back to the sanitized form
	// ("work-api") when the literal lookup is empty.
	if len(rows) == 0 {
		if sanitized := sanitizeGroupName(groupPath); sanitized != groupPath {
			if alt, lerr := src.LoadInstancesByGroup(sanitized); lerr == nil && len(alt) > 0 {
				rows = alt
			}
		}
	}
	// Empty source group is a successful no-op for idempotency: a second run
	// of the same migration finds the source emptied by the first run, which
	// is the desired end state — not an error.
	if len(rows) == 0 {
		return &ProfileMigrateResult{}, nil
	}

	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids) // deterministic order for test stability

	result := &ProfileMigrateResult{}
	for _, id := range ids {
		if err := migrateOneSession(src, dst, id, opts, result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// --- internals ---

// migrateOneSession is the primitive that all three public entrypoints call.
// It mutates the running result struct, appending counts and ids.
func migrateOneSession(src, dst *statedb.StateDB, sessionID string, opts ProfileMigrateOptions, result *ProfileMigrateResult) error {
	release, err := acquireInstanceSpawnLock(sessionID)
	if err != nil {
		return fmt.Errorf("lock session %s for profile migration: %w", sessionID, err)
	}
	defer release()

	srcSnapshot, err := src.LoadMigrationSnapshot(sessionID)
	if err != nil {
		return fmt.Errorf("load source instance %s: %w", sessionID, err)
	}
	dstSnapshot, err := dst.LoadMigrationSnapshot(sessionID)
	if err != nil {
		return fmt.Errorf("load target instance %s: %w", sessionID, err)
	}
	var srcRow, dstRow *statedb.InstanceRow
	if srcSnapshot != nil {
		srcRow = srcSnapshot.Instance
	}
	if dstSnapshot != nil {
		dstRow = dstSnapshot.Instance
	}

	// Pure idempotent no-op: row already at dst, nothing in src.
	if srcRow == nil && dstRow != nil {
		result.MovedSessionIDs = append(result.MovedSessionIDs, sessionID)
		result.SkippedIdempotent = append(result.SkippedIdempotent, sessionID)
		return nil
	}
	if srcRow == nil && dstRow == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	if srcRow != nil && dstRow != nil && !statedb.MigrationCoreMatchesTransfer(srcSnapshot, dstSnapshot) {
		return fmt.Errorf("%w: session %s", ErrProfileRuntimeConflict, sessionID)
	}

	// Running-session guard.
	if srcRow != nil && srcRow.Status == "running" && !opts.Force {
		return fmt.Errorf("%w: session %s (%s)", ErrSessionRunning, sessionID, srcRow.Title)
	}

	costRows := srcSnapshot.CostEvents
	watcherRows := srcSnapshot.WatcherEvents

	// Ensure the destination group exists.
	createdGroup, err := ensureGroupAtDst(src, dst, srcRow.GroupPath)
	if err != nil {
		return fmt.Errorf("ensure group %q in target: %w", srcRow.GroupPath, err)
	}
	if createdGroup {
		result.CreatedGroups = append(result.CreatedGroups, srcRow.GroupPath)
	}

	// Target-write phase. If any step fails, we have not touched src yet —
	// return the error and let the user retry.
	var insertedTargetSnapshot *statedb.MigrationSnapshot
	var insertedTargetCosts []*statedb.CostEventRow
	var insertedTargetWatchers []*statedb.WatcherEventRow
	if dstRow == nil {
		inserted, err := dst.InsertInstanceRowForMigration(srcSnapshot)
		if err != nil {
			return fmt.Errorf("insert instance at target: %w", err)
		}
		insertedTargetSnapshot = inserted
	}
	for _, ev := range costRows {
		inserted, err := dst.InsertCostEventRowForMigration(ev)
		if err != nil {
			rollbackTargetWrites(dst, sessionID, insertedTargetSnapshot, insertedTargetCosts, insertedTargetWatchers)
			return fmt.Errorf("insert cost_event at target: %w", err)
		}
		if inserted != nil {
			insertedTargetCosts = append(insertedTargetCosts, inserted)
		}
		result.MovedCostEvents++
	}
	// Before inserting watcher_events, ensure the referenced watcher row
	// exists at dst — watcher_events.watcher_id is a foreign key to
	// watchers(id) and foreign_keys are ON, so a missing watcher would fail
	// the event insert. Track which watcher_ids we've already checked this
	// call so the round-trip cost is one statedb hit per distinct watcher.
	seenWatchers := make(map[string]bool, 4)
	for _, ev := range watcherRows {
		if !seenWatchers[ev.WatcherID] {
			seenWatchers[ev.WatcherID] = true
			if err := ensureWatcherAtDst(src, dst, ev.WatcherID); err != nil {
				rollbackTargetWrites(dst, sessionID, insertedTargetSnapshot, insertedTargetCosts, insertedTargetWatchers)
				return fmt.Errorf("ensure watcher %q at target: %w", ev.WatcherID, err)
			}
		}
		inserted, err := dst.InsertWatcherEventRowForMigration(ev)
		if err != nil {
			rollbackTargetWrites(dst, sessionID, insertedTargetSnapshot, insertedTargetCosts, insertedTargetWatchers)
			return fmt.Errorf("insert watcher_event at target: %w", err)
		}
		if inserted != nil {
			insertedTargetWatchers = append(insertedTargetWatchers, inserted)
		}
		result.MovedWatcherEvents++
	}

	// Source-delete phase. Production lifecycle deletion shares the instance
	// lock held above. Keep the destination SQLite writer lease from the final
	// validation through the source CAS so an uncoordinated target deletion or
	// replacement cannot interleave and leave both profiles empty.
	profileMigrateBeforeSourceDeleteFn(sessionID)
	if err := dst.WithMigrationTargetLease(srcSnapshot, func() error {
		return src.DeleteMigrationSource(srcSnapshot)
	}); err != nil {
		rollbackTargetWrites(dst, sessionID, insertedTargetSnapshot, insertedTargetCosts, insertedTargetWatchers)
		if errors.Is(err, statedb.ErrMigrationTargetConflict) {
			return fmt.Errorf("%w: session %s changed before source deletion", ErrProfileRuntimeConflict, sessionID)
		}
		return fmt.Errorf("delete instance from source under target lease: %w", err)
	}

	result.MovedSessionIDs = append(result.MovedSessionIDs, sessionID)
	return nil
}

// rollbackTargetWrites removes only rows inserted by this call. A nil snapshot
// means the core pre-existed, so rollback is limited to precisely tracked event
// rows and cannot clobber prior target data.
func rollbackTargetWrites(
	dst *statedb.StateDB,
	sessionID string,
	insertedSnapshot *statedb.MigrationSnapshot,
	insertedCosts []*statedb.CostEventRow,
	insertedWatchers []*statedb.WatcherEventRow,
) {
	profileMigrateBeforeTargetRollbackFn(sessionID)
	_ = dst.RollbackMigrationTarget(insertedSnapshot, insertedCosts, insertedWatchers)
}

// ensureWatcherAtDst copies the watchers row referenced by a watcher_event
// from src to dst if dst lacks it. Required because watcher_events.watcher_id
// has a foreign-key constraint to watchers(id) and foreign_keys are ON.
// No-op if dst already has the watcher (leaving its state field untouched —
// watcher status is owned by whichever process is actually running it).
func ensureWatcherAtDst(src, dst *statedb.StateDB, watcherID string) error {
	existing, err := dst.LoadWatcherByID(watcherID)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	srcWatcher, err := src.LoadWatcherByID(watcherID)
	if err != nil {
		return err
	}
	if srcWatcher == nil {
		// Watcher row is gone from src too — the event references a deleted
		// watcher. Surface a clear error so the user can decide whether to
		// drop the orphaned event manually before migrating.
		return fmt.Errorf("watcher %q is referenced by watcher_events but missing from both source and target", watcherID)
	}
	return dst.SaveWatcher(srcWatcher)
}

// ensureGroupAtDst copies a group row from src to dst if dst lacks it.
// Returns true when the group was newly created.
func ensureGroupAtDst(src, dst *statedb.StateDB, groupPath string) (bool, error) {
	if groupPath == "" || groupPath == DefaultGroupPath {
		return false, nil
	}
	existing, err := dst.LoadGroup(groupPath)
	if err != nil {
		return false, err
	}
	if existing != nil {
		return false, nil
	}
	srcGroup, err := src.LoadGroup(groupPath)
	if err != nil {
		return false, err
	}
	if srcGroup == nil {
		// Source has no explicit group row either; nothing to migrate.
		return false, nil
	}
	if err := dst.SaveGroup(srcGroup); err != nil {
		return false, err
	}
	return true, nil
}

// findConductorByTitle returns the conductor row, or (nil, nil) if absent.
// We match on title AND is_conductor=1 so a session that happens to be
// titled "conductor-foo" without the flag set is not picked up.
func findConductorByTitle(db *statedb.StateDB, title string) (*statedb.InstanceRow, error) {
	// statedb has no by-title query; reuse the children scan plumbing via the
	// LoadInstances API and filter in Go. The instances table is small enough
	// (~10s-100s of rows) that a linear scan is acceptable.
	rows, err := db.LoadInstances()
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.IsConductor && r.Title == title {
			return r, nil
		}
	}
	return nil, nil
}

// updateConductorMetaProfile rewrites the Profile field on
// ~/.agent-deck/conductor/<name>/meta.json atomically. No-op if meta.json
// does not exist (the conductor may have been created without the standard
// `conductor setup` flow).
func updateConductorMetaProfile(name, profile string) error {
	if !IsConductorSetup(name) {
		return nil
	}
	meta, err := LoadConductorMeta(name)
	if err != nil {
		return err
	}
	if meta.Profile == profile {
		return nil
	}
	meta.Profile = profile
	return SaveConductorMeta(meta)
}

// requireProfileExists returns ErrProfileMissing unless the target profile's
// state.db is already on disk. We deliberately bypass NewStorageWithProfile
// here because that function auto-creates the profile directory.
func requireProfileExists(profile string) error {
	dir, err := GetProfileDir(profile)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(dir, "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: profile %q has no state.db (run any agent-deck command with --profile %s first to create it)", ErrProfileMissing, profile, profile)
		}
		return err
	}
	return nil
}
