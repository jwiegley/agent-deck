//go:build darwin

package tmux

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func captureProcessIdentity(pid int) (ProcessIdentity, error) {
	startToken, err := processStartIdentityFn(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	handle := &processIdentityHandle{
		signalFn: func(os.Signal) (bool, error) {
			return false, fmt.Errorf("tmux: %w on darwin for pid %d", errProcessIdentitySignalUnsupported, pid)
		},
		aliveFn: func() (bool, error) {
			current, err := processStartIdentityFn(pid)
			return err == nil && current == startToken, err
		},
	}
	return ProcessIdentity{PID: pid, StartToken: startToken, handle: handle}, nil
}

// Darwin does not expose a supported signal primitive bound to the process
// start identity captured below. Fail closed rather than reintroducing a
// check-then-kill race through the numeric PID.
func signalProcessIdentity(identity ProcessIdentity, signal os.Signal) (bool, error) {
	return identity.handle.signal(signal)
}

func processIdentityAlive(identity ProcessIdentity) (bool, error) {
	return identity.handle.alive()
}

// readProcessStartIdentity returns the kernel's process start timeval. Unlike
// ps output, kern.proc.pid is a direct kernel record and survives exec.
func readProcessStartIdentity(pid int) (string, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	return darwinProcessStartIdentity(pid, processes)
}

func darwinProcessStartIdentity(pid int, processes []unix.KinfoProc) (string, error) {
	if len(processes) == 0 {
		return "", fmt.Errorf("%w: kern.proc.pid returned no process %d", errProcessIdentityNotFound, pid)
	}
	if len(processes) != 1 {
		return "", fmt.Errorf("kern.proc.pid returned %d processes for pid %d", len(processes), pid)
	}
	process := processes[0]
	if int(process.Proc.P_pid) != pid {
		return "", fmt.Errorf("kern.proc.pid returned pid %d for requested pid %d", process.Proc.P_pid, pid)
	}
	started := process.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 || started.Usec >= 1_000_000 {
		return "", fmt.Errorf("kern.proc.pid returned invalid start time %d:%d for %d", started.Sec, started.Usec, pid)
	}
	return fmt.Sprintf("%d:%d", started.Sec, started.Usec), nil
}
