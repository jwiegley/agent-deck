package main

import (
	"os"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

type fakeConductorRuntimeTarget struct {
	selection session.RuntimeSelection
	killed    []session.RuntimeSelection
	deleted   []session.RuntimeSelection
}

func TestRuntimeLifecycle_ConductorRemovalFencesDirectoryTeardown(t *testing.T) {
	raw, err := os.ReadFile("conductor_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	body := extractFuncBody(string(raw), "handleConductorTeardown")
	actionAt := strings.Index(body, "applyConductorRuntimeAction(")
	teardownAt := strings.Index(body, "session.TeardownConductor(")
	if actionAt < 0 || teardownAt < 0 || actionAt >= teardownAt {
		t.Fatalf("conditional runtime action must precede directory teardown")
	}
	if strings.Contains(body, "RemoveSessionAndVerify(") {
		t.Fatalf("conductor teardown must not issue an unconditional second delete")
	}
}

func (f *fakeConductorRuntimeTarget) CaptureRuntimeSelection() session.RuntimeSelection {
	return f.selection
}

func (f *fakeConductorRuntimeTarget) KillAndWaitCaptured(selection session.RuntimeSelection) error {
	f.killed = append(f.killed, selection)
	return nil
}

func (f *fakeConductorRuntimeTarget) DeleteAndWaitCaptured(selection session.RuntimeSelection) error {
	f.deleted = append(f.deleted, selection)
	return nil
}

func TestRuntimeLifecycle_ConductorActionUsesCapturedSelection(t *testing.T) {
	selection := session.RuntimeSelection{State: statedb.RuntimeState{
		InstanceID: "conductor", Generation: 7, StatusRevision: 3,
		TmuxSession: "conductor-g7", TmuxSocketName: "isolated", Status: "running",
	}}

	stop := &fakeConductorRuntimeTarget{selection: selection}
	if err := applyConductorRuntimeAction(stop, stop.CaptureRuntimeSelection(), false); err != nil {
		t.Fatal(err)
	}
	if len(stop.killed) != 1 || stop.killed[0] != selection || len(stop.deleted) != 0 {
		t.Fatalf("stop action = killed %#v, deleted %#v", stop.killed, stop.deleted)
	}

	remove := &fakeConductorRuntimeTarget{selection: selection}
	if err := applyConductorRuntimeAction(remove, remove.CaptureRuntimeSelection(), true); err != nil {
		t.Fatal(err)
	}
	if len(remove.deleted) != 1 || remove.deleted[0] != selection || len(remove.killed) != 0 {
		t.Fatalf("remove action = killed %#v, deleted %#v", remove.killed, remove.deleted)
	}
}
