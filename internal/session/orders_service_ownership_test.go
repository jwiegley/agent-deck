package session

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/platform"
)

func TestOrdersDaemon_ExternalFilesAreUntouched(t *testing.T) {
	for _, target := range []string{"launchd", "service", "timer"} {
		for _, kind := range externalUnitKinds {
			t.Run(target+"/"+kind.name, func(t *testing.T) {
				useTempConductorHome(t)
				t.Setenv("PATH", t.TempDir())
				fake := installServiceManagerFake(t)
				forbidSystemdUserCheck(t)
				path, err := ordersLaunchdPath()
				if err != nil {
					t.Fatal(err)
				}
				label := ordersLaunchdLabel
				install := installOrdersLaunchd
				isHost := platform.Detect() == platform.PlatformMacOS
				if target != "launchd" {
					dir, err := SystemdUserDir()
					if err != nil {
						t.Fatal(err)
					}
					service := filepath.Join(dir, SystemdOrdersServiceName())
					timer := filepath.Join(dir, SystemdOrdersTimerName())
					writeServiceFixture(t, service, "owned service", 0o644)
					writeServiceFixture(t, timer, "owned timer", 0o644)
					path = service
					if target == "timer" {
						path = timer
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					label, install = SystemdOrdersTimerName(), installOrdersSystemd
					isHost = platform.Detect() == platform.PlatformLinux || platform.Detect() == platform.PlatformWSL2
				}
				kind.setup(t, path)
				before := snapshotUnitDir(t, filepath.Dir(path))
				_, err = install()
				wantExternal(t, err, label, path, kind.reason)
				wantCalls(t, fake.Calls())
				if got := snapshotUnitDir(t, filepath.Dir(path)); !maps.Equal(got, before) {
					t.Fatalf("install changed externally managed files: got %v, want %v", got, before)
				}
				if isHost {
					if !OrdersDaemonInstalled() {
						t.Fatal("external unit was not detected, so teardown cannot report it")
					}
					wantExternal(t, UninstallOrdersDaemon(), label, path, kind.reason)
					wantCalls(t, fake.Calls())
					if got := snapshotUnitDir(t, filepath.Dir(path)); !maps.Equal(got, before) {
						t.Fatalf("uninstall changed externally managed files: got %v, want %v", got, before)
					}
				}
			})
		}
	}
}
