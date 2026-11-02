package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/asheshgoplani/agent-deck/internal/update"
)

// bridge.py is written from the bridge embedded in the binary, but people edit
// the file they run: a hook for their own channel, a patched timeout. So
// agent-deck replaces bridge.py only while it can show that the file is still
// the one it wrote. Each bridge it writes is recorded in a stamp beside it, the
// sha256 of the bytes written, and a later install replaces bridge.py only
// while the file still hashes to that stamp. A bridge without a matching stamp
// is the user's, and that includes every bridge written before the stamp
// existed. The one exception is a regular file byte-identical to the embedded
// bridge: install adopts it by stamping it, and removal deletes it.

const (
	// bridgeStampName is the hidden file beside bridge.py that holds the
	// bridgeStamp of the last bridge agent-deck wrote.
	bridgeStampName = ".bridge.py.sha256"
	// bridgePreviousName is the hidden file beside bridge.py that holds the
	// bridge the last replacement displaced. It keeps one generation: the next
	// replacement overwrites it.
	bridgePreviousName = ".bridge.py.previous"
)

// BridgeScriptOutcome says what installing bridge.py did.
type BridgeScriptOutcome string

const (
	// BridgeScriptInstalled: there was no bridge.py and the embedded one was
	// written.
	BridgeScriptInstalled BridgeScriptOutcome = "installed"
	// BridgeScriptUpdated: bridge.py was a bridge agent-deck wrote earlier and
	// now holds the embedded one. The earlier one is kept as
	// .bridge.py.previous.
	BridgeScriptUpdated BridgeScriptOutcome = "updated"
	// BridgeScriptUnchanged: bridge.py already held the embedded bytes.
	BridgeScriptUnchanged BridgeScriptOutcome = "unchanged"
	// BridgeScriptKept: bridge.py is the user's (edited, written before the
	// stamp existed, or a symlink) and was left alone.
	BridgeScriptKept BridgeScriptOutcome = "kept"
)

// BridgeScriptResult is what installing bridge.py did and, when it kept the
// user's bridge, the line to show about it.
type BridgeScriptResult struct {
	Outcome BridgeScriptOutcome
	// Notice is set only when Outcome is BridgeScriptKept: BridgeKeptNotice or
	// BridgeSymlinkNotice.
	Notice string
}

// BridgeKeptNotice is the line shown when bridge.py was left unchanged because
// it does not match its stamp: the user edited it, or it was written before the
// stamp existed.
const BridgeKeptNotice = "[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run 'agent-deck conductor setup <name>'."

// BridgeSymlinkNotice is the line shown when bridge.py was left unchanged
// because it is a symlink.
const BridgeSymlinkNotice = "[skip] bridge.py is a symlink; left unchanged."

// BridgeKeptError is the error InstallBridgeScript returns when it left
// bridge.py unchanged. A caller that only has an error value tells it from a
// failed write with errors.As, and a caller that cannot import this package
// matches the update.BridgeKeptNotifier it implements.
type BridgeKeptError struct {
	// Notice is BridgeKeptNotice or BridgeSymlinkNotice.
	Notice string
}

// Error names the file and what happened, without the remedy: callers print it
// inside a message of their own, as in "Error installing bridge.py: ...".
func (e *BridgeKeptError) Error() string {
	if e.Notice == BridgeSymlinkNotice {
		return "bridge.py is a symlink; left unchanged"
	}
	return "bridge.py has local changes; left unchanged"
}

// BridgeKeptNotice returns the line to show the user.
func (e *BridgeKeptError) BridgeKeptNotice() string { return e.Notice }

// The assertion is the contract: update.UpdateBridgePy finds this error through
// update.BridgeKeptNotifier, and internal/update cannot import this package to
// check it. A rename of BridgeKeptNotice on either side stops the build here.
var _ update.BridgeKeptNotifier = (*BridgeKeptError)(nil)

// bridgeStamp is the stamp for a bridge.py that holds content: its sha256 as 64
// lower-case hex digits, then a newline. Stamps are compared exactly, byte for
// byte.
func bridgeStamp(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]) + "\n"
}

// readBridgeStamp returns the bytes of dir's stamp, or "" when there is nothing
// to trust: the stamp is missing, unreadable, or not a regular file. It decides
// on the open file, not on the path, so a swap between the check and the read
// cannot matter. O_NOFOLLOW means a stamp that is a symlink is never followed,
// O_NONBLOCK means opening a FIFO cannot block, and the open descriptor must be
// a regular file, so a FIFO or other special file is never read. In deciding
// whether to replace or remove bridge.py, "" matches no bridge, so an
// unreadable stamp can only make install keep a bridge it would have updated,
// or removal leave one it would have removed.
func readBridgeStamp(dir string) string {
	f, err := os.OpenFile(filepath.Join(dir, bridgeStampName), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(data)
}

// InstallBridgeScriptOutcome writes the embedded bridge to bridge.py in the
// conductor base directory, creating the directory if needed, unless the file
// is the user's; installBridgeScriptAt says when that is. The returned result
// says what happened, and it is the zero result whenever the error is not nil.
//
// It takes no lock: MigrateConductorDir calls it while holding the conductor
// base lock, which is not reentrant. The atomic exchange and the check of the
// displaced file are what guard against a concurrent edit.
func InstallBridgeScriptOutcome() (BridgeScriptResult, error) {
	dir, err := ConductorDir()
	if err != nil {
		return BridgeScriptResult{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return BridgeScriptResult{}, fmt.Errorf("failed to create conductor dir: %w", err)
	}
	return installBridgeScriptAt(dir, []byte(conductorBridgePy))
}

// InstallBridgeScript is InstallBridgeScriptOutcome for callers that only need
// an error: main.go injects it into the update package. A bridge that was kept
// is returned as a *BridgeKeptError, not as nil, so a caller does not report an
// install that did not happen.
func InstallBridgeScript() error {
	res, err := InstallBridgeScriptOutcome()
	if err != nil {
		return err
	}
	if res.Outcome == BridgeScriptKept {
		return &BridgeKeptError{Notice: res.Notice}
	}
	return nil
}

// installBridgeScriptAt brings dir/bridge.py to embedded and reports what it
// did:
//
//   - missing: it is written, then stamped (installed);
//   - already equal to embedded: it is left alone, and stamped unless the stamp
//     is already right (unchanged);
//   - a regular file that hashes to the stamp: it is replaced by an atomic
//     exchange and restamped, and the replaced file is kept as
//     .bridge.py.previous (updated);
//   - any other regular file, including one with no stamp: left alone (kept);
//   - a symlink, dangling or not: left alone (kept, with the symlink notice);
//   - anything else, such as a directory: an error.
//
// The stamp is written after bridge.py, so a crash between the two heals on the
// next run of the same agent-deck version, when the file equals embedded. A
// newer version finds a file that differs from its embedded bridge and has no
// stamp that matches it, and keeps it. A replacement is stamped only once it is
// clean: a concurrent edit found in the displaced file is an error, and
// the stamp stays as it was. Without atomic exchange (an NFS home) a stamped
// bridge is not replaced, and the error says to move bridge.py aside.
func installBridgeScriptAt(dir string, embedded []byte) (BridgeScriptResult, error) {
	stamp := readBridgeStamp(dir)
	outcome, err := replaceGeneratedFile(generatedFileSpec{
		Path:    filepath.Join(dir, "bridge.py"),
		Current: embedded,
		Perm:    0o755,
		Replaceable: func(existing []byte) bool {
			return stamp == bridgeStamp(existing)
		},
		DisplacedTo: filepath.Join(dir, bridgePreviousName),
	})
	if err != nil {
		// ENOTSUP, EOPNOTSUPP and ENOSYS are ErrUnsupported; EINVAL is what
		// renameat2 returns on NFS.
		if errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EINVAL) {
			return BridgeScriptResult{}, fmt.Errorf("replacing bridge.py: %w; to take the shipped bridge here, move bridge.py aside and re-run", err)
		}
		return BridgeScriptResult{}, fmt.Errorf("writing bridge.py: %w", err)
	}

	var result BridgeScriptResult
	writeStamp := true
	switch outcome {
	case generatedFileCreated:
		result.Outcome = BridgeScriptInstalled
	case generatedFileReplaced:
		result.Outcome = BridgeScriptUpdated
	case generatedFileUnchanged:
		result.Outcome = BridgeScriptUnchanged
		writeStamp = stamp != bridgeStamp(embedded)
	case generatedFileKept:
		return BridgeScriptResult{Outcome: BridgeScriptKept, Notice: BridgeKeptNotice}, nil
	case generatedFileSymlink:
		return BridgeScriptResult{Outcome: BridgeScriptKept, Notice: BridgeSymlinkNotice}, nil
	default:
		return BridgeScriptResult{}, fmt.Errorf("writing bridge.py: unexpected outcome %d", outcome)
	}
	if writeStamp {
		if err := atomicWriteFile(filepath.Join(dir, bridgeStampName), []byte(bridgeStamp(embedded)), 0o644); err != nil {
			return BridgeScriptResult{}, fmt.Errorf("writing %s: %w", bridgeStampName, err)
		}
	}
	return result, nil
}

// RemoveBridgeScript deletes bridge.py from the conductor base directory, with
// its stamp and .bridge.py.previous, only when agent-deck owns the bridge, and
// reports whether it did. It does not create the directory.
func RemoveBridgeScript() (removed bool, err error) {
	dir, err := ConductorDir()
	if err != nil {
		return false, err
	}
	return removeBridgeScriptAt(dir, []byte(conductorBridgePy))
}

// removeBridgeScriptAt deletes dir/bridge.py, its stamp and .bridge.py.previous
// when bridge.py is a regular file that agent-deck owns: one equal to embedded,
// or one that hashes to the stamp. It reports whether it did.
//
// Everything else is left in place; only two of those cases are errors: Lstat
// failing for a reason other than a missing file, and a regular bridge.py that
// cannot be read. A missing bridge.py leaves an orphan stamp or
// .bridge.py.previous where it is. A bridge.py that Lstat shows to be a
// symlink, a directory or a FIFO is neither read nor removed: a symlink points
// at something that is not agent-deck's. A bridge that neither equals embedded
// nor hashes to the stamp, such as a customized one or one written before the
// stamp existed, is the user's. bridge.py.backup, which may hold the user's own
// bridge, and .bridge.py.previous-<n> copies of an interrupted replacement are
// never removed.
//
// Ownership is decided on the bytes read, and bridge.py is then removed by
// name, so an edit saved in between is lost.
func removeBridgeScriptAt(dir string, embedded []byte) (bool, error) {
	path := filepath.Join(dir, "bridge.py")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking bridge.py: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("reading bridge.py: %w", err)
	}
	if !bytes.Equal(content, embedded) && readBridgeStamp(dir) != bridgeStamp(content) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("removing bridge.py: %w", err)
	}
	for _, name := range []string{bridgeStampName, bridgePreviousName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return true, fmt.Errorf("removing %s: %w", name, err)
		}
	}
	return true, nil
}
