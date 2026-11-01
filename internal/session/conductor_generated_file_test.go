package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/testutil"
)

// acceptAnyGenerated is a Replaceable predicate that accepts any bytes.
func acceptAnyGenerated([]byte) bool { return true }

// onlyOldGenerated is a Replaceable predicate that accepts only the bytes "old".
func onlyOldGenerated(existing []byte) bool { return string(existing) == "old" }

// generatedArtifacts lists the entries of dir that match pattern. In these
// tests ".managed.*" matches every temporary (.managed.previous-*) and fixed
// (.managed.previous) copy of dir/managed.
func generatedArtifacts(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// requireGeneratedArtifacts fails the test unless the entries of dir that match
// pattern are exactly want (none when want is empty).
func requireGeneratedArtifacts(t *testing.T, dir, pattern string, want ...string) {
	t.Helper()
	got := generatedArtifacts(t, dir, pattern)
	if len(got) != len(want) {
		t.Fatalf("entries matching %s = %v, want %v", pattern, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries matching %s = %v, want %v", pattern, got, want)
		}
	}
}

func TestReplaceGeneratedFileCreatesOnlyWhenAbsent(t *testing.T) {
	// The mode an O_EXCL create at 0o666 leaves under this process's umask.
	ref := filepath.Join(t.TempDir(), "ref")
	if err := os.WriteFile(ref, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	refInfo, err := os.Stat(ref)
	if err != nil {
		t.Fatal(err)
	}
	umasked := refInfo.Mode().Perm()

	tests := []struct {
		name         string
		racer        string // what a racing process writes first; empty means no race
		wantOutcome  generatedFileOutcome
		wantContent  string
		wantMode     os.FileMode
		wantPrevious string // what the one retained .managed.previous-* copy holds; empty means no artifact at all
	}{
		{"absent", "", generatedFileCreated, "new", umasked, ""},
		{"racer wrote the current bytes", "new", generatedFileUnchanged, "new", 0o600, ""},
		{"racer wrote other bytes", "racer", generatedFileKept, "racer", 0o600, ""},
		// The racer's file is migrated like any older generation, and the
		// replacement keeps its mode bits, not Perm.
		{"racer wrote an older generation", "old", generatedFileReplaced, "new", 0o600, "old"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "managed")
			if tc.racer != "" {
				original := createGeneratedFile
				createGeneratedFile = func(target string, content []byte, perm os.FileMode) (bool, error) {
					if err := os.WriteFile(target, []byte(tc.racer), 0o600); err != nil {
						return false, err
					}
					return createFileIfAbsent(target, content, perm)
				}
				t.Cleanup(func() { createGeneratedFile = original })
			}

			outcome, err := replaceGeneratedFile(generatedFileSpec{Path: path, Current: []byte("new"), Perm: 0o666, Replaceable: onlyOldGenerated})
			if err != nil {
				t.Fatalf("replaceGeneratedFile: %v", err)
			}
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %d, want %d", outcome, tc.wantOutcome)
			}
			if got := readFile(t, path); got != tc.wantContent {
				t.Errorf("content = %q, want %q", got, tc.wantContent)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != tc.wantMode {
				t.Errorf("mode = %o, want %o", info.Mode().Perm(), tc.wantMode)
			}
			if tc.wantPrevious == "" {
				requireGeneratedArtifacts(t, dir, ".managed.*")
			} else {
				// No DisplacedTo: the displaced copy keeps its random temporary name.
				retained := generatedArtifacts(t, dir, ".managed.previous-*")
				if len(retained) != 1 {
					t.Fatalf("retained copies = %v, want exactly one", retained)
				}
				requireGeneratedArtifacts(t, dir, ".managed.*", retained[0])
				if got := readFile(t, retained[0]); got != tc.wantPrevious {
					t.Errorf("retained copy holds %q, want %q", got, tc.wantPrevious)
				}
			}
		})
	}
}

func TestReplaceGeneratedFileRemovesPartialCreate(t *testing.T) {
	// The create seam leaves written at the path (nothing when it is empty),
	// reports created, and fails with failure. With readOnlyDir it first makes
	// the directory unwritable.
	tests := []struct {
		name        string
		written     string
		created     bool
		failure     string
		readOnlyDir bool
		wantErr     string // also expected in the error; empty means the create error alone
		wantErrIs   error  // also expected in the error chain; nil means none
		wantPresent bool   // the path still holds written afterwards
	}{
		// A partial file left behind would read as the user's on every later run.
		{name: "file created by this call", written: "ne", created: true, failure: "disk full"},
		// A file this call did not create is not the writer's to remove.
		{name: "file not created by this call", written: "racer", failure: "permission denied", wantPresent: true},
		// When the cleanup fails too, the error says the partial file survives
		// and keeps the removal's own error in its chain.
		{name: "partial file cannot be removed", written: "ne", created: true, failure: "disk full", readOnlyDir: true, wantErr: "removing partial generated file", wantErrIs: os.ErrPermission, wantPresent: true},
		// A partial file that is already gone is nothing to report.
		{name: "partial file already gone", created: true, failure: "disk full"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.readOnlyDir {
				testutil.SkipIfRoot(t, "root ignores directory permissions and removes the file")
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "managed")
			if tc.readOnlyDir {
				// Cleanups run last in, first out, so the directory is writable
				// again before t.TempDir's own cleanup removes it.
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			}
			seamErr := errors.New(tc.failure)
			original := createGeneratedFile
			createGeneratedFile = func(target string, _ []byte, _ os.FileMode) (bool, error) {
				if tc.written != "" {
					if err := os.WriteFile(target, []byte(tc.written), 0o600); err != nil {
						return false, err
					}
				}
				if tc.readOnlyDir {
					if err := os.Chmod(dir, 0o555); err != nil {
						return false, err
					}
				}
				return tc.created, seamErr
			}
			t.Cleanup(func() { createGeneratedFile = original })

			outcome, err := replaceGeneratedFile(generatedFileSpec{Path: path, Current: []byte("new"), Perm: 0o666, Replaceable: onlyOldGenerated})
			if outcome != 0 {
				t.Errorf("outcome = %d, want 0 with an error", outcome)
			}
			if err == nil || !strings.Contains(err.Error(), tc.failure) {
				t.Fatalf("error = %v, want one containing %q", err, tc.failure)
			}
			if !errors.Is(err, seamErr) {
				t.Errorf("error = %v, want one wrapping the create error", err)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want one also containing %q", err, tc.wantErr)
			}
			if tc.wantErr == "" && err.Error() != tc.failure {
				t.Errorf("error = %q, want the create error alone (%q)", err, tc.failure)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Errorf("error = %v, want one wrapping %v", err, tc.wantErrIs)
			}
			if tc.wantPresent {
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != tc.written {
					t.Errorf("the path holds %q (read error: %v), want %q left alone", got, readErr, tc.written)
				}
			} else if _, lstatErr := os.Lstat(path); !os.IsNotExist(lstatErr) {
				t.Errorf("partial file outlived the failed create: Lstat error = %v", lstatErr)
			}
			requireGeneratedArtifacts(t, dir, ".managed.*")
		})
	}
}

func TestReplaceGeneratedFileLeavesNonReplaceableTargetsAlone(t *testing.T) {
	tests := []struct {
		name        string
		fixture     func(path, dir string) error // makes dir/managed
		replaceable func([]byte) bool
		wantOutcome generatedFileOutcome
		wantErr     string // empty means no error
	}{
		{
			name:        "current bytes although the predicate accepts them",
			fixture:     func(path, _ string) error { return os.WriteFile(path, []byte("new"), 0o600) },
			replaceable: acceptAnyGenerated,
			wantOutcome: generatedFileUnchanged,
		},
		{
			name:        "predicate refuses",
			fixture:     func(path, _ string) error { return os.WriteFile(path, []byte("user edited"), 0o600) },
			replaceable: onlyOldGenerated,
			wantOutcome: generatedFileKept,
		},
		{
			name:        "symlink to replaceable bytes",
			fixture:     func(path, dir string) error { return os.Symlink(filepath.Join(dir, "target"), path) },
			replaceable: acceptAnyGenerated,
			wantOutcome: generatedFileSymlink,
		},
		{
			name:        "dangling symlink",
			fixture:     func(path, dir string) error { return os.Symlink(filepath.Join(dir, "missing"), path) },
			replaceable: acceptAnyGenerated,
			wantOutcome: generatedFileSymlink,
		},
		{
			name:        "directory",
			fixture:     func(path, _ string) error { return os.Mkdir(path, 0o755) },
			replaceable: acceptAnyGenerated,
			wantErr:     "refusing to replace unsafe generated-file target",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "managed")
			if err := tc.fixture(path, dir); err != nil {
				t.Fatal(err)
			}
			infoBefore, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			// A dangling symlink and a directory have no bytes to read: both
			// reads must fail the same way before and after.
			bytesBefore, readErrBefore := os.ReadFile(path)

			outcome, err := replaceGeneratedFile(generatedFileSpec{
				Path:        path,
				Current:     []byte("new"),
				Perm:        0o644,
				Replaceable: tc.replaceable,
				DisplacedTo: filepath.Join(dir, ".managed.previous"),
			})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("replaceGeneratedFile: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %d, want %d", outcome, tc.wantOutcome)
			}

			infoAfter, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(infoBefore, infoAfter) {
				t.Error("the path was replaced by another file")
			}
			bytesAfter, readErrAfter := os.ReadFile(path)
			if !bytes.Equal(bytesBefore, bytesAfter) || (readErrBefore == nil) != (readErrAfter == nil) {
				t.Errorf("content changed: before %q (%v), after %q (%v)", bytesBefore, readErrBefore, bytesAfter, readErrAfter)
			}
			if got := readFile(t, target); got != "old" {
				t.Errorf("dir/target = %q, want it untouched (%q)", got, "old")
			}
			requireGeneratedArtifacts(t, dir, ".managed.*")
		})
	}
}

func TestReplaceGeneratedFileKeepsOneDisplacedCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed")
	displaced := filepath.Join(dir, ".managed.previous")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	specFor := func(current string) generatedFileSpec {
		return generatedFileSpec{Path: path, Current: []byte(current), Perm: 0o644, Replaceable: acceptAnyGenerated, DisplacedTo: displaced}
	}

	for _, step := range []struct{ current, wantDisplaced string }{{"v2", "v1"}, {"v3", "v2"}} {
		outcome, err := replaceGeneratedFile(specFor(step.current))
		if err != nil || outcome != generatedFileReplaced {
			t.Fatalf("replacing with %q: outcome = %d, error = %v, want %d and nil", step.current, outcome, err, generatedFileReplaced)
		}
		if got := readFile(t, path); got != step.current {
			t.Errorf("after %q the path holds %q", step.current, got)
		}
		if got := readFile(t, displaced); got != step.wantDisplaced {
			t.Errorf("after %q the displaced copy holds %q, want %q", step.current, got, step.wantDisplaced)
		}
		requireGeneratedArtifacts(t, dir, ".managed.*", displaced)
	}

	// A directory in the way of the fixed name makes the final move fail. The
	// new bytes are already published, and the verified displaced copy stays
	// where the exchange left it, under its temporary name.
	if err := os.Remove(displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(displaced, 0o755); err != nil {
		t.Fatal(err)
	}
	outcome, err := replaceGeneratedFile(specFor("v4"))
	temporary := generatedArtifacts(t, dir, ".managed.previous-*")
	if len(temporary) != 1 {
		t.Fatalf("temporary copies = %v, want exactly one", temporary)
	}
	if outcome != 0 {
		t.Errorf("outcome = %d, want 0 with an error", outcome)
	}
	if err == nil || !strings.Contains(err.Error(), temporary[0]) {
		t.Fatalf("error = %v, want one naming the retained copy %q", err, temporary[0])
	}
	if got := readFile(t, temporary[0]); got != "v3" {
		t.Errorf("retained copy holds %q, want %q", got, "v3")
	}
	if got := readFile(t, path); got != "v4" {
		t.Errorf("the path holds %q, want %q", got, "v4")
	}
}

func TestReplaceGeneratedFileConcurrentEditKeepsTemporaryCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed")
	displaced := filepath.Join(dir, ".managed.previous")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := exchangeGeneratedFiles
	var recovery string
	calls := 0
	exchangeGeneratedFiles = func(from, to string) error {
		calls++
		if calls == 1 {
			recovery = from
			if err := os.WriteFile(to, []byte("edit A"), 0o600); err != nil {
				return err
			}
		}
		return exchangeGeneratedFile(from, to)
	}
	t.Cleanup(func() { exchangeGeneratedFiles = original })

	outcome, err := replaceGeneratedFile(generatedFileSpec{Path: path, Current: []byte("new"), Perm: 0o600, Replaceable: onlyOldGenerated, DisplacedTo: displaced})
	if calls != 1 {
		t.Fatalf("exchanges = %d, want exactly 1 (no rollback exchange)", calls)
	}
	if outcome != 0 {
		t.Errorf("outcome = %d, want 0 with an error", outcome)
	}
	if err == nil || !strings.Contains(err.Error(), recovery) {
		t.Fatalf("error = %v, want one naming the retained copy %q", err, recovery)
	}
	// The edited copy is not the generated file's to move: the fixed name stays free.
	if _, err := os.Lstat(displaced); !os.IsNotExist(err) {
		t.Errorf("the fixed displaced name was written: Lstat error = %v", err)
	}
	requireGeneratedArtifacts(t, dir, ".managed.*", recovery)
	requireGeneratedArtifacts(t, dir, ".managed.previous-*", recovery)
	if got := readFile(t, recovery); got != "edit A" {
		t.Errorf("retained copy holds %q, want %q", got, "edit A")
	}
	if got := readFile(t, path); got != "new" {
		t.Errorf("the path holds %q, want %q", got, "new")
	}
}

func TestReplaceGeneratedFileDisplacedToKeepsOpenEditorInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed")
	displaced := filepath.Join(dir, ".managed.previous")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	editor, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer editor.Close()

	outcome, err := replaceGeneratedFile(generatedFileSpec{Path: path, Current: []byte("new"), Perm: 0o600, Replaceable: onlyOldGenerated, DisplacedTo: displaced})
	if err != nil || outcome != generatedFileReplaced {
		t.Fatalf("outcome = %d, error = %v, want %d and nil", outcome, err, generatedFileReplaced)
	}
	if err := editor.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := editor.WriteAt([]byte("late editor contents"), 0); err != nil {
		t.Fatal(err)
	}
	if err := editor.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, displaced); got != "late editor contents" {
		t.Errorf("open editor writes lost: the displaced copy holds %q", got)
	}
	if got := readFile(t, path); got != "new" {
		t.Errorf("the path holds %q, want %q", got, "new")
	}
	requireGeneratedArtifacts(t, dir, ".managed.*", displaced)
}
