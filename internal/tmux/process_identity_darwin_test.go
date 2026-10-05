//go:build darwin

package tmux

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func darwinKinfoProcess(pid int32, sec int64, usec int32) unix.KinfoProc {
	var process unix.KinfoProc
	process.Proc.P_pid = pid
	process.Proc.P_starttime = unix.Timeval{Sec: sec, Usec: usec}
	return process
}

func TestRuntimeLifecycle_DarwinIdentitySignalFailsClosed(t *testing.T) {
	signaled, err := signalProcessIdentity(ProcessIdentity{PID: 4242, StartToken: "1725000000:123456"}, syscall.SIGTERM)
	if signaled || !errors.Is(err, errProcessIdentitySignalUnsupported) {
		t.Fatalf("signal result = %v, err=%v; want explicit unsupported failure", signaled, err)
	}
}

func TestRuntimeLifecycle_DarwinProcessStartIdentity(t *testing.T) {
	got, err := darwinProcessStartIdentity(4242, []unix.KinfoProc{
		darwinKinfoProcess(4242, 1_725_000_000, 123456),
	})
	if err != nil || got != "1725000000:123456" {
		t.Fatalf("start identity = %q, err=%v", got, err)
	}
}

func TestRuntimeLifecycle_DarwinProcessStartIdentityRejectsInvalidRows(t *testing.T) {
	for _, test := range []struct {
		name         string
		rows         []unix.KinfoProc
		wantNotFound bool
	}{
		{name: "empty", wantNotFound: true},
		{name: "multiple", rows: []unix.KinfoProc{darwinKinfoProcess(4242, 1, 0), darwinKinfoProcess(4242, 1, 0)}},
		{name: "wrong pid", rows: []unix.KinfoProc{darwinKinfoProcess(4343, 1, 0)}},
		{name: "zero seconds", rows: []unix.KinfoProc{darwinKinfoProcess(4242, 0, 1)}},
		{name: "negative seconds", rows: []unix.KinfoProc{darwinKinfoProcess(4242, -1, 0)}},
		{name: "negative microseconds", rows: []unix.KinfoProc{darwinKinfoProcess(4242, 1, -1)}},
		{name: "microseconds overflow", rows: []unix.KinfoProc{darwinKinfoProcess(4242, 1, 1_000_000)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := darwinProcessStartIdentity(4242, test.rows)
			if err == nil {
				t.Fatal("invalid Darwin process rows were accepted")
			}
			if test.wantNotFound != errors.Is(err, errProcessIdentityNotFound) {
				t.Fatalf("error = %v, want not-found=%v", err, test.wantNotFound)
			}
		})
	}
}
