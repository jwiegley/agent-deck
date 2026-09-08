package main

import (
	"os"
	"strings"
	"testing"
)

func requireCallOrder(t *testing.T, body, earlier, later string) {
	t.Helper()
	earlierAt := strings.Index(body, earlier)
	laterAt := strings.Index(body, later)
	if earlierAt < 0 || laterAt < 0 || earlierAt >= laterAt {
		t.Fatalf("expected %q before %q", earlier, later)
	}
}

func sourceFunctionBody(t *testing.T, path, name string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := extractFuncBody(string(raw), name)
	if body == "" {
		t.Fatalf("could not extract %s from %s", name, path)
	}
	return body
}

func TestRuntimeLifecycle_HandleRemoveFencesAllCleanup(t *testing.T) {
	body := sourceFunctionBody(t, "main.go", "handleRemove")
	requireCallOrder(t, body, "CaptureRuntimeSelection()", "DeleteAndWaitCaptured(selection)")
	requireCallOrder(t, body, "DeleteAndWaitCaptured(selection)", "SaveGroupsOnly(groupTree)")
	requireCallOrder(t, body, "DeleteAndWaitCaptured(selection)", "backend.RemoveWorktree(")
	if strings.Contains(body, "RemoveSessionAndVerify(") || strings.Contains(body, "inst.KillAndWait(") ||
		strings.Contains(body, "RetireServiceUnit(") {
		t.Fatalf("handleRemove retains an unfenced destructive path")
	}
}

func TestRuntimeLifecycle_ParentDeletingSurfacesUseCentralServiceRetirement(t *testing.T) {
	central := sourceFunctionBody(t, "../../internal/session/runtime_delete.go", "deleteCapturedAndRetireService")
	requireCallOrder(t, central, "captureParentDeleteServiceOwnershipFn", "deleteParent()")
	requireCallOrder(t, central, "deleteParent()", "retireParentDeleteServiceUnitFn")

	for _, name := range []string{"DeleteCaptured", "DeleteCapturedWithCleanup", "DeleteAndWaitCaptured", "RemoveCaptured"} {
		body := sourceFunctionBody(t, "../../internal/session/runtime_delete.go", name)
		if !strings.Contains(body, "deleteCapturedAndRetireService") {
			t.Fatalf("%s bypasses central service-unit retirement", name)
		}
	}

	callers := []struct {
		path, name, call string
	}{
		{"session_remove_cmd.go", "handleSessionRemove", "DeleteAndWaitCaptured(selection)"},
		{"session_remove_cmd.go", "bulkRemoveSessions", "DeleteAndWaitCaptured(selections[inst.ID])"},
		{"../../internal/ui/web_mutator.go", "DeleteSession", "DeleteCaptured(selection)"},
		{"../../internal/ui/home.go", "deleteSession", "DeleteCapturedWithCleanup(selection"},
		{"../../internal/ui/home.go", "removeSession", "RemoveCaptured(selection)"},
	}
	for _, caller := range callers {
		if body := sourceFunctionBody(t, caller.path, caller.name); !strings.Contains(body, caller.call) {
			t.Fatalf("%s.%s does not route through %s", caller.path, caller.name, caller.call)
		}
	}
}

func TestRuntimeLifecycle_HandleSessionStopCapturesBeforeProbes(t *testing.T) {
	body := sourceFunctionBody(t, "session_cmd.go", "handleSessionStop")
	requireCallOrder(t, body, "CaptureRuntimeSelection()", "inst.Exists()")
	requireCallOrder(t, body, "CaptureRuntimeSelection()", "SyncSessionIDsFromTmux()")
	requireCallOrder(t, body, "KillCaptured(selection)", "drainGroupQueue(")
	requireCallOrder(t, body, "KillCaptured(selection)", "saveSessionData(")
	if strings.Contains(body, "inst.Kill()") {
		t.Fatalf("handleSessionStop retains an unfenced kill")
	}
}

func TestRuntimeLifecycle_HandleSessionAdoptRuntimeIsDryRunByDefault(t *testing.T) {
	body := sourceFunctionBody(t, "session_cmd.go", "handleSessionAdoptRuntime")
	dryRunStart := strings.Index(body, "if !*yes")
	planAt := strings.Index(body, "PlanLegacyRuntimeAdoption()")
	adoptAt := strings.Index(body, "AdoptLegacyRuntime()")
	if dryRunStart < 0 || planAt < dryRunStart || adoptAt < planAt {
		t.Fatal("adopt-runtime does not isolate planning in the default dry-run branch")
	}
	dryRun := body[dryRunStart:adoptAt]
	if !strings.Contains(dryRun, "return") || !strings.Contains(dryRun, "re-run with --yes") ||
		strings.Contains(body[adoptAt:], "PlanLegacyRuntimeAdoption()") {
		t.Fatalf("adopt-runtime dry-run does not return before mutation with explicit confirmation guidance")
	}
}

func TestRuntimeLifecycle_HandleSessionArchiveFencesMetadata(t *testing.T) {
	body := sourceFunctionBody(t, "session_cmd.go", "handleSessionArchive")
	requireCallOrder(t, body, "CaptureRuntimeSelection()", "inst.IsArchived()")
	requireCallOrder(t, body, "KillCapturedRuntime(selection)", "inst.ArchivedAt =")
	requireCallOrder(t, body, "KillCapturedRuntime(selection)", "persistArchivedCLI(storage, inst, runtime, selection.Incarnation)")
	if strings.Contains(body, "inst.Kill()") {
		t.Fatalf("handleSessionArchive retains an unfenced kill")
	}
	persist := sourceFunctionBody(t, "session_cmd.go", "persistArchivedCLI")
	if !strings.Contains(persist, "SetArchivedIfRuntime(expected, incarnation, inst.ArchivedAt)") {
		t.Fatal("CLI archive does not fence metadata with the returned runtime")
	}
}

func TestRuntimeLifecycle_UIArchiveCallersFenceMetadata(t *testing.T) {
	tui := sourceFunctionBody(t, "../../internal/ui/home.go", "archiveSession")
	requireCallOrder(t, tui, "KillCapturedRuntime(selection)", "runtime: runtime")
	web := sourceFunctionBody(t, "../../internal/ui/web_mutator.go", "ArchiveSession")
	requireCallOrder(t, web, "KillCapturedRuntime(selection)", "persistArchivedIfRuntime(runtime, selection.Incarnation, archivedAt)")
}

func TestRuntimeLifecycle_SwitchAccountFencesDiskAndAccountMutation(t *testing.T) {
	body := sourceFunctionBody(t, "../../internal/session/account_switch.go", "SwitchAccount")
	requireCallOrder(t, body, "CaptureRuntimeSelection()", "inst.Exists()")
	requireCallOrder(t, body, "CaptureRuntimeSelection()", "SyncSessionIDsFromTmux()")
	requireCallOrder(t, body, "CaptureRuntimeSelection()", "LocateConversationConfigDir(")
	for _, call := range []string{"SwitchAccountRuntime(selection", "commitSwitchAccount(", "ConsumePhysicalRuntimeResult("} {
		if !strings.Contains(body, call) {
			t.Fatalf("SwitchAccount does not route through %s", call)
		}
	}
	if strings.Contains(body, "inst.Kill()") || strings.Contains(body, "inst.Start()") {
		t.Fatal("SwitchAccount retains an unfenced lifecycle path")
	}
}
