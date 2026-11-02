package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/asheshgoplani/agent-deck/internal/platform"
)

// ExternalServiceError reports a service definition file that another manager
// owns: it is a symlink, or it is read-only, with no write bit set or not
// writable by this user (home-manager installs its launch agents as 0444
// files). Agent Deck leaves such a file alone: it does not write or remove it,
// and it makes no service-manager call for it, so the other manager's service
// keeps running as configured.
type ExternalServiceError struct {
	Label  string // names the service, as the service manager does
	Path   string // the service definition file
	Reason string // "symlink" or "read-only"
}

func (e *ExternalServiceError) Error() string {
	return fmt.Sprintf("%s: managed externally (%s)", e.Label, e.Reason)
}

// serviceFileWritable reports whether this process can write path. A variable
// so a test can stand in for a file that only root could write.
var serviceFileWritable = func(path string) bool { return unix.Access(path, unix.W_OK) == nil }

// serviceFileExternal reports whether the service definition at path belongs
// to another manager. A missing file and a regular file with a write bit that
// this user can write (access(2) W_OK) are Agent Deck's: it returns (nil, nil).
// The file's owner is not compared with this user. The path is judged by Lstat,
// so a symlink is external whatever it points at, a dangling one included. A
// path that cannot be examined, and anything that is neither a symlink nor a
// regular file, is an error.
func serviceFileExternal(label, path string) (*ExternalServiceError, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checking %s at %s: %w", label, path, err)
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return &ExternalServiceError{Label: label, Path: path, Reason: "symlink"}, nil
	case mode.IsRegular():
		// The mode bits are tested first: root can write a 0444 file, but a file
		// that grants no write bit to anyone was made to be left alone.
		if mode.Perm()&0o222 == 0 || !serviceFileWritable(path) {
			return &ExternalServiceError{Label: label, Path: path, Reason: "read-only"}, nil
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("refusing to manage %s: %s is not a regular file (%s)", label, path, mode.Type())
	}
}

// requireOwnedServiceFile returns nil when path is missing or a regular file
// with a write bit that this user can write (access(2) W_OK; the file's owner
// is not checked), the *ExternalServiceError itself when another manager owns
// it, and any other failure as serviceFileExternal reports it.
// Callers return the result as it is. That spares each of them the nil check
// that returning serviceFileExternal's pointer as an error needs: a nil
// *ExternalServiceError stored in an error is not == nil.
func requireOwnedServiceFile(label, path string) error {
	ext, err := serviceFileExternal(label, path)
	if err != nil {
		return err
	}
	if ext != nil {
		return ext
	}
	return nil
}

// renameServiceFile moves a temp file over a service definition. A variable so
// a test can make the move fail.
var renameServiceFile = os.Rename

// writeServiceTemp writes content to a new file beside target, flushes it and
// returns its path. The mode is exactly perm: CreateTemp makes the file 0o600,
// and Chmod, unlike a create mode, is not trimmed by the umask. Any failure
// removes the file. The name starts with a dot and never ends in .plist,
// .service or .timer, so the globs that look for service files (the update
// hygiene pass globs *.plist) do not take it for one.
func writeServiceTemp(target string, content []byte, perm os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	err = f.Chmod(perm)
	if err == nil {
		_, err = f.Write(content)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// replaceLaunchAgent installs content as the launchd plist at plistPath and
// loads it. A plist that belongs to another manager is returned as an
// *ExternalServiceError before anything is written or unloaded. For an owned
// plist the new bytes are written to a temp file first, so a failed write
// leaves the running agent alone. Only then is the agent unloaded, the temp
// renamed over the plist and the plist loaded. A new plist gets mode 0o644, as
// os.WriteFile gave it; a replaced one keeps the mode of the old. If the rename
// fails the old plist is still in place and is loaded again. If the load fails
// the old bytes are put back and loaded again. The error says which step
// failed, and what to run by hand when the old plist could not be loaded again.
func replaceLaunchAgent(label, plistPath string, content []byte) error {
	if err := requireOwnedServiceFile(label, plistPath); err != nil {
		return err
	}

	// What is replaced: its bytes, to put back if the new plist is rejected, and
	// its mode, which the new file takes over.
	perm := os.FileMode(0o644)
	var old []byte
	hadOld := false
	info, err := os.Lstat(plistPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("reading %s plist: %w", label, err)
	default:
		if old, err = os.ReadFile(plistPath); err != nil {
			return fmt.Errorf("reading %s plist: %w", label, err)
		}
		hadOld, perm = true, info.Mode().Perm()
	}

	tmp, err := writeServiceTemp(plistPath, content, perm)
	if err != nil {
		return fmt.Errorf("writing %s plist: %w", label, err)
	}
	// Issued even for a first install, with no plist, as it always was; its
	// result is ignored.
	_ = runLaunchctl("unload", plistPath)
	if err := renameServiceFile(tmp, plistPath); err != nil {
		_ = os.Remove(tmp)
		if !hadOld {
			return fmt.Errorf("replacing %s plist: %w", label, err)
		}
		// The rename failed, so the old plist is still in place.
		if loadErr := runLaunchctl("load", plistPath); loadErr != nil {
			return fmt.Errorf("replacing %s plist: %w; the previous plist did not load: run 'launchctl load %s'", label, err, plistPath)
		}
		return fmt.Errorf("replacing %s plist: %w; reloaded the previous plist", label, err)
	}

	loadErr := runLaunchctl("load", plistPath)
	if loadErr == nil {
		return nil
	}
	if !hadOld {
		return fmt.Errorf("plist written but failed to load %s: %w", label, loadErr)
	}
	// The new plist was rejected. Put the old bytes back, and load them again
	// only once they are back.
	restored, err := writeServiceTemp(plistPath, old, perm)
	if err == nil {
		if err = renameServiceFile(restored, plistPath); err != nil {
			_ = os.Remove(restored)
		}
	}
	if err != nil {
		return fmt.Errorf("loading %s: %w; restoring the previous plist failed: %v", label, loadErr, err)
	}
	if err := runLaunchctl("load", plistPath); err != nil {
		return fmt.Errorf("loading %s: %w; restored the previous plist, but it did not load: run 'launchctl load %s'", label, loadErr, plistPath)
	}
	return fmt.Errorf("loading %s: %w; restored and reloaded the previous plist", label, loadErr)
}

// removeLaunchAgent unloads the launchd agent whose plist is at plistPath and
// deletes the plist. A missing plist is nothing to remove: it returns nil
// without a launchctl call. A plist that belongs to another manager is neither
// unloaded nor deleted: it is returned as an *ExternalServiceError. With
// strictUnload a failed unload keeps the plist and is returned, since the agent
// may still be running and `launchctl unload` needs the file; without it the
// failure is ignored and the plist is deleted anyway.
func removeLaunchAgent(label, plistPath string, strictUnload bool) error {
	if _, err := os.Lstat(plistPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := requireOwnedServiceFile(label, plistPath); err != nil {
		return err
	}
	if err := runLaunchctl("unload", plistPath); err != nil && strictUnload {
		return fmt.Errorf("unloading %s: %w", label, err)
	}
	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("removing %s plist: %w", label, err)
	}
	return nil
}

// errSystemdUserUnavailable is what replaceSystemdUnits returns, as it is, when
// there is no systemd user session to enable the units in: the units are in
// place and nothing was enabled. A caller wraps it with what to run by hand.
var errSystemdUserUnavailable = errors.New("systemd user session not available")

// systemdUnitFile is one unit of a set that is installed and removed together:
// a heartbeat's service and timer.
type systemdUnitFile struct {
	Name    string // the unit name, as systemctl knows it
	Path    string // the unit file
	Content []byte // what replaceSystemdUnits writes; removeSystemdUnits ignores it
}

// systemdUnitsExternal returns what serviceFileExternal reports for the first
// unit, in slice order, whose file belongs to another manager or cannot be
// examined, and (nil, nil) when every unit is Agent Deck's or missing. The units
// are one service, so one external file keeps all of them, and label names that
// service in the result.
func systemdUnitsExternal(label string, units []systemdUnitFile) (*ExternalServiceError, error) {
	for _, u := range units {
		if ext, err := serviceFileExternal(label, u.Path); err != nil || ext != nil {
			return ext, err
		}
	}
	return nil, nil
}

// requireOwnedSystemdUnits returns nil when every unit is missing or a regular
// file with a write bit that this user can write (access(2) W_OK; the file's
// owner is not checked), the *ExternalServiceError itself when another manager
// owns one, and any other failure as systemdUnitsExternal reports it. It does
// for a set of units what requireOwnedServiceFile does for one file: callers
// return the result as it is, without the nil check that returning
// systemdUnitsExternal's pointer as an error needs (a nil *ExternalServiceError
// stored in an error is not == nil).
func requireOwnedSystemdUnits(label string, units []systemdUnitFile) error {
	ext, err := systemdUnitsExternal(label, units)
	if err != nil {
		return err
	}
	if ext != nil {
		return ext
	}
	return nil
}

// replaceSystemdUnits installs units as one set, then enables enableUnit. A set
// in which a unit belongs to another manager is returned as an
// *ExternalServiceError before anything is written. Every unit is written to a
// temp file first, so a failed write leaves all of them as they were. The temps
// are then renamed into place in slice order, each as a 0o644 file. Nothing was
// stopped, so a rename that fails after another succeeded leaves the earlier
// units replaced; no temp is left behind. Only then is availability checked: with
// no systemd user session it returns errSystemdUserUnavailable, the units in
// place. Last comes `systemctl --user enable --now enableUnit`, with no stop and
// no daemon-reload.
func replaceSystemdUnits(label string, units []systemdUnitFile, enableUnit string) error {
	if err := requireOwnedSystemdUnits(label, units); err != nil {
		return err
	}

	// The temp of each unit that is not yet renamed into place; whatever is left
	// when this returns is removed.
	temps := make([]string, len(units))
	defer func() {
		for _, tmp := range temps {
			if tmp != "" {
				_ = os.Remove(tmp)
			}
		}
	}()
	for i, u := range units {
		tmp, err := writeServiceTemp(u.Path, u.Content, 0o644)
		if err != nil {
			return fmt.Errorf("writing %s: %w", u.Name, err)
		}
		temps[i] = tmp
	}
	for i, u := range units {
		if err := renameServiceFile(temps[i], u.Path); err != nil {
			return fmt.Errorf("installing %s: %w", u.Name, err)
		}
		temps[i] = ""
	}

	if !systemdUserAvailableCheck() {
		return errSystemdUserUnavailable
	}
	if err := runSystemctl("--user", "enable", "--now", enableUnit); err != nil {
		return fmt.Errorf("unit written but enable failed: %w", err)
	}
	return nil
}

// removeSystemdUnits disables and deletes a set of units. A set in which a unit
// belongs to another manager is returned as an *ExternalServiceError, with no
// systemctl call and no file removed. With no unit file there is nothing to
// remove: it returns nil without a systemctl call. When the file of disableUnit
// exists it runs `systemctl --user disable --now disableUnit`. With
// strictDisable a failure keeps every file and is returned, since the unit may
// still be running and disabling it needs its file; without it the failure is
// ignored and the files are removed anyway. Then each unit file that exists is
// removed, in slice order, and a daemon-reload follows whose result is ignored.
func removeSystemdUnits(label string, units []systemdUnitFile, disableUnit string, strictDisable bool) error {
	if err := requireOwnedSystemdUnits(label, units); err != nil {
		return err
	}

	var present []systemdUnitFile
	for _, u := range units {
		if _, err := os.Lstat(u.Path); err == nil {
			present = append(present, u)
		}
	}
	if len(present) == 0 {
		return nil
	}

	if slices.ContainsFunc(present, func(u systemdUnitFile) bool { return u.Name == disableUnit }) {
		if err := runSystemctl("--user", "disable", "--now", disableUnit); err != nil && strictDisable {
			return fmt.Errorf("disabling %s: %w", disableUnit, err)
		}
	}
	for _, u := range present {
		if err := os.Remove(u.Path); err != nil {
			return fmt.Errorf("removing %s: %w", u.Name, err)
		}
	}
	_ = runSystemctl("--user", "daemon-reload")
	return nil
}

// probeExternal returns the *ExternalServiceError for the service file at path,
// or nil when Agent Deck owns it, it is missing, or it cannot be checked: the
// install or removal that follows reports that error.
func probeExternal(label, path string) *ExternalServiceError {
	ext, err := serviceFileExternal(label, path)
	if err != nil {
		return nil
	}
	return ext
}

// BridgeDaemonExternal returns the *ExternalServiceError for the bridge's
// service file (the launchd plist on macOS, the systemd unit on Linux and WSL2)
// when another manager owns it, so `conductor setup` and `conductor migrate-dir`
// can ask before they touch what that manager keeps. It is nil when Agent Deck
// owns the file, it is missing, it cannot be checked, or the platform has no
// service manager here.
func BridgeDaemonExternal() *ExternalServiceError {
	switch platform.Detect() {
	case platform.PlatformMacOS:
		if path, err := LaunchdPlistPath(); err == nil {
			return probeExternal(LaunchdPlistName, path)
		}
	case platform.PlatformLinux, platform.PlatformWSL2:
		if path, err := SystemdBridgeServicePath(); err == nil {
			return probeExternal(systemdBridgeServiceName, path)
		}
	}
	return nil
}

// HeartbeatDaemonExternal is BridgeDaemonExternal for the heartbeat of the
// conductor name. On systemd the heartbeat is a service and a timer, named by
// the timer: the first of the two files another manager owns is returned. The
// service counts as well, since it hard-codes the path of heartbeat.sh, which
// `conductor migrate-dir` moves.
func HeartbeatDaemonExternal(name string) *ExternalServiceError {
	switch platform.Detect() {
	case platform.PlatformMacOS:
		if path, err := HeartbeatPlistPath(name); err == nil {
			return probeExternal(HeartbeatPlistLabel(name), path)
		}
	case platform.PlatformLinux, platform.PlatformWSL2:
		label := SystemdHeartbeatTimerName(name)
		for _, unitPath := range []func(string) (string, error){SystemdHeartbeatServicePath, SystemdHeartbeatTimerPath} {
			if path, err := unitPath(name); err == nil {
				if ext := probeExternal(label, path); ext != nil {
					return ext
				}
			}
		}
	}
	return nil
}
