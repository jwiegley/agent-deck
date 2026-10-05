//go:build runtime_lifecycle_helper

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func main() {
	if len(os.Args) == 6 && os.Args[1] == "contend" {
		ready := os.NewFile(uintptr(3), "runtime-lifecycle-ready")
		release := os.NewFile(uintptr(4), "runtime-lifecycle-release")
		if ready == nil || release == nil {
			fmt.Fprintln(os.Stderr, "runtime lifecycle contender barrier unavailable")
			os.Exit(2)
		}
		defer ready.Close()
		defer release.Close()
		result, err := session.RunRuntimeLifecycleContenderHelper(session.RuntimeLifecycleHelperConfig{
			DBPath: os.Args[2], InventoryPath: os.Args[3], LockRoot: os.Args[4],
			SessionName: os.Args[5], Ready: ready, Release: release,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: runtime-lifecycle-crash-helper STAGE DB INVENTORY LOCK_ROOT | contend DB INVENTORY LOCK_ROOT SESSION_NAME")
		os.Exit(2)
	}
	err := session.RunRuntimeLifecycleCrashHelper(session.RuntimeLifecycleHelperConfig{
		Stage:         session.RuntimeTransitionStage(os.Args[1]),
		DBPath:        os.Args[2],
		InventoryPath: os.Args[3],
		LockRoot:      os.Args[4],
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
