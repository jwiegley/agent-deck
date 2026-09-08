//go:build !darwin && !linux

package tmux

import (
	"fmt"
	"os"
)

func captureProcessIdentity(pid int) (ProcessIdentity, error) {
	startToken, err := processStartIdentityFn(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{PID: pid, StartToken: startToken}, nil
}

func signalProcessIdentity(identity ProcessIdentity, _ os.Signal) (bool, error) {
	return false, fmt.Errorf("tmux: %w on this platform for pid %d", errProcessIdentitySignalUnsupported, identity.PID)
}

func processIdentityAlive(identity ProcessIdentity) (bool, error) {
	current, err := processStartIdentityFn(identity.PID)
	return err == nil && current == identity.StartToken, err
}

func readProcessStartIdentity(pid int) (string, error) {
	return "", fmt.Errorf("kernel process start identity is unsupported for pid %d on this platform", pid)
}
