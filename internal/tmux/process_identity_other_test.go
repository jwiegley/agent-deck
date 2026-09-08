//go:build !darwin && !linux

package tmux

import (
	"errors"
	"syscall"
	"testing"
)

func TestRuntimeLifecycle_UnsupportedPlatformIdentitySignalFailsClosed(t *testing.T) {
	signaled, err := signalProcessIdentity(ProcessIdentity{PID: 4242, StartToken: "birth-a"}, syscall.SIGTERM)
	if signaled || !errors.Is(err, errProcessIdentitySignalUnsupported) {
		t.Fatalf("signal result = %v, err=%v; want explicit unsupported failure", signaled, err)
	}
}
