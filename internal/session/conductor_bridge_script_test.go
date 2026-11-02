package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const bridgeV1, bridgeV2 = "# bridge v1\n", "# bridge v2\n" // bridgeV2 plays the embedded bridge

// bridgeStampOf is the stamp agent-deck leaves beside a bridge.py that holds
// content: its sha256 in lower-case hex, then a newline.
func bridgeStampOf(content string) string { return fmtHash(content) + "\n" }

// bridgeFiles is what bridge.py, its stamp, .bridge.py.previous and
// bridge.py.backup hold in a conductor directory. An empty string means the
// file does not exist.
type bridgeFiles struct{ bridge, stamp, previous, backup string }

// writeBridgeFixtures writes each non-empty entry (name -> content) into dir with writeTestFile.
func writeBridgeFixtures(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if content != "" {
			writeTestFile(t, filepath.Join(dir, name), content)
		}
	}
}

// requireBridgeFiles fails the test unless dir holds exactly the files want
// describes: nothing else, so no temporary file and no .bridge.py.previous-<n>
// copy.
func requireBridgeFiles(t *testing.T, dir string, want bridgeFiles) {
	t.Helper()
	var wantNames []string
	for name, content := range map[string]string{
		"bridge.py":        want.bridge,
		bridgeStampName:    want.stamp,
		bridgePreviousName: want.previous,
		"bridge.py.backup": want.backup,
	} {
		path := filepath.Join(dir, name)
		if content == "" {
			assertNotExist(t, path)
			continue
		}
		wantNames = append(wantNames, name)
		if got := readFileString(t, path); got != content {
			t.Errorf("%s holds %q, want %q", name, got, content)
		}
	}
	slices.Sort(wantNames)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var gotNames []string
	for _, entry := range entries {
		gotNames = append(gotNames, entry.Name())
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Errorf("entries of the conductor dir = %v, want %v", gotNames, wantNames)
	}
}

// unblockFIFOOpen opens the write end of fifo and closes it again, which
// releases a reader that is stuck opening the FIFO.
func unblockFIFOOpen(fifo string) {
	if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
		_ = w.Close()
	}
}

// installWithTimeout runs installBridgeScriptAt in a goroutine and returns its
// result. When the call has not returned after 10 seconds it runs unblock, which
// must release whatever the call is stuck on, waits up to 5 seconds more for the
// goroutine so that it does not outlive the test, and fails the test: a
// regression must fail, not hang the suite.
func installWithTimeout(t *testing.T, dir string, unblock func()) (BridgeScriptResult, error) {
	t.Helper()
	type installed struct {
		res BridgeScriptResult
		err error
	}
	done := make(chan installed, 1)
	go func() {
		res, err := installBridgeScriptAt(dir, []byte(bridgeV2))
		done <- installed{res, err}
	}()
	var got installed
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		unblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("install blocked on a FIFO stamp")
	}
	return got.res, got.err
}

func TestBridgeScript_InstallOwnership(t *testing.T) {
	const edited = "# my edits\n"
	// The mode an O_EXCL create at 0o755 leaves under this process's umask.
	ref := filepath.Join(t.TempDir(), "ref")
	if err := os.WriteFile(ref, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	refInfo, err := os.Stat(ref)
	if err != nil {
		t.Fatal(err)
	}
	umasked := refInfo.Mode().Perm()

	installed := BridgeScriptResult{Outcome: BridgeScriptInstalled}
	updated := BridgeScriptResult{Outcome: BridgeScriptUpdated}
	unchanged := BridgeScriptResult{Outcome: BridgeScriptUnchanged}
	kept := BridgeScriptResult{Outcome: BridgeScriptKept, Notice: BridgeKeptNotice}
	tests := []struct {
		name          string
		bridge, stamp string // the fixtures; empty means not created
		want          BridgeScriptResult
		after         bridgeFiles
		created       bool // this call creates bridge.py, so it gets 0o755 less the umask
	}{
		{"missing: installed", "", "", installed, bridgeFiles{bridgeV2, bridgeStampOf(bridgeV2), "", ""}, true},
		{"equal, no stamp: stamped", bridgeV2, "", unchanged, bridgeFiles{bridgeV2, bridgeStampOf(bridgeV2), "", ""}, false},
		{"equal, stale stamp: restamped", bridgeV2, bridgeStampOf(bridgeV1), unchanged, bridgeFiles{bridgeV2, bridgeStampOf(bridgeV2), "", ""}, false},
		{"equal and stamped: no exchange", bridgeV2, bridgeStampOf(bridgeV2), unchanged, bridgeFiles{bridgeV2, bridgeStampOf(bridgeV2), "", ""}, false},
		{"stamped older: updated", bridgeV1, bridgeStampOf(bridgeV1), updated, bridgeFiles{bridgeV2, bridgeStampOf(bridgeV2), bridgeV1, ""}, false},
		// hera's case: an unedited bridge from before the stamp existed looks like a hand edit.
		{"pre-fix unstamped older bridge: kept", bridgeV1, "", kept, bridgeFiles{bridgeV1, "", "", ""}, false},
		{"stamp present but not matching: kept", edited, bridgeStampOf(bridgeV1), kept, bridgeFiles{edited, bridgeStampOf(bridgeV1), "", ""}, false},
		// The stamp is compared exactly, without trimming.
		{"stamp without its newline: kept", bridgeV1, fmtHash(bridgeV1), kept, bridgeFiles{bridgeV1, fmtHash(bridgeV1), "", ""}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBridgeFixtures(t, dir, map[string]string{"bridge.py": tc.bridge, bridgeStampName: tc.stamp})
			stampPath := filepath.Join(dir, bridgeStampName)
			stampBefore, _ := os.Lstat(stampPath) // nil when the fixture has no stamp

			got, err := installBridgeScriptAt(dir, []byte(bridgeV2))
			if err != nil {
				t.Fatalf("installBridgeScriptAt: %v", err)
			}
			if got != tc.want {
				t.Errorf("result = %+v, want %+v", got, tc.want)
			}
			requireBridgeFiles(t, dir, tc.after)
			// A stamp that is already right is not rewritten.
			if tc.stamp != "" && tc.stamp == tc.after.stamp {
				if stampAfter, err := os.Lstat(stampPath); err != nil || !os.SameFile(stampBefore, stampAfter) {
					t.Error("the stamp was rewritten although it was already right")
				}
			}
			if tc.created {
				info, err := os.Lstat(filepath.Join(dir, "bridge.py"))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != umasked {
					t.Errorf("bridge.py mode = %o, want %o", info.Mode().Perm(), umasked)
				}
			}
		})
	}
}

func TestBridgeScript_SecondUpgradeKeepsOnePrevious(t *testing.T) {
	const bridgeV3 = "# bridge v3\n"
	dir := t.TempDir()
	previous := filepath.Join(dir, bridgePreviousName)
	steps := []struct {
		embedded     string
		want         BridgeScriptOutcome
		wantPrevious string // empty means .bridge.py.previous does not exist
	}{
		{bridgeV1, BridgeScriptInstalled, ""},
		{bridgeV2, BridgeScriptUpdated, bridgeV1},
		{bridgeV3, BridgeScriptUpdated, bridgeV2},
		// A rerun keeps the real previous bridge.
		{bridgeV3, BridgeScriptUnchanged, bridgeV2},
	}
	for i, step := range steps {
		got, err := installBridgeScriptAt(dir, []byte(step.embedded))
		if err != nil {
			t.Fatalf("step %d: installBridgeScriptAt: %v", i+1, err)
		}
		if want := (BridgeScriptResult{Outcome: step.want}); got != want {
			t.Errorf("step %d: result = %+v, want %+v", i+1, got, want)
		}
		if step.wantPrevious == "" {
			assertNotExist(t, previous)
		} else if held := readFileString(t, previous); held != step.wantPrevious {
			t.Errorf("step %d: %s holds %q, want %q", i+1, bridgePreviousName, held, step.wantPrevious)
		}
	}
	if leftovers := generatedArtifacts(t, dir, bridgePreviousName+"-*"); len(leftovers) != 0 {
		t.Errorf("temporary displaced copies left behind: %v", leftovers)
	}
}

func TestBridgeScript_SymlinkKeptDirectoryRefused(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "bridge.py")
		writeTestFile(t, target, bridgeV1)
		link := filepath.Join(dir, "bridge.py")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		got, err := installBridgeScriptAt(dir, []byte(bridgeV2))
		if err != nil {
			t.Fatalf("installBridgeScriptAt: %v", err)
		}
		if want := (BridgeScriptResult{Outcome: BridgeScriptKept, Notice: BridgeSymlinkNotice}); got != want {
			t.Errorf("result = %+v, want %+v", got, want)
		}
		if dest, err := os.Readlink(link); err != nil || dest != target {
			t.Errorf("bridge.py is no longer a symlink to %s: Readlink = %q, %v", target, dest, err)
		}
		if held := readFileString(t, target); held != bridgeV1 {
			t.Errorf("the link target holds %q, want %q", held, bridgeV1)
		}
		assertNotExist(t, filepath.Join(dir, bridgeStampName))
	})

	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "bridge.py"), 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := installBridgeScriptAt(dir, []byte(bridgeV2))
		const want = "refusing to replace unsafe generated-file target"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want one containing %q", err, want)
		}
		if got != (BridgeScriptResult{}) {
			t.Errorf("result = %+v, want the zero result with an error", got)
		}
		assertNotExist(t, filepath.Join(dir, bridgeStampName))
	})

	// A stamp that is a symlink is not followed, so it cannot vouch for bridge.py.
	t.Run("stamp symlink", func(t *testing.T) {
		dir := t.TempDir()
		writeBridgeFixtures(t, dir, map[string]string{"bridge.py": bridgeV1})
		target := filepath.Join(t.TempDir(), "stamp")
		writeTestFile(t, target, bridgeStampOf(bridgeV1))
		if err := os.Symlink(target, filepath.Join(dir, bridgeStampName)); err != nil {
			t.Fatal(err)
		}

		got, err := installBridgeScriptAt(dir, []byte(bridgeV2))
		if err != nil {
			t.Fatalf("installBridgeScriptAt: %v", err)
		}
		if want := (BridgeScriptResult{Outcome: BridgeScriptKept, Notice: BridgeKeptNotice}); got != want {
			t.Errorf("result = %+v, want %+v", got, want)
		}
		if held := readFileString(t, filepath.Join(dir, "bridge.py")); held != bridgeV1 {
			t.Errorf("bridge.py holds %q, want it left as it was (%q)", held, bridgeV1)
		}
	})

	// A stamp that is a FIFO is never read, and it must not block the install.
	// Each row leaves the FIFO in a state that a read, or an open that waits
	// for a writer, would trip over.
	fifoStamps := []struct {
		name string
		// prepare leaves the FIFO in the row's state and returns what releases
		// an install that is stuck on it.
		prepare func(t *testing.T, fifo string) (unblock func())
	}{
		{
			// Opening it for reading would wait for a writer that never comes.
			name: "stamp FIFO",
			prepare: func(t *testing.T, fifo string) func() {
				return func() { unblockFIFOOpen(fifo) }
			},
		},
		{
			// A live writer means a reader never sees EOF. Reading the pipe
			// through Go's poller, as on Linux (darwin does not poll FIFOs),
			// then waits for the writer to close.
			name: "stamp FIFO with a writer",
			prepare: func(t *testing.T, fifo string) func() {
				hold, err := os.OpenFile(fifo, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = hold.Close() })
				// The valid stamp for bridge.py waits in the pipe.
				if _, err := hold.WriteString(bridgeStampOf(bridgeV1)); err != nil {
					t.Fatal(err)
				}
				return func() { _ = hold.Close() }
			},
		},
		{
			// No writer is left and a reader holds the pipe open with the valid
			// stamp for bridge.py in it: a read returns the stamp, then EOF.
			name: "stamp FIFO with data",
			prepare: func(t *testing.T, fifo string) func() {
				hold, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = hold.Close() })
				w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = w.WriteString(bridgeStampOf(bridgeV1))
				if closeErr := w.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatal(err)
				}
				return func() { unblockFIFOOpen(fifo) }
			},
		},
	}
	for _, tc := range fifoStamps {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBridgeFixtures(t, dir, map[string]string{"bridge.py": bridgeV1})
			stamp := filepath.Join(dir, bridgeStampName)
			if err := syscall.Mkfifo(stamp, 0o644); err != nil {
				t.Fatal(err)
			}
			unblock := tc.prepare(t, stamp)

			got, err := installWithTimeout(t, dir, unblock)
			if err != nil {
				t.Fatalf("installBridgeScriptAt: %v", err)
			}
			if want := (BridgeScriptResult{Outcome: BridgeScriptKept, Notice: BridgeKeptNotice}); got != want {
				t.Errorf("result = %+v, want %+v", got, want)
			}
			if held := readFileString(t, filepath.Join(dir, "bridge.py")); held != bridgeV1 {
				t.Errorf("bridge.py holds %q, want it left as it was (%q)", held, bridgeV1)
			}
			if info, err := os.Lstat(stamp); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				t.Errorf("the stamp is no longer a FIFO: Lstat = %v, %v", info, err)
			}
			assertNotExist(t, filepath.Join(dir, bridgePreviousName))
		})
	}
}

// A filesystem without atomic exchange gets no fallback: a stamped bridge stays
// as it is, and the error tells the user how to take the shipped one.
func TestBridgeScript_UnsupportedExchangeKeepsBridge(t *testing.T) {
	tests := []struct {
		name       string
		failure    error
		wantRemedy bool // the error tells the user to move bridge.py aside
	}{
		{"EINVAL", syscall.EINVAL, true},   // renameat2 on NFS
		{"ENOTSUP", syscall.ENOTSUP, true}, // renamex_np without RENAME_SWAP
		{"other failure", errors.New("injected exchange failure"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBridgeFixtures(t, dir, map[string]string{"bridge.py": bridgeV1, bridgeStampName: bridgeStampOf(bridgeV1)})
			original := exchangeGeneratedFiles
			exchangeGeneratedFiles = func(string, string) error { return tc.failure }
			t.Cleanup(func() { exchangeGeneratedFiles = original })

			got, err := installBridgeScriptAt(dir, []byte(bridgeV2))
			if err == nil {
				t.Fatalf("installBridgeScriptAt = %+v, nil; want an error", got)
			}
			if got != (BridgeScriptResult{}) {
				t.Errorf("result = %+v, want the zero result with an error", got)
			}
			if !errors.Is(err, tc.failure) {
				t.Errorf("error = %v, want one wrapping %v", err, tc.failure)
			}
			if has := strings.Contains(err.Error(), "move bridge.py aside"); has != tc.wantRemedy {
				t.Errorf("error = %q: names the move-aside remedy = %v, want %v", err, has, tc.wantRemedy)
			}
			requireBridgeFiles(t, dir, bridgeFiles{bridge: bridgeV1, stamp: bridgeStampOf(bridgeV1)})
		})
	}
}

func TestBridgeScript_ConcurrentEditWritesNoStamp(t *testing.T) {
	dir := t.TempDir()
	writeBridgeFixtures(t, dir, map[string]string{"bridge.py": bridgeV1, bridgeStampName: bridgeStampOf(bridgeV1)})
	original := exchangeGeneratedFiles
	var recovery string
	exchanges := 0
	exchangeGeneratedFiles = func(from, to string) error {
		exchanges++
		recovery = from
		// An editor saves bridge.py just before the exchange displaces it.
		if err := os.WriteFile(to, []byte("# edit A\n"), 0o644); err != nil {
			return err
		}
		return exchangeGeneratedFile(from, to)
	}
	t.Cleanup(func() { exchangeGeneratedFiles = original })

	got, err := installBridgeScriptAt(dir, []byte(bridgeV2))
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want exactly 1 (no rollback exchange)", exchanges)
	}
	if err == nil || !strings.Contains(err.Error(), recovery) {
		t.Fatalf("error = %v, want one naming the retained copy %q", err, recovery)
	}
	if strings.Contains(err.Error(), "move bridge.py aside") {
		t.Errorf("error = %q: a concurrent edit is not fixed by moving bridge.py aside", err)
	}
	if got != (BridgeScriptResult{}) {
		t.Errorf("result = %+v, want the zero result with an error", got)
	}
	if held := readFileString(t, filepath.Join(dir, "bridge.py")); held != bridgeV2 {
		t.Errorf("bridge.py holds %q, want %q", held, bridgeV2)
	}
	if held := readFileString(t, recovery); held != "# edit A\n" {
		t.Errorf("retained copy holds %q, want %q", held, "# edit A\n")
	}
	// The stamp is written only after a clean replacement.
	if held := readFileString(t, filepath.Join(dir, bridgeStampName)); held != bridgeStampOf(bridgeV1) {
		t.Errorf("stamp holds %q, want it untouched (%q)", held, bridgeStampOf(bridgeV1))
	}
	assertNotExist(t, filepath.Join(dir, bridgePreviousName))
}

func TestBridgeScript_StampWriteFailure(t *testing.T) {
	dir := t.TempDir()
	// A directory where the stamp goes makes the rename that publishes it fail.
	if err := os.Mkdir(filepath.Join(dir, bridgeStampName), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := installBridgeScriptAt(dir, []byte(bridgeV2))
	want := "writing " + bridgeStampName
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one containing %q", err, want)
	}
	if got != (BridgeScriptResult{}) {
		t.Errorf("result = %+v, want the zero result with an error", got)
	}
	// The stamp comes after the bridge, so the next run finds bridge.py equal
	// to the embedded one and stamps it.
	if held := readFileString(t, filepath.Join(dir, "bridge.py")); held != bridgeV2 {
		t.Errorf("bridge.py holds %q, want %q", held, bridgeV2)
	}
}

func TestBridgeScript_KeptErrorAndWrappers(t *testing.T) {
	for _, tc := range []struct{ notice, want string }{
		{BridgeKeptNotice, "bridge.py has local changes; left unchanged"},
		{BridgeSymlinkNotice, "bridge.py is a symlink; left unchanged"},
	} {
		kept := &BridgeKeptError{Notice: tc.notice}
		if got := kept.Error(); got != tc.want {
			t.Errorf("Error() for notice %q = %q, want %q", tc.notice, got, tc.want)
		}
		if got := kept.BridgeKeptNotice(); got != tc.notice {
			t.Errorf("BridgeKeptNotice() = %q, want %q", got, tc.notice)
		}
	}

	// TestMain's HOME is shared by the whole package, so a bridge written
	// through ConductorDir would leak into other tests.
	setupSessionXDGPathEnv(t)
	dir, err := ConductorDir()
	if err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(dir, "bridge.py")

	res, err := InstallBridgeScriptOutcome()
	if err != nil {
		t.Fatalf("InstallBridgeScriptOutcome: %v", err)
	}
	if want := (BridgeScriptResult{Outcome: BridgeScriptInstalled}); res != want {
		t.Errorf("InstallBridgeScriptOutcome = %+v, want %+v", res, want)
	}
	if held := readFileString(t, bridge); held != conductorBridgePy {
		t.Errorf("bridge.py holds %d bytes, want the embedded bridge (%d bytes)", len(held), len(conductorBridgePy))
	}
	if err := InstallBridgeScript(); err != nil {
		t.Fatalf("InstallBridgeScript over the embedded bridge: %v", err)
	}

	writeTestFile(t, bridge, "# my bridge\n")
	err = InstallBridgeScript()
	if _, ok := err.(*BridgeKeptError); !ok {
		t.Fatalf("InstallBridgeScript over a customized bridge = %T (%v), want *BridgeKeptError itself", err, err)
	}
	// update.UpdateBridgePy wraps whatever the installer returns.
	var kept *BridgeKeptError
	if !errors.As(fmt.Errorf("failed to install bridge.py: %w", err), &kept) {
		t.Fatal("errors.As found no *BridgeKeptError in the wrapped error")
	}
	if got := kept.BridgeKeptNotice(); got != BridgeKeptNotice {
		t.Errorf("BridgeKeptNotice() = %q, want %q", got, BridgeKeptNotice)
	}
	if held := readFileString(t, bridge); held != "# my bridge\n" {
		t.Errorf("bridge.py holds %q, want it left as the user's", held)
	}
}

func TestBridgeScript_RemoveOnlyOwnedBridge(t *testing.T) {
	tests := []struct {
		name        string
		bridge      string // what bridge.py holds, or what its link points at; empty means no bridge.py
		link        bool   // bridge.py is a symlink to a file in another t.TempDir()
		stamp       string // empty means no stamp
		wantRemoved bool
	}{
		{"equal to embedded, stale stamp", bridgeV2, false, bridgeStampOf(bridgeV1), true},
		{"stamped older", bridgeV1, false, bridgeStampOf(bridgeV1), true},
		{"customized", "# my bridge\n", false, bridgeStampOf(bridgeV1), false},
		{"pre-fix unstamped older bridge", bridgeV1, false, "", false},
		{"symlink to the embedded bytes", bridgeV2, true, bridgeStampOf(bridgeV2), false},
		{"missing", "", false, bridgeStampOf(bridgeV1), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{bridgeStampName: tc.stamp, bridgePreviousName: bridgeV1, "bridge.py.backup": "# backup\n"}
			bridge := filepath.Join(dir, "bridge.py")
			if tc.link {
				target := filepath.Join(t.TempDir(), "bridge.py")
				writeTestFile(t, target, tc.bridge)
				if err := os.Symlink(target, bridge); err != nil {
					t.Fatal(err)
				}
			} else {
				files["bridge.py"] = tc.bridge
			}
			writeBridgeFixtures(t, dir, files)

			removed, err := removeBridgeScriptAt(dir, []byte(bridgeV2))
			if err != nil {
				t.Fatalf("removeBridgeScriptAt: %v", err)
			}
			if removed != tc.wantRemoved {
				t.Errorf("removed = %v, want %v", removed, tc.wantRemoved)
			}
			// bridge.py.backup is never the installer's to delete.
			want := bridgeFiles{bridge: tc.bridge, stamp: tc.stamp, previous: bridgeV1, backup: "# backup\n"}
			if tc.wantRemoved {
				want = bridgeFiles{backup: "# backup\n"}
			}
			requireBridgeFiles(t, dir, want)
			if tc.link {
				if info, err := os.Lstat(bridge); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("bridge.py is no longer a symlink: Lstat = %v, %v", info, err)
				}
			}
		})
	}

	t.Run("RemoveBridgeScript resolves ConductorDir", func(t *testing.T) {
		setupSessionXDGPathEnv(t)
		if _, err := InstallBridgeScriptOutcome(); err != nil {
			t.Fatalf("InstallBridgeScriptOutcome: %v", err)
		}
		dir, err := ConductorDir()
		if err != nil {
			t.Fatal(err)
		}

		removed, err := RemoveBridgeScript()
		if err != nil || !removed {
			t.Fatalf("RemoveBridgeScript = %v, %v; want true, nil", removed, err)
		}
		assertNotExist(t, filepath.Join(dir, "bridge.py"))
		assertNotExist(t, filepath.Join(dir, bridgeStampName))
	})
}
