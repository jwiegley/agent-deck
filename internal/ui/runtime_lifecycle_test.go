package ui

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_StaleRestartCompletionIsIgnored(t *testing.T) {
	canonical := &session.Instance{
		ID: "one", Title: "canonical", Tool: "pi", Status: session.StatusRunning,
		RuntimeGeneration: 2, StatusRevision: 3,
	}
	home := &Home{
		instances:        []*session.Instance{canonical},
		instanceByID:     map[string]*session.Instance{"one": canonical},
		transitionTokens: make(map[string]uint64),
		resumingSessions: make(map[string]time.Time),
	}
	staleToken := home.beginSessionTransition("one")
	currentToken := home.beginSessionTransition("one")
	home.resumingSessions["one"] = time.Unix(1, 0)

	model, cmd := home.Update(sessionRestartedMsg{
		sessionID: "one",
		token:     staleToken,
		runtime: statedb.RuntimeState{
			InstanceID: "one", Generation: 1, StatusRevision: 0,
			TmuxSession: "stale-runtime", Status: "error",
		},
	})
	if model != home || cmd != nil {
		t.Fatalf("stale completion returned model=%T cmd=%v", model, cmd)
	}
	if home.transitionTokens["one"] != currentToken {
		t.Fatal("stale completion cleared the current transition token")
	}
	if _, ok := home.resumingSessions["one"]; !ok {
		t.Fatal("stale completion cleared the current transition animation")
	}
	if got := canonical.RuntimeState(); got.Generation != 2 || got.StatusRevision != 3 || got.Status != "running" {
		t.Fatalf("stale completion changed canonical runtime: %#v", got)
	}
}

func TestRuntimeLifecycle_OutOfOrderReloadKeepsCanonicalReferences(t *testing.T) {
	setXDGTestHome(t)
	statedb.SetGlobal(nil)
	h := NewHomeWithProfileAndMode("_runtime_lifecycle_out_of_order_reload")
	if h.storage == nil {
		t.Fatal("storage unavailable")
	}
	t.Cleanup(func() {
		statedb.SetGlobal(nil)
		if h.cancel != nil {
			h.cancel()
		}
		if h.storage != nil {
			_ = h.storage.Close()
		}
	})
	h.feedbackDialog = nil
	h.startupReconcileOnce.Do(func() {})

	canonical := session.NewInstanceWithGroup("before", "/tmp/project", "work")
	canonical.ID = "one"
	canonical.ApplyRuntimeState(statedb.RuntimeState{
		InstanceID: "one", Generation: 2, TmuxSession: "g2", Status: "running",
	})
	if err := h.storage.InsertSessionAndVerify(canonical, nil); err != nil {
		t.Fatal(err)
	}
	h.instances = []*session.Instance{canonical}
	h.instanceByID = map[string]*session.Instance{"one": canonical}
	h.groupTree = session.NewGroupTree(h.instances)
	h.search.SetItems(h.instances)
	h.rebuildFlatItems()
	h.cursor = h.flatItemIndexByID("one")

	oldVersion := h.beginReload()
	newVersion := h.beginReload()
	state := h.preserveState()
	acceptedInstances, _, err := h.storage.LoadWithGroups()
	if err != nil || len(acceptedInstances) != 1 {
		t.Fatalf("load accepted snapshot: instances=%d err=%v", len(acceptedInstances), err)
	}
	accepted := acceptedInstances[0]
	accepted.Title = "accepted"
	accepted.ApplyRuntimeState(statedb.RuntimeState{
		InstanceID: "one", Generation: 3, TmuxSession: "g3", Status: "waiting",
	})
	h.Update(loadSessionsMsg{
		instances: []*session.Instance{accepted}, restoreState: &state, reloadVersion: newVersion,
	})

	staleInstances, _, err := h.storage.LoadWithGroups()
	if err != nil || len(staleInstances) != 1 {
		t.Fatalf("load stale snapshot: instances=%d err=%v", len(staleInstances), err)
	}
	stale := staleInstances[0]
	stale.Title = "stale-token"
	stale.ApplyRuntimeState(statedb.RuntimeState{
		InstanceID: "one", Generation: 4, TmuxSession: "g4", Status: "error",
	})
	_, cmd := h.Update(loadSessionsMsg{instances: []*session.Instance{stale}, reloadVersion: oldVersion})
	if cmd != nil {
		t.Fatalf("stale reload returned cmd=%v", cmd)
	}
	if got := canonical.RuntimeState(); canonical.Title != "accepted" || got.Generation != 3 {
		t.Fatalf("stale token mutated canonical: title=%q state=%#v", canonical.Title, got)
	}

	group := h.groupTree.Groups["work"]
	flatIndex := h.flatItemIndexByID("one")
	if group == nil || flatIndex < 0 || len(group.Sessions) != 1 || len(h.search.allItems) != 1 || len(h.search.results) != 1 {
		t.Fatal("rebuilt views missing fixture")
	}
	refs := map[string]*session.Instance{
		"instances": h.instances[0], "instanceByID": h.instanceByID["one"],
		"group": group.Sessions[0], "search.allItems": h.search.allItems[0],
		"search.results": h.search.results[0], "search.Selected": h.search.Selected(),
		"flatItems": h.flatItems[flatIndex].Session, "selected": h.getSelectedSession(),
	}
	for name, got := range refs {
		if got != canonical {
			t.Errorf("%s=%p, want canonical %p", name, got, canonical)
		}
	}
}

func TestRuntimeLifecycle_ForkSeedsBeforeStartAndKeepsPartialSuccess(t *testing.T) {
	setXDGTestHome(t)
	storage, err := session.NewStorageWithProfile("_runtime_lifecycle_fork_seed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	forked := session.NewInstanceWithGroupAndTool("fork", t.TempDir(), "work", "shell")
	candidate := statedb.RuntimeState{
		InstanceID: forked.ID, Generation: 1, StatusRevision: 1,
		TmuxSession: "fork-generation-1", TmuxSocketName: "test-socket", Status: string(session.StatusRunning),
	}
	partial := &session.RestartPartialSuccessError{
		InstanceID: forked.ID, Runtime: candidate, NeedsReconciliation: true,
		Err: errors.New("runtime commit interrupted"),
	}

	startSawParentRow, reconcileCalls := false, 0
	h := &Home{storage: storage}
	h.startRuntimeFn = func(inst *session.Instance) (statedb.RuntimeState, error) {
		row, loadErr := storage.GetDB().LoadInstanceByID(inst.ID)
		if loadErr != nil || row == nil {
			t.Fatalf("StartRuntime parent row=%v err=%v", row, loadErr)
		}
		startSawParentRow = true
		return candidate, partial
	}
	h.reconcileRestartFn = func(inst *session.Instance, got error) error {
		reconcileCalls++
		if inst != forked || !session.IsRestartPartialSuccess(got) {
			t.Fatalf("reconcile got inst=%p err=%v", inst, got)
		}
		return nil
	}

	deps := h.forkInstanceDeps()
	deps.createInstance = func(*session.Instance, string, string, *session.ClaudeOptions) (*session.Instance, error) {
		return forked, nil
	}
	deps.createMultiRepoDir = func(*session.Instance, *session.Instance) error { return nil }
	seedRollbacks, worktreeRollbacks := 0, 0
	realRollbackSeed := deps.rollbackSeed
	deps.rollbackSeed = func(inst *session.Instance, seed statedb.InstanceSeedToken, seeded bool, cause error) (bool, error) {
		seedRollbacks++
		return realRollbackSeed(inst, seed, seeded, cause)
	}
	deps.rollback = func(string, string, string) { worktreeRollbacks++ }

	completed, err := completeForkRuntime(
		&session.Instance{}, "fork", "work", forkToggles{},
		&session.ClaudeOptions{WorktreeRepoRoot: "/repo", WorktreePath: "/repo/worktree", WorktreeBranch: "branch"},
		"", "", true, deps,
	)
	if err != nil {
		t.Fatalf("completeForkRuntime: %v", err)
	}
	if !startSawParentRow || completed.instance != forked || !completed.seeded || completed.runtime != candidate || completed.warning == "" || reconcileCalls != 1 {
		t.Fatalf("completion=%#v sawParent=%v reconcile=%d", completed, startSawParentRow, reconcileCalls)
	}
	if seedRollbacks != 0 || worktreeRollbacks != 0 {
		t.Fatalf("completed runtime rolled back: seed=%d worktree=%d", seedRollbacks, worktreeRollbacks)
	}
	if exists, err := storage.InstanceExists(forked.ID); err != nil || !exists {
		t.Fatalf("parent row exists=%v err=%v", exists, err)
	}
}

func TestRuntimeLifecycle_RolledBackSeedCleanupPreservesSameIDReplacement(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_runtime_lifecycle_seed_cleanup_aba")
	original := session.NewInstanceWithGroupAndTool("original", t.TempDir(), "work", "shell")
	seed, err := storage.InsertSessionIfAbsent(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RollbackSessionSeed(seed); err != nil {
		t.Fatal(err)
	}

	winner := session.NewInstanceWithGroupAndTool("winner", original.ProjectPath, "work", "shell")
	winner.ID = original.ID
	winnerSeed, err := storage.InsertSessionIfAbsent(winner)
	if err != nil {
		t.Fatal(err)
	}
	h.instances = []*session.Instance{winner}
	h.instanceByID = map[string]*session.Instance{winner.ID: winner}
	h.groupTree = session.NewGroupTree(h.instances)

	h.removeRolledBackSeededInstance(winner.ID, seed)
	if len(h.instances) != 1 || h.instances[0] != winner || h.instanceByID[winner.ID] != winner {
		t.Fatal("late cleanup of the old seed removed the same-ID replacement")
	}

	if err := storage.RollbackSessionSeed(winnerSeed); err != nil {
		t.Fatal(err)
	}
	h.removeRolledBackSeededInstance(winner.ID, winnerSeed)
	if len(h.instances) != 0 || h.instanceByID[winner.ID] != nil {
		t.Fatal("exact rolled-back seed remained in memory")
	}
}

func TestRuntimeLifecycle_ReloadReplacementRetiresOldIncarnationTransition(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_runtime_lifecycle_reload_incarnation_aba")
	original := session.NewInstanceWithGroupAndTool("original", t.TempDir(), "work", "shell")
	seed, err := storage.InsertSessionIfAbsent(original)
	if err != nil {
		t.Fatal(err)
	}
	h.instances = []*session.Instance{original}
	h.instanceByID = map[string]*session.Instance{original.ID: original}
	h.groupTree = session.NewGroupTree(h.instances)
	token := h.beginSessionTransitionFor(original)
	_, floor := h.transitionSnapshot(original.ID)
	if !h.deferTransitionReload(original.ID, original, floor, original) {
		t.Fatal("failed to seed deferred reload")
	}

	if err := storage.RollbackSessionSeed(seed); err != nil {
		t.Fatal(err)
	}
	replacement := session.NewInstanceWithGroupAndTool("replacement", original.ProjectPath, "work", "shell")
	replacement.ID = original.ID
	if _, err := storage.InsertSessionIfAbsent(replacement); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("loaded replacement=%d err=%v", len(loaded), err)
	}

	canonical := h.mergeReloadedInstance(loaded[0], original)
	if canonical != loaded[0] || canonical.Title != "replacement" {
		t.Fatalf("reload retained old incarnation: got=%p title=%q want=%p", canonical, canonical.Title, loaded[0])
	}
	if h.transitionCurrent(original.ID, token) {
		t.Fatal("same-ID replacement did not retire the old incarnation transition")
	}
	if deferred := h.transitionDeferred[original.ID]; deferred != nil {
		t.Fatalf("same-ID replacement retained deferred metadata from old incarnation: %p", deferred)
	}
	if original.Title != "original" {
		t.Fatalf("replacement metadata was merged into old object: title=%q", original.Title)
	}
}

func TestRuntimeLifecycle_ReloadDefersLaunchMetadataUntilTransitionCompletes(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_runtime_lifecycle_deferred_reload")
	canonical := session.NewInstanceWithGroupAndTool("before", t.TempDir(), "work", "claude")
	canonical.Command = "old-command"
	canonical.SandboxContainer = "successor-container"
	canonical.LoadedMCPNames = []string{"successor-mcp"}
	if _, err := storage.InsertSessionIfAbsent(canonical); err != nil {
		t.Fatal(err)
	}
	peer := session.NewInstanceWithGroupAndTool("peer", t.TempDir(), "later", "shell")
	peer.Status = session.StatusRunning
	peer.Order = 2
	if _, err := storage.InsertSessionIfAbsent(peer); err != nil {
		t.Fatal(err)
	}
	h.instances = []*session.Instance{canonical, peer}
	h.instanceByID = map[string]*session.Instance{canonical.ID: canonical}
	h.instanceByID[peer.ID] = peer
	h.groupTree = session.NewGroupTree(h.instances)
	h.search = NewSearch()
	h.search.SetItems(h.instances)
	h.rebuildFlatItems()

	token := h.beginSessionTransitionFor(canonical)
	entered := make(chan struct{})
	release := make(chan struct{})
	observed := make(chan [2]string, 1)
	go func() {
		close(entered)
		<-release
		observed <- [2]string{canonical.Tool, canonical.Command}
	}()
	<-entered

	successor := canonical.RuntimeState()
	successor.Generation++
	successor.StatusRevision = 0
	successor.TmuxSession = "successor"
	successor.TmuxSocketName = "isolated"
	successor.Status = string(session.StatusRunning)
	if err := storage.GetDB().CommitRuntimeTransitionWithBindingPlan(
		canonical.RuntimeState().Generation, canonical.PersistenceIncarnation(), successor, nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetDB().DB().Exec(`UPDATE instances
		SET title = 'after', tool = 'codex', command = 'new-command'
		WHERE id = ?`, canonical.ID); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 2 {
		t.Fatalf("load deferred metadata: instances=%d err=%v", len(loaded), err)
	}
	var detached *session.Instance
	for _, candidate := range loaded {
		if candidate.ID == canonical.ID {
			detached = candidate
			break
		}
	}
	if detached == nil {
		t.Fatal("deferred reload omitted canonical session")
	}
	if got := h.mergeReloadedInstance(detached, canonical); got != canonical {
		t.Fatalf("reload replaced canonical pointer: got=%p want=%p", got, canonical)
	}
	if canonical.Title != "before" || canonical.Tool != "claude" || canonical.Command != "old-command" {
		t.Fatalf("launch metadata changed in flight: title=%q tool=%q command=%q",
			canonical.Title, canonical.Tool, canonical.Command)
	}
	close(release)
	if got := <-observed; got != [2]string{"claude", "old-command"} {
		t.Fatalf("transition observed reloaded launch inputs: %v", got)
	}
	if canonical.RuntimeState() != successor {
		t.Fatalf("runtime-only reload did not publish successor: got=%#v want=%#v", canonical.RuntimeState(), successor)
	}

	// A later durable edit must win over the snapshot captured above, while an
	// explicit local rename awaiting persistence must win over both.
	if _, err := storage.GetDB().DB().Exec(`UPDATE instances
		SET title = 'durable-newest', tool = 'pi', command = 'newest-command',
		    group_path = 'later', sort_order = 7
		WHERE id = ?`, canonical.ID); err != nil {
		t.Fatal(err)
	}
	canonical.SetTitleThreadSafe("local-pending")
	canonical.TitleLocked = true
	if h.pendingTitleChanges == nil {
		h.pendingTitleChanges = make(map[string]pendingTitle)
	}
	h.pendingTitleChanges[canonical.ID] = pendingTitle{title: "local-pending", locked: true}
	h.search.input.SetValue("local-pending")
	h.cursor = h.flatItemIndexByID(canonical.ID)
	if !h.finishSessionTransition(canonical.ID, token) {
		t.Fatal("deferred metadata was not drained")
	}
	h.resyncDeferredTransitionViews()
	if canonical.Title != "local-pending" || canonical.Tool != "pi" || canonical.Command != "newest-command" {
		t.Fatalf("deferred metadata was not drained: title=%q tool=%q command=%q",
			canonical.Title, canonical.Tool, canonical.Command)
	}
	if canonical.GroupPath != "later" || canonical.Order != 7 {
		t.Fatalf("deferred placement was not drained: group=%q order=%d", canonical.GroupPath, canonical.Order)
	}
	if canonical.RuntimeState() != successor {
		t.Fatalf("deferred metadata changed successor runtime: got=%#v want=%#v", canonical.RuntimeState(), successor)
	}
	if canonical.SandboxContainer != "successor-container" ||
		!slices.Equal(canonical.LoadedMCPNames, []string{"successor-mcp"}) {
		t.Fatalf("deferred metadata clobbered transition outputs: container=%q mcps=%v",
			canonical.SandboxContainer, canonical.LoadedMCPNames)
	}
	if group := h.groupTree.Groups["work"]; group != nil && len(group.Sessions) != 0 {
		t.Fatalf("old group retained moved session: %#v", group.Sessions)
	}
	group := h.groupTree.Groups["later"]
	if group == nil || len(group.Sessions) != 2 || group.Sessions[0] != peer || group.Sessions[1] != canonical {
		t.Fatalf("group tree was not reindexed by persisted order: %#v", group)
	}
	if len(h.search.allItems) != 2 || len(h.search.results) != 1 || h.search.results[0] != canonical {
		t.Fatalf("search index was not refreshed: all=%#v results=%#v", h.search.allItems, h.search.results)
	}
	peerFlat, canonicalFlat := -1, -1
	for index, item := range h.flatItems {
		if item.Type == session.ItemTypeSession {
			switch item.Session {
			case peer:
				peerFlat = index
			case canonical:
				canonicalFlat = index
			}
		}
	}
	if peerFlat < 0 || canonicalFlat <= peerFlat {
		t.Fatalf("flat index was not rebuilt in destination order: peer=%d canonical=%d", peerFlat, canonicalFlat)
	}
	if selected := h.getSelectedSession(); selected != canonical {
		t.Fatalf("derived-view rebuild lost selected session: got=%p want=%p", selected, canonical)
	}

	h.forceSaveInstances()
	persisted, _, err := storage.LoadWithGroups()
	if err != nil || len(persisted) != 2 {
		t.Fatalf("reload persisted metadata: instances=%d err=%v", len(persisted), err)
	}
	var saved *session.Instance
	for _, candidate := range persisted {
		if candidate.ID == canonical.ID {
			saved = candidate
			break
		}
	}
	if saved == nil {
		t.Fatal("persisted reload omitted canonical session")
	}
	if saved.Title != "local-pending" || saved.Tool != "pi" || saved.Command != "newest-command" {
		t.Fatalf("persisted metadata regressed: title=%q tool=%q command=%q",
			saved.Title, saved.Tool, saved.Command)
	}
}

func TestRuntimeLifecycle_DelayedDeleteMessagePreservesSameIDReplacement(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_runtime_lifecycle_delete_message_aba")
	original := seedSession(t, storage, nil, "delete-message-aba", "original")
	if err := h.HydrateInstancesFromStorage(); err != nil {
		t.Fatal(err)
	}
	deleted := h.instanceByID[original.ID]
	msg := h.removeSession(deleted)()

	replacement := seedSession(t, storage, nil, original.ID, "replacement")
	h.instances = []*session.Instance{replacement}
	h.instanceByID = map[string]*session.Instance{replacement.ID: replacement}
	h.groupTree = session.NewGroupTree(h.instances)
	undoBefore := len(h.undoStack)
	h.Update(msg)

	if len(h.instances) != 1 || h.instances[0] != replacement || h.instanceByID[replacement.ID] != replacement {
		t.Fatal("delayed delete message removed the same-ID replacement")
	}
	if len(h.undoStack) != undoBefore {
		t.Fatal("same-ID replacement was added to the deleted incarnation's undo history")
	}
}

func TestRuntimeLifecycle_DelayedArchiveMessagePreservesSameIDReplacement(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_runtime_lifecycle_archive_message_aba")
	original := session.NewInstanceWithGroupAndTool("original", t.TempDir(), "work", "shell")
	seed, err := storage.InsertSessionIfAbsent(original)
	if err != nil {
		t.Fatal(err)
	}
	msg := sessionArchivedMsg{
		sessionID: original.ID, incarnation: original.PersistenceIncarnation(),
		runtime: original.RuntimeState(), archivedAt: time.Unix(12, 0).UTC(),
	}
	if err := storage.RollbackSessionSeed(seed); err != nil {
		t.Fatal(err)
	}

	replacement := session.NewInstanceWithGroupAndTool("replacement", original.ProjectPath, "work", "shell")
	replacement.ID = original.ID
	if _, err := storage.InsertSessionIfAbsent(replacement); err != nil {
		t.Fatal(err)
	}
	h.instances = []*session.Instance{replacement}
	h.instanceByID = map[string]*session.Instance{replacement.ID: replacement}
	h.groupTree = session.NewGroupTree(h.instances)
	h.Update(msg)

	if replacement.IsArchived() {
		t.Fatal("delayed archive completion hid the same-ID replacement")
	}
	row, err := storage.GetDB().LoadInstanceByID(replacement.ID)
	if err != nil || row == nil || !row.ArchivedAt.IsZero() {
		t.Fatalf("replacement archive row=%#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_WebCreateSeedsBeforeStartAndKeepsPartialSuccess(t *testing.T) {
	setXDGTestHome(t)
	statedb.SetGlobal(nil)
	h := NewHomeWithProfileAndMode("_runtime_lifecycle_web_seed")
	if h.storage == nil {
		t.Fatal("storage unavailable")
	}
	t.Cleanup(func() {
		statedb.SetGlobal(nil)
		_ = h.storage.Close()
	})
	m := NewWebMutator(h)
	m.reconcileRestartFn = func(*session.Instance, error) error { return nil }
	startSawParentRow := false
	m.startRuntimeFn = func(inst *session.Instance) (statedb.RuntimeState, error) {
		row, err := h.storage.GetDB().LoadInstanceByID(inst.ID)
		if err != nil || row == nil {
			t.Fatalf("StartRuntime parent row=%v err=%v", row, err)
		}
		startSawParentRow = true
		candidate := statedb.RuntimeState{
			InstanceID: inst.ID, Generation: 1, StatusRevision: 1,
			TmuxSession: "web-generation-1", TmuxSocketName: "test", Status: "running",
		}
		if err := h.storage.GetDB().CommitRuntimeTransitionWithBindingPlan(
			0, inst.PersistenceIncarnation(), candidate, nil); err != nil {
			t.Fatal(err)
		}
		return candidate, &session.RestartPartialSuccessError{
			InstanceID: inst.ID, Runtime: candidate, NeedsReconciliation: true,
			Err: errors.New("acknowledgment interrupted"),
		}
	}

	id, err := m.CreateSession("web", "shell", t.TempDir(), "work", "", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !startSawParentRow {
		t.Fatal("StartRuntime did not observe its durable parent row")
	}
	if durable, found, err := h.storage.GetDB().ReadRuntimeState(id); err != nil || !found || durable.Generation != 1 {
		t.Fatalf("durable state=%#v found=%v err=%v", durable, found, err)
	}
}

func TestRuntimeLifecycle_StartupReconcileAdoptsUniqueAndReportsAmbiguity(t *testing.T) {
	t.Run("unique", func(t *testing.T) {
		inst := &session.Instance{ID: "one", RuntimeGeneration: 1, Status: session.StatusIdle}
		winner := statedb.RuntimeState{InstanceID: "one", Generation: 2, TmuxSession: "winner", Status: "running"}
		h := &Home{reconcileRuntimeFn: func(*session.Instance) (session.RuntimeReconciliationResult, error) {
			return session.RuntimeReconciliationResult{State: winner, Live: true, Adopted: true}, nil
		}}
		if warnings := h.reconcileStartupInstances([]*session.Instance{inst}); len(warnings) != 0 || inst.RuntimeState().Generation != 2 {
			t.Fatalf("warnings=%v state=%#v", warnings, inst.RuntimeState())
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		inst := &session.Instance{ID: "one", RuntimeGeneration: 1, Status: session.StatusIdle}
		candidates := []tmux.RuntimeCandidate{
			{SocketName: "socket-a", SessionName: "runtime-a", Generation: 2, GenerationKnown: true, PanePID: 101},
			{SocketName: "socket-b", SessionName: "runtime-b", PanePID: 202, ProofError: "missing generation"},
		}
		h := &Home{reconcileRuntimeFn: func(*session.Instance) (session.RuntimeReconciliationResult, error) {
			return session.RuntimeReconciliationResult{Candidates: candidates}, &session.RuntimeReconciliationAmbiguityError{
				InstanceID: "one", Reason: "multiple candidates", Candidates: candidates,
			}
		}}
		warnings := h.reconcileStartupInstances([]*session.Instance{inst})
		if len(warnings) != 1 || inst.RuntimeState().Generation != 1 {
			t.Fatalf("warnings=%v state=%#v", warnings, inst.RuntimeState())
		}
		for _, want := range []string{
			"runtime reconciliation for one is ambiguous",
			`socket="socket-a" session="runtime-a" generation=2 known=true pid=101 proof=""`,
			`socket="socket-b" session="runtime-b" generation=0 known=false pid=202 proof="missing generation"`,
		} {
			if !strings.Contains(warnings[0], want) {
				t.Errorf("warning %q lacks %q", warnings[0], want)
			}
		}
	})
}

func TestRuntimeLifecycle_LoadSessionsRunsStartupReconciliation(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_runtime_lifecycle_startup_wiring")
	inst := session.NewInstanceWithGroupAndTool("one", t.TempDir(), "work", "pi")
	if _, err := storage.InsertSessionIfAbsent(inst); err != nil {
		t.Fatal(err)
	}
	want := inst.RuntimeState()
	want.Generation++
	want.StatusRevision = 2
	want.TmuxSession = "reconciled-runtime"
	want.TmuxSocketName = "isolated"
	want.Status = string(session.StatusRunning)
	calls := 0
	h.reconcileRuntimeFn = func(got *session.Instance) (session.RuntimeReconciliationResult, error) {
		calls++
		if got.ID != inst.ID {
			t.Fatalf("reconciled instance=%q want=%q", got.ID, inst.ID)
		}
		return session.RuntimeReconciliationResult{State: want, Live: true}, nil
	}

	rawMsg := h.loadSessions()
	msg, ok := rawMsg.(loadSessionsMsg)
	if !ok {
		t.Fatalf("loadSessions message type=%T, want loadSessionsMsg", rawMsg)
	}
	if msg.err != nil || calls != 1 || len(msg.instances) != 1 {
		t.Fatalf("loadSessions err=%v calls=%d instances=%d", msg.err, calls, len(msg.instances))
	}
	if got := msg.instances[0].RuntimeState(); got != want {
		t.Fatalf("loadSessions published unreconciled runtime: got=%#v want=%#v", got, want)
	}
}

func TestRuntimeLifecycle_LoadMessageDoesNotRunStartupInventory(t *testing.T) {
	setXDGTestHome(t)
	statedb.SetGlobal(nil)
	h := NewHomeWithProfileAndMode("_runtime_lifecycle_async_startup")
	if h.storage == nil {
		t.Fatal("storage unavailable")
	}
	t.Cleanup(func() {
		statedb.SetGlobal(nil)
		if h.cancel != nil {
			h.cancel()
		}
		_ = h.storage.Close()
	})
	h.feedbackDialog = nil
	called := 0
	h.reconcileRuntimeFn = func(*session.Instance) (session.RuntimeReconciliationResult, error) {
		called++
		return session.RuntimeReconciliationResult{}, nil
	}
	inst := session.NewInstanceWithGroupAndTool("one", t.TempDir(), "work", "pi")
	reloadVersion := h.beginReload()
	h.Update(loadSessionsMsg{instances: []*session.Instance{inst}, reloadVersion: reloadVersion})
	if called != 0 {
		t.Fatalf("loadSessionsMsg ran %d startup inventories on the Bubble Tea event loop", called)
	}
}

func TestRuntimeLifecycle_SharedStatusCannotRegressRuntimeGeneration(t *testing.T) {
	inst := &session.Instance{ID: "one", RuntimeGeneration: 2, StatusRevision: 3, Status: session.StatusRunning}
	changed := applySharedStatusRow(inst, statedb.StatusRow{
		Status: "error", Generation: 1, StatusRevision: 9,
	}, func(string) (statedb.RuntimeState, bool, error) {
		return statedb.RuntimeState{InstanceID: "one", Generation: 1, StatusRevision: 9, Status: "error"}, true, nil
	})
	if changed {
		t.Fatal("stale shared row reported an applied status change")
	}
	if got := inst.RuntimeState(); got.Generation != 2 || got.StatusRevision != 3 || got.Status != "running" {
		t.Fatalf("stale shared row regressed runtime: %#v", got)
	}
}

func TestRuntimeLifecycle_TUIArchiveCompletionRejectsReplacementRuntime(t *testing.T) {
	setXDGTestHome(t)
	storage, err := session.NewStorageWithProfile("_runtime_lifecycle_archive_fence")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := session.NewInstanceWithGroupAndTool("archive", t.TempDir(), "work", "pi")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	running := statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1,
		TmuxSocketName: "isolated", Status: string(session.StatusRunning),
		LastStartedAt: time.Unix(2, 0).UTC(),
	}
	if err := storage.GetDB().CommitRuntimeTransition(0, inst.PersistenceIncarnation(), running); err != nil {
		t.Fatal(err)
	}
	inst.ApplyRuntimeState(running)
	h := &Home{
		storage: storage, instances: []*session.Instance{inst},
		instanceByID: map[string]*session.Instance{inst.ID: inst},
	}
	msg, ok := h.archiveSession(inst)().(sessionArchivedMsg)
	if !ok || msg.killErr != nil || msg.runtime.Status != string(session.StatusStopped) {
		t.Fatalf("archive completion = %#v", msg)
	}
	replacement := msg.runtime
	replacement.Generation++
	replacement.StatusRevision = 0
	replacement.TmuxSession = "runtime-g2"
	replacement.Status = string(session.StatusRunning)
	replacement.LastStartedAt = time.Unix(3, 0).UTC()
	if err := storage.GetDB().CommitRuntimeTransition(
		msg.runtime.Generation, msg.incarnation, replacement); err != nil {
		t.Fatal(err)
	}
	h.updateInner(msg)
	if inst.IsArchived() {
		t.Fatal("stale TUI archive completion hid replacement runtime in memory")
	}
	row, err := storage.GetDB().LoadInstanceByID(inst.ID)
	if err != nil || row == nil || !row.ArchivedAt.IsZero() {
		t.Fatalf("replacement archive row=%#v err=%v", row, err)
	}
	if h.err == nil || !errors.Is(h.err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("TUI archive conflict = %v", h.err)
	}
}

func TestRuntimeLifecycle_WebArchiveCompletionRejectsReplacementRuntime(t *testing.T) {
	setXDGTestHome(t)
	storage, err := session.NewStorageWithProfile("_runtime_lifecycle_web_archive_fence")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := session.NewInstanceWithGroupAndTool("archive", t.TempDir(), "work", "pi")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	running := statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1,
		TmuxSocketName: "isolated", Status: string(session.StatusRunning),
		LastStartedAt: time.Unix(2, 0).UTC(),
	}
	if err := storage.GetDB().CommitRuntimeTransition(0, inst.PersistenceIncarnation(), running); err != nil {
		t.Fatal(err)
	}
	inst.ApplyRuntimeState(running)
	h := &Home{
		storage: storage, instances: []*session.Instance{inst},
		instanceByID: map[string]*session.Instance{inst.ID: inst},
	}
	m := NewWebMutator(h)
	m.archiveAfterKillFn = func(killed statedb.RuntimeState) {
		replacement := killed
		replacement.Generation++
		replacement.StatusRevision = 0
		replacement.TmuxSession = "runtime-g2"
		replacement.Status = string(session.StatusRunning)
		replacement.LastStartedAt = time.Unix(3, 0).UTC()
		if err := storage.GetDB().CommitRuntimeTransition(
			killed.Generation, inst.PersistenceIncarnation(), replacement); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.ArchiveSession(inst.ID); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("stale web archive error = %v, want generation conflict", err)
	}
	if inst.IsArchived() {
		t.Fatal("stale web archive completion hid replacement runtime in memory")
	}
	row, err := storage.GetDB().LoadInstanceByID(inst.ID)
	if err != nil || row == nil || !row.ArchivedAt.IsZero() {
		t.Fatalf("replacement archive row=%#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_UnarchiveCallersRejectReplacementIncarnation(t *testing.T) {
	newFixture := func(t *testing.T, profile string) (*session.Storage, *session.Instance, time.Time) {
		t.Helper()
		setXDGTestHome(t)
		storage, err := session.NewStorageWithProfile(profile)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = storage.Close() })
		archivedAt := time.Unix(44, 0).UTC()
		inst := session.NewInstanceWithGroupAndTool("archived A", t.TempDir(), "work", "pi")
		inst.ArchivedAt = archivedAt
		if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
			t.Fatal(err)
		}
		return storage, inst, archivedAt
	}
	replace := func(t *testing.T, storage *session.Storage, original *session.Instance, archivedAt time.Time) *session.Instance {
		t.Helper()
		if err := storage.GetDB().DeleteInstance(original.ID); err != nil {
			t.Fatal(err)
		}
		replacement := session.NewInstanceWithGroupAndTool("archived B", t.TempDir(), "work", "pi")
		replacement.ID = original.ID
		replacement.ArchivedAt = archivedAt
		if err := storage.InsertSessionAndVerify(replacement, nil); err != nil {
			t.Fatal(err)
		}
		return replacement
	}

	t.Run("TUI", func(t *testing.T) {
		storage, original, archivedAt := newFixture(t, "_runtime_lifecycle_tui_unarchive_aba")
		h := &Home{
			storage: storage, instances: []*session.Instance{original},
			instanceByID: map[string]*session.Instance{original.ID: original},
		}
		cmd := h.unarchiveSession(original)
		replacement := replace(t, storage, original, archivedAt)
		h.instances = []*session.Instance{replacement}
		h.instanceByID[original.ID] = replacement

		h.updateInner(cmd())
		if h.err == nil || !errors.Is(h.err, statedb.ErrInstanceParentConflict) {
			t.Fatalf("stale TUI unarchive error = %v, want parent conflict", h.err)
		}
		if !replacement.ArchivedAt.Equal(archivedAt) {
			t.Fatalf("replacement unarchived in memory: %v", replacement.ArchivedAt)
		}
		row, err := storage.GetDB().LoadInstanceByID(original.ID)
		if err != nil || row == nil || !row.ArchivedAt.Equal(archivedAt) {
			t.Fatalf("replacement unarchive row=%#v err=%v", row, err)
		}
	})

	t.Run("web", func(t *testing.T) {
		storage, original, archivedAt := newFixture(t, "_runtime_lifecycle_web_unarchive_aba")
		h := &Home{
			storage: storage, instances: []*session.Instance{original},
			instanceByID: map[string]*session.Instance{original.ID: original},
		}
		_ = replace(t, storage, original, archivedAt)
		if err := NewWebMutator(h).UnarchiveSession(original.ID); !errors.Is(err, statedb.ErrInstanceParentConflict) {
			t.Fatalf("stale web unarchive error = %v, want parent conflict", err)
		}
		if !original.ArchivedAt.Equal(archivedAt) {
			t.Fatalf("stale web object was unarchived: %v", original.ArchivedAt)
		}
		row, err := storage.GetDB().LoadInstanceByID(original.ID)
		if err != nil || row == nil || !row.ArchivedAt.Equal(archivedAt) {
			t.Fatalf("replacement unarchive row=%#v err=%v", row, err)
		}
	})
}

func TestRuntimeLifecycle_DelayedTUICommandsRejectSameIDReplacementBeforeMutation(t *testing.T) {
	replacePending := func(h *Home, original, replacement *session.Instance) {
		h.instancesMu.Lock()
		h.instances = []*session.Instance{replacement}
		h.instanceByID[original.ID] = replacement
		h.instancesMu.Unlock()
	}
	newPair := func(t *testing.T) (*Home, *session.Instance, *session.Instance) {
		t.Helper()
		original := session.NewInstanceWithGroupAndTool("A", t.TempDir(), "work", "pi")
		replacement := session.NewInstanceWithGroupAndTool("B", t.TempDir(), "work", "pi")
		replacement.ID = original.ID
		if replacement.PersistenceIncarnation() == original.PersistenceIncarnation() {
			t.Fatal("test replacement reused persistence incarnation")
		}
		h := &Home{
			instances:    []*session.Instance{original},
			instanceByID: map[string]*session.Instance{original.ID: original},
		}
		return h, original, replacement
	}

	t.Run("same incarnation keeps selected pointer", func(t *testing.T) {
		setXDGTestHome(t)
		storage, err := session.NewStorageWithProfile("_runtime_lifecycle_selected_pointer")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = storage.Close() })
		selected := session.NewInstanceWithGroupAndTool("selected", t.TempDir(), "work", "pi")
		if err := storage.InsertSessionAndVerify(selected, nil); err != nil {
			t.Fatal(err)
		}
		loaded, _, err := storage.LoadWithGroups()
		if err != nil || len(loaded) != 1 {
			t.Fatalf("load detached pointer: instances=%d err=%v", len(loaded), err)
		}
		current := loaded[0]
		if current == selected || current.PersistenceIncarnation() != selected.PersistenceIncarnation() {
			t.Fatalf("detached pointer=%p selected=%p incarnations current=%q selected=%q",
				current, selected, current.PersistenceIncarnation(), selected.PersistenceIncarnation())
		}

		h := &Home{
			instances:    []*session.Instance{selected},
			instanceByID: map[string]*session.Instance{selected.ID: selected},
		}
		var restarted *session.Instance
		h.restartRuntimeFn = func(got *session.Instance) (statedb.RuntimeState, error) {
			restarted = got
			return statedb.RuntimeState{}, nil
		}
		cmd := h.restartSession(selected)
		h.instances = []*session.Instance{current}
		h.instanceByID[selected.ID] = current
		msg := cmd().(sessionRestartedMsg)
		if msg.err != nil || restarted != selected {
			t.Fatalf("restart err=%v pointer=%p, want selected %p", msg.err, restarted, selected)
		}
	})

	t.Run("restart", func(t *testing.T) {
		h, original, replacement := newPair(t)
		restarted := false
		h.restartRuntimeFn = func(*session.Instance) (statedb.RuntimeState, error) {
			restarted = true
			return statedb.RuntimeState{}, nil
		}
		cmd := h.restartSession(original)
		replacePending(h, original, replacement)
		msg := cmd().(sessionRestartedMsg)
		if !errors.Is(msg.err, statedb.ErrInstanceParentConflict) || restarted {
			t.Fatalf("restart result err=%v restarted=%v", msg.err, restarted)
		}
	})

	t.Run("fresh restart", func(t *testing.T) {
		h, original, replacement := newPair(t)
		restarted := false
		h.restartFreshRuntimeFn = func(*session.Instance) (statedb.RuntimeState, error) {
			restarted = true
			return statedb.RuntimeState{}, nil
		}
		cmd := h.restartSessionFresh(original)
		replacePending(h, original, replacement)
		msg := cmd().(sessionRestartedMsg)
		if !errors.Is(msg.err, statedb.ErrInstanceParentConflict) || restarted {
			t.Fatalf("fresh restart result err=%v restarted=%v", msg.err, restarted)
		}
	})

	t.Run("multi-repo filesystem", func(t *testing.T) {
		h, original, replacement := newPair(t)
		tempDir := t.TempDir()
		original.MultiRepoTempDir = tempDir
		sentinel := filepath.Join(tempDir, "keep")
		if err := os.WriteFile(sentinel, []byte("selected A"), 0o600); err != nil {
			t.Fatal(err)
		}
		restarted := false
		h.restartRuntimeFn = func(*session.Instance) (statedb.RuntimeState, error) {
			restarted = true
			return statedb.RuntimeState{}, nil
		}
		cmd := h.applyMultiRepoPathChanges(original, []string{t.TempDir()})
		replacePending(h, original, replacement)
		msg := cmd().(sessionRestartedMsg)
		if !errors.Is(msg.err, statedb.ErrInstanceParentConflict) || restarted {
			t.Fatalf("multi-repo result err=%v restarted=%v", msg.err, restarted)
		}
		if got, err := os.ReadFile(sentinel); err != nil || string(got) != "selected A" {
			t.Fatalf("filesystem mutated before replacement rejection: got=%q err=%v", got, err)
		}
	})
}

func TestRuntimeLifecycle_HomeBindingObservationsRejectTransitionRaces(t *testing.T) {
	statedb.SetGlobal(nil)
	t.Cleanup(func() { statedb.SetGlobal(nil) })

	t.Run("restored OpenCode publishes", func(t *testing.T) {
		inst := &session.Instance{ID: "opencode-new", Tool: "opencode"}
		home := &Home{
			instances:    []*session.Instance{inst},
			instanceByID: map[string]*session.Instance{inst.ID: inst},
		}
		home.updateInner(openCodeDetectionCompleteMsg{
			instanceID: inst.ID,
			sessionID:  "candidate",
			observed:   inst.CaptureRuntimeBindingObservation("opencode"),
			detectedAt: time.Unix(50, 0).UTC(),
		})
		if binding := inst.RuntimeBindings["opencode"]; inst.OpenCodeSessionID != "candidate" || binding.Value != "candidate" || binding.Revision != 1 {
			t.Fatalf("OpenCode observation was not published: id=%q binding=%#v", inst.OpenCodeSessionID, binding)
		}
	})

	t.Run("restored OpenCode", func(t *testing.T) {
		inst := &session.Instance{
			ID: "opencode-one", Tool: "opencode", RuntimeGeneration: 1,
			OpenCodeSessionID: "winner",
			RuntimeBindings: map[string]statedb.RuntimeBinding{
				"opencode": {
					InstanceID: "opencode-one", Kind: "opencode", Generation: 1,
					Revision: 1, Value: "winner",
				},
			},
		}
		observed := inst.CaptureRuntimeBindingObservation("opencode")
		inst.ApplyRuntimeState(statedb.RuntimeState{InstanceID: inst.ID, Generation: 2, Status: "running"})
		home := &Home{
			instances:    []*session.Instance{inst},
			instanceByID: map[string]*session.Instance{inst.ID: inst},
		}
		home.updateInner(openCodeDetectionCompleteMsg{
			instanceID: inst.ID,
			sessionID:  "stale-candidate",
			observed:   observed,
			detectedAt: time.Unix(100, 0).UTC(),
		})
		if inst.OpenCodeSessionID != "winner" {
			t.Fatalf("stale OpenCode observation replaced winner with %q", inst.OpenCodeSessionID)
		}
	})

	t.Run("live Claude content", func(t *testing.T) {
		inst := &session.Instance{
			ID: "claude-one", Tool: "claude", RuntimeGeneration: 1,
			ClaudeSessionID: "winner",
			RuntimeBindings: map[string]statedb.RuntimeBinding{
				"claude": {
					InstanceID: "claude-one", Kind: "claude", Generation: 1,
					Revision: 1, Value: "winner",
				},
			},
		}
		observed := inst.CaptureRuntimeBindingObservation("claude")
		inst.ApplyRuntimeState(statedb.RuntimeState{InstanceID: inst.ID, Generation: 2, Status: "running"})
		_, err := getSessionContentWithLiveObservation(inst, observed, "stale-candidate")
		if !errors.Is(err, session.ErrRuntimeBindingObservationStale) {
			t.Fatalf("live-content stale observation error = %v", err)
		}
		if inst.ClaudeSessionID != "winner" {
			t.Fatalf("stale Claude observation replaced winner with %q", inst.ClaudeSessionID)
		}
	})
}

func TestRuntimeLifecycle_TransitionReloadCompletionSaveKeepsCanonicalRuntime(t *testing.T) {
	setXDGTestHome(t)
	h := NewHomeWithProfileAndMode("_runtime_lifecycle_reload")
	if h.storage == nil {
		t.Fatal("storage unavailable")
	}
	t.Cleanup(func() {
		statedb.SetGlobal(nil)
		_ = h.storage.Close()
	})
	h.feedbackDialog = nil
	h.startupReconcileOnce.Do(func() {})

	canonical := session.NewInstanceWithGroupAndTool("before", t.TempDir(), "work", "codex")
	if err := h.storage.InsertSessionAndVerify(canonical, nil); err != nil {
		t.Fatal(err)
	}
	loadDetached := func(label string) *session.Instance {
		t.Helper()
		instances, _, err := h.storage.LoadWithGroups()
		if err != nil || len(instances) != 1 {
			t.Fatalf("load %s snapshot: instances=%d err=%v", label, len(instances), err)
		}
		return instances[0]
	}
	delayed := loadDetached("delayed reload")
	detachedCompletion := loadDetached("detached completion")
	h.instances = []*session.Instance{canonical}
	h.instanceByID = map[string]*session.Instance{canonical.ID: canonical}
	h.groupTree = session.NewGroupTree(h.instances)
	h.search.SetItems(h.instances)
	h.rebuildFlatItems()
	h.cursor = h.flatItemIndexByID(canonical.ID)
	token := h.beginSessionTransitionFor(canonical)
	winner := statedb.RuntimeState{
		InstanceID: canonical.ID, Generation: 1, StatusRevision: 1,
		TmuxSession: "winner-g1", TmuxSocketName: "test", Status: "running",
	}
	if err := h.storage.GetDB().CommitRuntimeTransitionWithBindingPlan(
		0, canonical.PersistenceIncarnation(), winner, nil); err != nil {
		t.Fatal(err)
	}
	canonical.ApplyRuntimeState(winner)
	canonical.RuntimeBindings = map[string]statedb.RuntimeBinding{
		"codex": {InstanceID: canonical.ID, Kind: "codex", Generation: 1, Revision: 1, Value: "winner-binding"},
	}

	delayed.Title = "metadata-from-reload"
	delayed.RuntimeBindings = map[string]statedb.RuntimeBinding{
		"codex": {InstanceID: canonical.ID, Kind: "codex", Generation: 0, Revision: 9, Value: "stale-binding"},
	}
	reloadVersion := h.beginReload()
	h.Update(loadSessionsMsg{instances: []*session.Instance{delayed}, reloadVersion: reloadVersion})

	detachedCompletion.Title = "completion-copy"
	h.Update(sessionCreatedMsg{
		instance: detachedCompletion, sessionID: canonical.ID, token: token,
		runtime: winner, seeded: true,
	})
	h.saveInstances()

	group := h.groupTree.Groups["work"]
	flatIndex := h.flatItemIndexByID(canonical.ID)
	refs := []*session.Instance{
		h.instances[0], h.instanceByID[canonical.ID], group.Sessions[0],
		h.search.allItems[0], h.search.results[0], h.flatItems[flatIndex].Session, h.getSelectedSession(),
	}
	for _, got := range refs {
		if got != canonical {
			t.Fatalf("detached lifecycle replaced canonical: got=%p want=%p", got, canonical)
		}
	}
	if got := canonical.RuntimeState(); got.Generation != 1 || got.TmuxSession != "winner-g1" {
		t.Fatalf("runtime regressed: %#v", got)
	}
	if got := canonical.RuntimeBindings["codex"]; got.Generation != 1 || got.Value != "winner-binding" {
		t.Fatalf("binding regressed: %#v", got)
	}
	if durable, found, err := h.storage.GetDB().ReadRuntimeState(canonical.ID); err != nil || !found || durable.Generation != 1 {
		t.Fatalf("durable state=%#v found=%v err=%v", durable, found, err)
	}
}

func TestRuntimeLifecycle_PartialResultKeepsEqualGenerationWinner(t *testing.T) {
	inst := &session.Instance{ID: "one", Status: session.StatusIdle}
	winner := statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 4, StatusRevision: 2,
		TmuxSession: "winner", TmuxSocketName: "socket-winner", Status: "running",
	}
	loser := winner
	loser.TmuxSession = "loser"
	loser.TmuxSocketName = "socket-loser"
	if !inst.ApplyRuntimeState(winner) {
		t.Fatal("winner apply was rejected")
	}
	partial := &session.RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: loser, NeedsReconciliation: true,
		Err: errors.New("lost runtime CAS"),
	}
	calls := 0
	got, failure, warning := consumePhysicalRuntimeResult(
		inst, loser, partial,
		func(gotInst *session.Instance, gotErr error) error {
			calls++
			if gotInst.RuntimeState() != winner {
				t.Fatalf("loser was applied before reconciliation: %#v", gotInst.RuntimeState())
			}
			return statedb.ErrRuntimeGenerationConflict
		},
	)
	if failure != nil || calls != 1 {
		t.Fatalf("failure=%v reconcile calls=%d", failure, calls)
	}
	if warning == "" {
		t.Fatal("missing reconciliation warning")
	}
	if got != winner || inst.RuntimeState() != winner {
		t.Fatalf("partial result restored loser: returned=%#v canonical=%#v want=%#v",
			got, inst.RuntimeState(), winner)
	}
}
