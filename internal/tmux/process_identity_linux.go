//go:build linux

package tmux

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	pidfdOpenFn       = unix.PidfdOpen
	pidfdSendSignalFn = func(pidfd int, signal syscall.Signal) error {
		return unix.PidfdSendSignal(pidfd, signal, nil, 0)
	}
	pidfdCloseFn = unix.Close
)

// captureProcessIdentity opens the pidfd before reading /proc, then probes the
// same handle after the read. If the numeric PID was replaced in that window,
// the old handle reports ESRCH and capture fails even when the replacement has
// the same coarse procfs start tick.
func captureProcessIdentity(pid int) (ProcessIdentity, error) {
	pidfd, err := pidfdOpenFn(pid, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ProcessIdentity{}, errProcessIdentityNotFound
		}
		return ProcessIdentity{}, fmt.Errorf("open pidfd: %w", err)
	}
	send := pidfdSendSignalFn
	closeFD := pidfdCloseFn
	handle := &processIdentityHandle{
		signalFn: func(signal os.Signal) (bool, error) {
			unixSignal, ok := signal.(syscall.Signal)
			if !ok {
				return false, fmt.Errorf("tmux: unsupported process signal %T", signal)
			}
			if err := send(pidfd, unixSignal); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					return false, nil
				}
				return false, fmt.Errorf("tmux: pidfd signal process %d: %w", pid, err)
			}
			return true, nil
		},
		aliveFn: func() (bool, error) {
			if err := send(pidfd, 0); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					return false, nil
				}
				return false, fmt.Errorf("tmux: pidfd probe process %d: %w", pid, err)
			}
			return true, nil
		},
		closeFn: func() error { return closeFD(pidfd) },
	}

	startToken, err := processStartIdentityFn(pid)
	if err != nil {
		_ = handle.close()
		return ProcessIdentity{}, err
	}
	alive, err := handle.alive()
	if err != nil {
		_ = handle.close()
		return ProcessIdentity{}, err
	}
	if !alive {
		_ = handle.close()
		return ProcessIdentity{}, errProcessIdentityNotFound
	}
	return ProcessIdentity{PID: pid, StartToken: startToken, handle: handle}, nil
}

func signalProcessIdentity(identity ProcessIdentity, signal os.Signal) (bool, error) {
	if identity.handle == nil {
		return false, fmt.Errorf("tmux: Linux process identity for pid %d has no retained pidfd", identity.PID)
	}
	return identity.handle.signal(signal)
}

func processIdentityAlive(identity ProcessIdentity) (bool, error) {
	if identity.handle == nil {
		return false, fmt.Errorf("tmux: Linux process identity for pid %d has no retained pidfd", identity.PID)
	}
	return identity.handle.alive()
}

// readProcessStartIdentity returns field 22 (starttime) from proc_pid_stat.
// starttime is the process birth tick count since boot and does not change
// across exec. Parsing begins after the final ')' because comm may contain
// spaces and ')' characters.
func readProcessStartIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %v", errProcessIdentityNotFound, err)
		}
		return "", err
	}
	return parseLinuxProcessStartIdentity(data)
}

func parseLinuxProcessStartIdentity(data []byte) (string, error) {
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 || closeParen+1 >= len(data) {
		return "", fmt.Errorf("malformed proc pid stat")
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	// fields[0] is proc_pid_stat field 3 (state), so field 22 is index 19.
	if len(fields) <= 19 {
		return "", fmt.Errorf("proc pid stat has %d fields after comm", len(fields))
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid proc pid stat starttime %q: %w", fields[19], err)
	}
	return strconv.FormatUint(start, 10), nil
}
