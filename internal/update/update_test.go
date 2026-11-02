package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name     string
		v1, v2   string
		expected int
	}{
		{"equal versions", "1.0.0", "1.0.0", 0},
		{"v1 less than v2", "1.0.0", "1.0.1", -1},
		{"v1 greater than v2", "2.0.0", "1.9.9", 1},
		{"with v prefix", "v1.2.3", "v1.2.3", 0},
		{"mixed prefix", "v1.0.0", "1.0.1", -1},
		{"major difference", "0.8.85", "0.9.0", -1},
		{"patch difference", "0.8.84", "0.8.85", -1},
		{"two-part version padded", "1.0", "1.0.0", 0},
		{"single-part version", "2", "1.9.9", 1},
		// #2164: a pre-release is older than the release it previews, so a
		// preview controller never judges a remote on that release as behind.
		{"pre-release below its release", "1.16.4-switch-preview.abc1234", "1.16.4", -1},
		{"release above its pre-release", "1.16.4", "1.16.4-switch-preview.abc1234", 1},
		{"pre-release above the previous release", "1.16.4-rc.1", "1.16.3", 1},
		{"pre-release below the next release", "1.16.4-rc.1", "1.16.5", -1},
		{"same pre-release", "v1.16.4-rc.1", "1.16.4-rc.1", 0},
		{"pre-releases of one core compare by tag", "1.16.4-rc.1", "1.16.4-rc.2", -1},
		{"numeric identifiers compare as numbers", "1.16.4-rc.2", "1.16.4-rc.10", -1},
		{"numeric below alphanumeric", "1.16.4-1", "1.16.4-alpha", -1},
		{"alphanumeric identifiers compare as text", "1.16.4-alpha", "1.16.4-beta", -1},
		{"a prefix tag sorts first", "1.16.4-rc.1", "1.16.4-rc.1.1", -1},
		{"preview build with sha tag", "1.16.4-switch-preview.abc", "1.16.4-switch-preview.abd", -1},
		{"numeric identifier past int64 is still larger", "1.0.0-rc.9223372036854775808", "1.0.0-rc.2", 1},
		{"numeric identifier past int64 above int64 max", "1.0.0-rc.9223372036854775808", "1.0.0-rc.9223372036854775807", 1},
		{"numeric identifier far past int64", "1.0.0-rc.99999999999999999999999", "1.0.0-rc.3", 1},
		{"leading zeros do not change the value", "1.0.0-rc.010", "1.0.0-rc.10", 0},
		{"build metadata is ignored", "1.16.4+abc", "1.16.4", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CompareVersions(tt.v1, tt.v2)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestReleaseSupersedes_LocalBuild pins #2710: a "+local" build of a version
// is offered the published release of that same version, while ordinary
// semver precedence (pre-releases, other build metadata) is unchanged.
func TestReleaseSupersedes_LocalBuild(t *testing.T) {
	tests := []struct {
		release, current string
		want             bool
	}{
		{"1.16.29", "1.16.29+local.20261018.abc1234", true},
		{"v1.16.29", "v1.16.29+local", true},
		{"1.16.30", "1.16.29+local.x", true},
		{"1.16.28", "1.16.29+local.x", false},
		{"1.16.29-rc.2", "1.16.29-rc.2+local.x", true},
		{"1.16.29-rc.1", "1.16.29-rc.2+local.x", false},
		{"1.16.29+local.y", "1.16.29+local.x", false},
		{"1.16.29", "1.16.29+build.5", false},
		{"1.16.29", "1.16.29+localhost", false},
		{"1.16.29", "1.16.29", false},
		{"1.16.30", "1.16.29", true},
		{"1.16.29-rc.1", "1.16.29", false},
		{"1.16.29", "1.16.29-rc.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.release+"_over_"+tt.current, func(t *testing.T) {
			assert.Equal(t, tt.want, ReleaseSupersedes(tt.release, tt.current))
		})
	}
}

// TestCachedUpdateInfo_LocalBuildOfferedSameRelease is the #2710 user path:
// a machine running 1.16.29+local... with 1.16.29 cached as latest must see
// the update as available.
func TestCachedUpdateInfo_LocalBuildOfferedSameRelease(t *testing.T) {
	info := &UpdateInfo{}
	applyCacheToInfo(info, &UpdateCache{LatestVersion: "1.16.29"}, "1.16.29+local.20261018.abc1234")
	assert.True(t, info.Available, "a +local build must be offered its own release")
	assert.Equal(t, 1, CountReleasesBehind("1.16.29+local.x", []Release{{TagName: "v1.16.29"}, {TagName: "v1.16.28"}}))

	info = &UpdateInfo{}
	applyCacheToInfo(info, &UpdateCache{LatestVersion: "1.16.29"}, "1.16.29")
	assert.False(t, info.Available, "the release itself is current")
}

func TestParseChangelog(t *testing.T) {
	content := `# Changelog

## [0.8.85] - 2026-01-28

### Fixed
- Fix notification bar truncation

### Added
- New analytics panel

## [0.8.84] - 2026-01-27

### Fixed
- Fix status detection race
`

	entries := ParseChangelog(content)
	require.Len(t, entries, 2)

	assert.Equal(t, "0.8.85", entries[0].Version)
	assert.Equal(t, "2026-01-28", entries[0].Date)
	assert.Contains(t, entries[0].Content, "Fix notification bar truncation")
	assert.Contains(t, entries[0].Content, "New analytics panel")

	assert.Equal(t, "0.8.84", entries[1].Version)
	assert.Equal(t, "2026-01-27", entries[1].Date)
	assert.Contains(t, entries[1].Content, "Fix status detection race")
}

func TestParseChangelogEmpty(t *testing.T) {
	entries := ParseChangelog("")
	assert.Empty(t, entries)
}

func TestParseChangelogNoHeaders(t *testing.T) {
	entries := ParseChangelog("Just some text\nwithout version headers\n")
	assert.Empty(t, entries)
}

func TestGetChangesBetweenVersions(t *testing.T) {
	entries := []ChangelogEntry{
		{Version: "0.8.85", Date: "2026-01-28", Content: "latest changes"},
		{Version: "0.8.84", Date: "2026-01-27", Content: "middle changes"},
		{Version: "0.8.83", Date: "2026-01-26", Content: "old changes"},
	}

	tests := []struct {
		name          string
		current       string
		latest        string
		expectedCount int
		expectedFirst string
	}{
		{"one version behind", "0.8.84", "0.8.85", 1, "0.8.85"},
		{"two versions behind", "0.8.83", "0.8.85", 2, "0.8.85"},
		{"up to date", "0.8.85", "0.8.85", 0, ""},
		{"with v prefix", "v0.8.83", "v0.8.85", 2, "0.8.85"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetChangesBetweenVersions(entries, tt.current, tt.latest)
			assert.Len(t, result, tt.expectedCount)
			if tt.expectedCount > 0 {
				assert.Equal(t, tt.expectedFirst, result[0].Version)
			}
		})
	}
}

func TestFormatChangelogForDisplay(t *testing.T) {
	t.Run("empty entries", func(t *testing.T) {
		result := FormatChangelogForDisplay(nil)
		assert.Empty(t, result)
	})

	t.Run("section headers and bullet items", func(t *testing.T) {
		entries := []ChangelogEntry{
			{
				Version: "0.8.85",
				Date:    "2026-01-28",
				Content: "### Fixed\n- Bug fix one\n- Bug fix two\n\n### Added\n- New feature",
			},
		}
		result := FormatChangelogForDisplay(entries)
		assert.Contains(t, result, "v0.8.85")
		assert.Contains(t, result, "2026-01-28")
		assert.Contains(t, result, "[Fixed]")
		assert.Contains(t, result, "- Bug fix one")
		assert.Contains(t, result, "- Bug fix two")
		assert.Contains(t, result, "[Added]")
		assert.Contains(t, result, "- New feature")
	})

	t.Run("preserves unrecognized lines", func(t *testing.T) {
		entries := []ChangelogEntry{
			{
				Version: "1.0.0",
				Content: "### Changed\n- Item one\nSome plain text line\n  nested content",
			},
		}
		result := FormatChangelogForDisplay(entries)
		assert.Contains(t, result, "Some plain text line",
			"non-empty lines that don't match any prefix pattern should still appear")
	})

	t.Run("version without date", func(t *testing.T) {
		entries := []ChangelogEntry{
			{Version: "0.1.0", Content: "- Initial release"},
		}
		result := FormatChangelogForDisplay(entries)
		assert.Contains(t, result, "v0.1.0")
		assert.NotContains(t, result, "()")
	})
}

func TestUpdateBridgePy_NoConductorDir(t *testing.T) {
	tmpHome := isolateUpdatePaths(t)

	err := UpdateBridgePy()
	require.NoError(t, err)

	legacyCondDir := filepath.Join(tmpHome, ".agent-deck", "conductor")
	xdgCondDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "agent-deck", "conductor")
	_, legacyErr := os.Stat(legacyCondDir)
	_, xdgErr := os.Stat(xdgCondDir)
	assert.True(t, os.IsNotExist(legacyErr), "legacy conductor dir should not be created when not installed")
	assert.True(t, os.IsNotExist(xdgErr), "XDG conductor dir should not be created when not installed")
}

func TestUpdateBridgePy_UsesInjectedInstallerAndWritesNoBackup(t *testing.T) {
	tmpHome := isolateUpdatePaths(t)

	condDir := filepath.Join(tmpHome, ".agent-deck", "conductor")
	require.NoError(t, os.MkdirAll(condDir, 0o755))

	bridgePath := filepath.Join(condDir, "bridge.py")
	legacyContent := "# legacy bridge\nprint('old bridge')\n"
	require.NoError(t, os.WriteFile(bridgePath, []byte(legacyContent), 0o755))

	SetBridgeScriptInstaller(func() error {
		return os.WriteFile(bridgePath, []byte("# injected bridge\n"), 0o755)
	})
	t.Cleanup(func() {
		SetBridgeScriptInstaller(nil)
	})

	err := UpdateBridgePy()
	require.NoError(t, err)

	assert.NoFileExists(t, bridgePath+".backup", "update must not write bridge.py.backup")

	newContent, err := os.ReadFile(bridgePath)
	require.NoError(t, err)
	assert.Equal(t, "# injected bridge\n", string(newContent))
}

// TestUpdateBridgePy_NonCLICallerDoesNotHardFail is the regression test for
// Blocker 3. The bridge installer is injected only by the CLI layer
// (initUpdateSettings). A non-CLI caller (e.g. a watcher or library consumer)
// that triggers UpdateBridgePy with a conductor dir present must NOT hard-fail
// with "bridge.py installer is not configured" — it should gracefully no-op so
// the surrounding update flow is not aborted.
func TestUpdateBridgePy_NonCLICallerDoesNotHardFail(t *testing.T) {
	tmpHome := isolateUpdatePaths(t)

	// Conductor dir exists (so we get past the "not installed" early return),
	// exercising the installer branch.
	condDir := filepath.Join(tmpHome, ".agent-deck", "conductor")
	require.NoError(t, os.MkdirAll(condDir, 0o755))

	// Simulate a non-CLI caller: installer never injected.
	SetBridgeScriptInstaller(nil)

	err := UpdateBridgePy()
	require.NoError(t, err, "UpdateBridgePy must not hard-fail when installer is not configured (non-CLI caller)")
}

// TestUpdateBridgePy_NoOpLeavesExistingBackupUntouched is the regression test
// for Blocker 3's data-safety contract: when there is no injected installer the
// function advertises a true no-op ("the existing bridge.py and its .backup are
// left untouched"). Previously the .backup was overwritten with the current
// bridge.py BEFORE the nil-installer check ran, silently corrupting a prior
// good backup. This asserts the no-op truly does not read or rewrite .backup.
func TestUpdateBridgePy_NoOpLeavesExistingBackupUntouched(t *testing.T) {
	tmpHome := isolateUpdatePaths(t)

	condDir := filepath.Join(tmpHome, ".agent-deck", "conductor")
	require.NoError(t, os.MkdirAll(condDir, 0o755))

	bridgePath := filepath.Join(condDir, "bridge.py")
	backupPath := bridgePath + ".backup"

	// A live bridge.py and a PRE-EXISTING, distinct .backup (e.g. from an
	// earlier successful update). The no-op must not overwrite the backup with
	// the current bridge.py content.
	require.NoError(t, os.WriteFile(bridgePath, []byte("# current bridge\n"), 0o755))
	priorBackup := "# prior good backup\nprint('keep me')\n"
	require.NoError(t, os.WriteFile(backupPath, []byte(priorBackup), 0o644))

	// Non-CLI caller: installer never injected -> no-op path.
	SetBridgeScriptInstaller(nil)

	err := UpdateBridgePy()
	require.NoError(t, err)

	// The pre-existing backup must be byte-for-byte intact.
	gotBackup, err := os.ReadFile(backupPath)
	require.NoError(t, err)
	assert.Equal(t, priorBackup, string(gotBackup), "no-op must not touch bridge.py.backup")

	// bridge.py itself must also be unchanged.
	gotBridge, err := os.ReadFile(bridgePath)
	require.NoError(t, err)
	assert.Equal(t, "# current bridge\n", string(gotBridge), "no-op must not touch bridge.py")
}

// TestUpdateBridgePy_HonorsConductorDirOverride is the regression test for the
// update-path bypass: UpdateBridgePy previously resolved its conductor dir via
// agentpaths.EffectiveDataPath (the default XDG/legacy path), ignoring
// [conductor].dir. With an override pointing outside the default, the existence
// guard, the .backup, and the refresh all targeted the wrong directory. The fix
// routes through the injected conductorDirResolver (session.ConductorDir).
//
// This test pins the behavior WITHOUT importing internal/session: it injects a
// resolver returning a custom override dir and asserts the refresh lands there,
// not under the default conductor root, and that no bridge.py.backup is written.
func TestUpdateBridgePy_HonorsConductorDirOverride(t *testing.T) {
	tmpHome := isolateUpdatePaths(t)

	// The override lives OUTSIDE the default XDG/legacy conductor roots.
	override := filepath.Join(t.TempDir(), "conductor homes", "conductor")
	require.NoError(t, os.MkdirAll(override, 0o755))

	overrideBridge := filepath.Join(override, "bridge.py")
	legacyContent := "# pre-existing override bridge\n"
	require.NoError(t, os.WriteFile(overrideBridge, []byte(legacyContent), 0o755))

	SetConductorDirResolver(func() (string, error) { return override, nil })
	SetBridgeScriptInstaller(func() error {
		return os.WriteFile(overrideBridge, []byte("# refreshed override bridge\n"), 0o755)
	})
	t.Cleanup(func() {
		SetConductorDirResolver(nil)
		SetBridgeScriptInstaller(nil)
	})

	require.NoError(t, UpdateBridgePy())

	// The refresh lands under the OVERRIDE dir, with no .backup beside it.
	assert.NoFileExists(t, overrideBridge+".backup", "update must not write bridge.py.backup")

	refreshed, err := os.ReadFile(overrideBridge)
	require.NoError(t, err)
	assert.Equal(t, "# refreshed override bridge\n", string(refreshed), "refresh must target the override dir")

	// The default conductor roots must be untouched — proving the guard used the
	// override, not agentpaths' default resolution.
	legacyCondDir := filepath.Join(tmpHome, ".agent-deck", "conductor")
	xdgCondDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "agent-deck", "conductor")
	_, legacyErr := os.Stat(legacyCondDir)
	_, xdgErr := os.Stat(xdgCondDir)
	assert.True(t, os.IsNotExist(legacyErr), "default legacy conductor dir must not be touched")
	assert.True(t, os.IsNotExist(xdgErr), "default XDG conductor dir must not be touched")
}

// TestUpdateBridgePy_OverrideGuardSkipsWhenOverrideMissing complements the
// positive test: when the override dir does NOT exist, UpdateBridgePy must skip
// (early return) EVEN IF the default conductor root exists. Pre-fix, the guard
// keyed off the default dir and would have refreshed it; the fix keys off the
// resolved override.
func TestUpdateBridgePy_OverrideGuardSkipsWhenOverrideMissing(t *testing.T) {
	tmpHome := isolateUpdatePaths(t)

	// Default legacy conductor root EXISTS...
	defaultCondDir := filepath.Join(tmpHome, ".agent-deck", "conductor")
	require.NoError(t, os.MkdirAll(defaultCondDir, 0o755))

	// ...but the override points at a non-existent dir.
	override := filepath.Join(t.TempDir(), "missing", "conductor")
	SetConductorDirResolver(func() (string, error) { return override, nil })

	installerCalled := false
	SetBridgeScriptInstaller(func() error {
		installerCalled = true
		return nil
	})
	t.Cleanup(func() {
		SetConductorDirResolver(nil)
		SetBridgeScriptInstaller(nil)
	})

	require.NoError(t, UpdateBridgePy())
	assert.False(t, installerCalled, "installer must not run when the override dir is missing (guard must key off the override, not the default)")
}

// TestUpdateBridgePy_LeavesExistingBackupUntouched pins the safety rule behind
// dropping the backup: a bridge.py.backup left by a pre-fix update may hold the
// user's own bridge, so a refresh must not rewrite it.
func TestUpdateBridgePy_LeavesExistingBackupUntouched(t *testing.T) {
	tmpHome := isolateUpdatePaths(t)

	condDir := filepath.Join(tmpHome, ".agent-deck", "conductor")
	require.NoError(t, os.MkdirAll(condDir, 0o755))

	bridgePath := filepath.Join(condDir, "bridge.py")
	backupPath := bridgePath + ".backup"
	userBridge := "# custom bridge saved by a pre-fix update\nprint('keep me')\n"
	require.NoError(t, os.WriteFile(bridgePath, []byte("# shipped bridge\n"), 0o755))
	require.NoError(t, os.WriteFile(backupPath, []byte(userBridge), 0o644))

	SetBridgeScriptInstaller(func() error {
		return os.WriteFile(bridgePath, []byte("# newer shipped bridge\n"), 0o755)
	})
	t.Cleanup(func() { SetBridgeScriptInstaller(nil) })

	require.NoError(t, UpdateBridgePy())

	gotBackup, err := os.ReadFile(backupPath)
	require.NoError(t, err)
	assert.Equal(t, userBridge, string(gotBackup), "update must never touch an existing bridge.py.backup")

	gotBridge, err := os.ReadFile(bridgePath)
	require.NoError(t, err)
	assert.Equal(t, "# newer shipped bridge\n", string(gotBridge), "the installer must have run")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	// Read on a separate goroutine while fn runs, rather than after it
	// returns: the OS pipe buffer is bounded (~64KB), so a sequential
	// "run fn, then read" would deadlock if fn ever printed more than that
	// in one call (fn blocks on the full pipe with nobody draining it yet).
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	// Close w even if fn panics, so the reader goroutine always sees EOF
	// and this helper never leaks it (avoids the fd leak on a panicking fn).
	defer func() { _ = w.Close() }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	return <-done
}

// keptBridgeError stands in for session.BridgeKeptError, which this package
// cannot import (internal/session imports internal/update).
type keptBridgeError struct{ notice string }

func (e *keptBridgeError) Error() string { return "bridge.py has local changes; left unchanged" }

func (e *keptBridgeError) BridgeKeptNotice() string { return e.notice }

var _ BridgeKeptNotifier = (*keptBridgeError)(nil)

func TestUpdateBridgePy_KeptBridgePrintsNoticeAndSucceeds(t *testing.T) {
	condDir := filepath.Join(isolateUpdatePaths(t), ".agent-deck", "conductor")
	require.NoError(t, os.MkdirAll(condDir, 0o755))
	bridgePath := filepath.Join(condDir, "bridge.py")
	custom := "# customized bridge\nprint('qna hook')\n"
	require.NoError(t, os.WriteFile(bridgePath, []byte(custom), 0o755))
	const notice = "[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run 'agent-deck conductor setup <name>'."
	// Wrapped once: a bare type assertion misses it, errors.As finds it.
	SetBridgeScriptInstaller(func() error { return fmt.Errorf("wrapped: %w", &keptBridgeError{notice: notice}) })
	t.Cleanup(func() { SetBridgeScriptInstaller(nil) })

	var err error
	out := captureStdout(t, func() { err = UpdateBridgePy() })
	require.NoError(t, err, "a kept bridge is not an update failure")
	assert.Contains(t, out, notice+"\n")
	assert.NotContains(t, out, "✓ bridge.py updated!")
	assert.NoFileExists(t, bridgePath+".backup")
	got, readErr := os.ReadFile(bridgePath)
	require.NoError(t, readErr)
	assert.Equal(t, custom, string(got))

	// Only a kept bridge becomes a notice; any other installer error still fails.
	SetBridgeScriptInstaller(func() error { return errors.New("disk full") })
	assert.EqualError(t, UpdateBridgePy(), "failed to install bridge.py: disk full")
}

func TestNormalizeReleaseTag(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"whitespace", "  ", ""},
		{"plain semver", "1.7.4", "v1.7.4"},
		{"already prefixed", "v1.7.4", "v1.7.4"},
		{"uppercase prefix", "V1.7.4", "v1.7.4"},
		{"surrounding whitespace", "  1.7.4  ", "v1.7.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NormalizeReleaseTag(tt.input))
		})
	}
}

func TestFetchReleaseByTag(t *testing.T) {
	rel := Release{
		TagName: "v1.7.4",
		Name:    "v1.7.4",
		HTMLURL: "https://example/releases/v1.7.4",
		Assets: []Asset{{
			Name:               "agent-deck_1.7.4_darwin_arm64.tar.gz",
			BrowserDownloadURL: "https://example/download/agent-deck_1.7.4_darwin_arm64.tar.gz",
			Size:               123,
		}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+GitHubRepo+"/releases/tags/v1.7.4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origURL := apiBaseURL
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = origURL })

	t.Run("accepts plain semver", func(t *testing.T) {
		rel, err := FetchReleaseByTag("1.7.4")
		require.NoError(t, err)
		require.NotNil(t, rel)
		assert.Equal(t, "v1.7.4", rel.TagName)
		assert.Equal(t, "https://example/download/agent-deck_1.7.4_darwin_arm64.tar.gz",
			GetAssetURLForPlatform(rel, "darwin", "arm64"))
	})

	t.Run("accepts prefixed tag", func(t *testing.T) {
		rel, err := FetchReleaseByTag("v1.7.4")
		require.NoError(t, err)
		assert.Equal(t, "v1.7.4", rel.TagName)
	})

	t.Run("missing tag", func(t *testing.T) {
		_, err := FetchReleaseByTag("99.0.0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("empty tag", func(t *testing.T) {
		_, err := FetchReleaseByTag("  ")
		require.Error(t, err)
	})
}

func TestPerformVerifiedUpdate_MatchingChecksumInstalls(t *testing.T) {
	oldBinary := []byte("old-agent-deck")
	newBinary := []byte("new-agent-deck")
	execPath := selfUpdateTestTarget(t, oldBinary)

	archive := makeTarGz(t, newBinary)
	checksums := []byte(sha256hex(archive) + "  agent-deck_1.2.3_linux_amd64.tar.gz\n")
	rel, cleanup := selfUpdateReleaseServer(t, archive, checksums, http.StatusOK)
	defer cleanup()

	err := PerformVerifiedUpdate(rel, "linux", "amd64")
	require.NoError(t, err)

	installed, err := os.ReadFile(execPath)
	require.NoError(t, err)
	assert.Equal(t, newBinary, installed)
}

func TestPerformVerifiedUpdate_MissingChecksumEntryLeavesBinaryUntouched(t *testing.T) {
	oldBinary := []byte("old-agent-deck")
	newBinary := []byte("new-agent-deck")
	execPath := selfUpdateTestTarget(t, oldBinary)

	archive := makeTarGz(t, newBinary)
	checksums := []byte(sha256hex(archive) + "  agent-deck_1.2.3_windows_amd64.tar.gz\n")
	rel, cleanup := selfUpdateReleaseServer(t, archive, checksums, http.StatusOK)
	defer cleanup()

	err := PerformVerifiedUpdate(rel, "linux", "amd64")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "no published sha-256 checksum")

	installed, readErr := os.ReadFile(execPath)
	require.NoError(t, readErr)
	assert.Equal(t, oldBinary, installed)
}

func TestPerformVerifiedUpdate_MismatchedChecksumLeavesBinaryUntouched(t *testing.T) {
	oldBinary := []byte("old-agent-deck")
	newBinary := []byte("new-agent-deck")
	execPath := selfUpdateTestTarget(t, oldBinary)

	archive := makeTarGz(t, newBinary)
	checksums := []byte(sha256hex([]byte("different archive")) + "  agent-deck_1.2.3_linux_amd64.tar.gz\n")
	rel, cleanup := selfUpdateReleaseServer(t, archive, checksums, http.StatusOK)
	defer cleanup()

	err := PerformVerifiedUpdate(rel, "linux", "amd64")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "mismatch")

	installed, readErr := os.ReadFile(execPath)
	require.NoError(t, readErr)
	assert.Equal(t, oldBinary, installed)
}

func TestPerformVerifiedUpdate_ChecksumsDownloadFailureLeavesBinaryUntouched(t *testing.T) {
	oldBinary := []byte("old-agent-deck")
	newBinary := []byte("new-agent-deck")
	execPath := selfUpdateTestTarget(t, oldBinary)

	archive := makeTarGz(t, newBinary)
	rel, cleanup := selfUpdateReleaseServer(t, archive, nil, http.StatusNotFound)
	defer cleanup()

	err := PerformVerifiedUpdate(rel, "linux", "amd64")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksums.txt")
	assert.Contains(t, err.Error(), "status 404")

	installed, readErr := os.ReadFile(execPath)
	require.NoError(t, readErr)
	assert.Equal(t, oldBinary, installed)
}

func selfUpdateTestTarget(t *testing.T, initial []byte) string {
	t.Helper()

	isolateUpdatePaths(t)
	execPath := filepath.Join(t.TempDir(), "agent-deck")
	require.NoError(t, os.WriteFile(execPath, initial, 0o755))

	orig := detectHomebrewManagedInstall
	detectHomebrewManagedInstall = func() (string, string, bool, error) {
		return execPath, "", false, nil
	}
	t.Cleanup(func() { detectHomebrewManagedInstall = orig })

	return execPath
}

func selfUpdateReleaseServer(t *testing.T, archive, checksums []byte, checksumsStatus int) (*Release, func()) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/agent-deck_1.2.3_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(checksumsStatus)
		_, _ = w.Write(checksums)
	})
	srv := httptest.NewServer(mux)

	rel := &Release{
		TagName: "v1.2.3",
		Assets: []Asset{
			{Name: "agent-deck_1.2.3_linux_amd64.tar.gz", BrowserDownloadURL: srv.URL + "/agent-deck_1.2.3_linux_amd64.tar.gz"},
			{Name: ChecksumsAssetName, BrowserDownloadURL: srv.URL + "/checksums.txt"},
		},
	}
	return rel, srv.Close
}

func TestHomebrewUpgradeHint(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantOK   bool
		wantHint string
	}{
		{
			name:     "mac arm64 cellar",
			path:     "/opt/homebrew/Cellar/agent-deck/0.19.14/bin/agent-deck",
			wantOK:   true,
			wantHint: "brew upgrade asheshgoplani/tap/agent-deck",
		},
		{
			name:     "mac intel cellar",
			path:     "/usr/local/Cellar/agent-deck/0.19.14/bin/agent-deck",
			wantOK:   true,
			wantHint: "brew upgrade asheshgoplani/tap/agent-deck",
		},
		{
			name:     "linuxbrew cellar",
			path:     "/home/linuxbrew/.linuxbrew/Cellar/agent-deck/0.19.14/bin/agent-deck",
			wantOK:   true,
			wantHint: "brew upgrade asheshgoplani/tap/agent-deck",
		},
		{
			name:   "standalone local binary",
			path:   "/Users/ashesh/.local/bin/agent-deck",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint, ok := HomebrewUpgradeHint(tt.path)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantHint, hint)
		})
	}
}

// installFakeGh writes an executable `gh` shell script into a temp dir and
// prepends that dir to PATH so resolveGitHubToken's fallback runs it.
func installFakeGh(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script requires a POSIX shell")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	return dir
}

func TestResolveGitHubToken_OnlyUsesGithubComScopedGhToken(t *testing.T) {
	t.Run("passes --hostname github.com", func(t *testing.T) {
		dir := installFakeGh(t, `echo "$@" >> "$(dirname "$0")/args"; echo tok`)
		assert.Equal(t, "tok", resolveGitHubToken())
		argv, err := os.ReadFile(filepath.Join(dir, "args"))
		require.NoError(t, err)
		assert.Equal(t, "auth token --hostname github.com", strings.TrimSpace(string(argv)))
	})

	t.Run("GHE-only gh yields no token", func(t *testing.T) {
		// Mimics gh on a machine whose hosts.yml only knows an enterprise
		// host: the hostless form returns the enterprise token, the
		// github.com-scoped form fails.
		installFakeGh(t, `case " $* " in
  *" --hostname github.com "*) echo "no oauth token found for github.com" >&2; exit 1 ;;
  *) echo ghe_fake_token ;;
esac`)
		assert.Equal(t, "", resolveGitHubToken())
	})
}
