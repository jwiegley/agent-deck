//go:build linux

package tmux

import (
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func stubLinuxIdentitySignal(
	t *testing.T,
	read func(int) (string, error),
	open func(int, int) (int, error),
	send func(int, syscall.Signal) error,
	close func(int) error,
) {
	t.Helper()
	oldRead := processStartIdentityFn
	oldOpen := pidfdOpenFn
	oldSend := pidfdSendSignalFn
	oldClose := pidfdCloseFn
	t.Cleanup(func() {
		processStartIdentityFn = oldRead
		pidfdOpenFn = oldOpen
		pidfdSendSignalFn = oldSend
		pidfdCloseFn = oldClose
	})
	processStartIdentityFn = read
	pidfdOpenFn = open
	pidfdSendSignalFn = send
	pidfdCloseFn = close
}

func linuxStatForStartToken(comm, start string) []byte {
	// Fields after comm begin at field 3 (state). Supply fields 3 through 21,
	// then the requested field-22 starttime.
	fields := append([]string{"S"}, make([]string, 18)...)
	for index := 1; index < len(fields); index++ {
		fields[index] = "1"
	}
	fields = append(fields, start)
	return []byte("123 (" + comm + ") " + strings.Join(fields, " ") + "\n")
}

func TestRuntimeLifecycle_ParseLinuxProcessStartIdentityAcceptsKernelStatShape(t *testing.T) {
	for _, test := range []struct {
		name  string
		comm  string
		start string
	}{
		{name: "spaces and close paren in comm", comm: "agent ) worker", start: "987654321"},
		{name: "zero is a valid kernel tick value", comm: "early", start: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseLinuxProcessStartIdentity(linuxStatForStartToken(test.comm, test.start))
			if err != nil || got != test.start {
				t.Fatalf("start token = %q, err=%v; want %q", got, err, test.start)
			}
		})
	}
}

func TestRuntimeLifecycle_ParseLinuxProcessStartIdentityRejectsMalformedInput(t *testing.T) {
	for _, test := range []struct {
		name string
		stat []byte
	}{
		{name: "missing close paren", stat: []byte("123 (agent S 1 2 3")},
		{name: "short fields", stat: []byte("123 (agent) S 1 2")},
		{name: "non decimal", stat: linuxStatForStartToken("agent", "not-a-number")},
		{name: "overflow", stat: linuxStatForStartToken("agent", "18446744073709551616")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := parseLinuxProcessStartIdentity(test.stat); err == nil {
				t.Fatalf("malformed stat parsed as %q", got)
			}
		})
	}
}

func TestRuntimeLifecycle_LinuxCaptureRetainsOnePidfdForTERMKillAndLiveness(t *testing.T) {
	var events []string
	stubLinuxIdentitySignal(t,
		func(pid int) (string, error) {
			events = append(events, "read")
			return "birth-a", nil
		},
		func(pid, flags int) (int, error) {
			events = append(events, "open")
			if pid != 4242 || flags != 0 {
				t.Fatalf("PidfdOpen(%d, %d), want (4242, 0)", pid, flags)
			}
			return 17, nil
		},
		func(pidfd int, signal syscall.Signal) error {
			events = append(events, "signal:"+signal.String())
			if pidfd != 17 {
				t.Fatalf("PidfdSendSignal fd = %d, want 17", pidfd)
			}
			return nil
		},
		func(pidfd int) error {
			events = append(events, "close")
			if pidfd != 17 {
				t.Fatalf("Close(%d), want 17", pidfd)
			}
			return nil
		},
	)

	identity, err := CaptureProcessIdentity(4242)
	if err != nil {
		t.Fatal(err)
	}
	copyOfIdentity := identity
	if !ProcessIdentityMatches(copyOfIdentity) {
		t.Fatal("retained pidfd liveness probe reported dead")
	}
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		signaled, signalErr := signalProcessIdentity(identity, signal)
		if signalErr != nil || !signaled {
			t.Fatalf("signal %v result = %v, err=%v; want signaled", signal, signaled, signalErr)
		}
	}
	if err := copyOfIdentity.Close(); err != nil {
		t.Fatal(err)
	}
	if err := identity.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"open", "read", "signal:signal 0", "signal:signal 0", "signal:terminated", "signal:killed", "close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestRuntimeLifecycle_LinuxCaptureRejectsExitedHandleDespiteSameTickReplacement(t *testing.T) {
	sends := 0
	closes := 0
	stubLinuxIdentitySignal(t,
		// The numeric replacement deliberately has the exact same coarse
		// start token. The retained old handle, not this token, rejects it.
		func(int) (string, error) { return "same-tick", nil },
		func(int, int) (int, error) { return 17, nil },
		func(_ int, signal syscall.Signal) error {
			sends++
			if signal == 0 {
				return syscall.ESRCH
			}
			return nil
		},
		func(int) error {
			closes++
			return nil
		},
	)

	identity, err := CaptureProcessIdentity(4242)
	if identity.Valid() || !IsProcessIdentityNotFound(err) {
		t.Fatalf("capture = %#v, err=%v; want exited retained handle rejected", identity, err)
	}
	if sends != 1 || closes != 1 {
		t.Fatalf("pidfd probes=%d closes=%d, want one liveness probe and one close", sends, closes)
	}
}

func TestRuntimeLifecycle_LinuxCapturePidfdUnavailableHasNoRawPIDFallback(t *testing.T) {
	reads := 0
	sends := 0
	stubLinuxIdentitySignal(t,
		func(int) (string, error) {
			reads++
			return "birth-a", nil
		},
		func(int, int) (int, error) { return -1, syscall.ENOSYS },
		func(int, syscall.Signal) error {
			sends++
			return nil
		},
		func(int) error { return nil },
	)

	identity, err := CaptureProcessIdentity(4242)
	if err == nil || identity.Valid() {
		t.Fatalf("capture = %#v, err=%v; want explicit pidfd failure", identity, err)
	}
	if reads != 0 || sends != 0 {
		t.Fatalf("proc reads=%d pidfd sends=%d, want no fallback after open failure", reads, sends)
	}
}

func TestRuntimeLifecycle_LinuxCaptureIndeterminateProcReadClosesHandle(t *testing.T) {
	sends := 0
	closes := 0
	stubLinuxIdentitySignal(t,
		func(int) (string, error) { return "", errors.New("procfs unavailable") },
		func(int, int) (int, error) { return 17, nil },
		func(int, syscall.Signal) error {
			sends++
			return nil
		},
		func(int) error {
			closes++
			return nil
		},
	)

	identity, err := CaptureProcessIdentity(4242)
	if err == nil || identity.Valid() {
		t.Fatalf("capture = %#v, err=%v; want indeterminate failure", identity, err)
	}
	if sends != 0 || closes != 1 {
		t.Fatalf("pidfd sends=%d closes=%d, want no probe and one close", sends, closes)
	}
}

func TestRuntimeLifecycle_LinuxIdentityWithoutRetainedHandleNeverFallsBack(t *testing.T) {
	signaled, err := signalProcessIdentity(
		ProcessIdentity{PID: 4242, StartToken: "birth-a"}, syscall.SIGKILL)
	if err == nil || signaled {
		t.Fatalf("signal result = %v, err=%v; want missing-handle failure", signaled, err)
	}
}

func TestRuntimeLifecycle_LinuxPartialCaptureClosesEveryOpenedPidfd(t *testing.T) {
	var closed []int
	stubLinuxIdentitySignal(t,
		func(pid int) (string, error) {
			if pid == 2 {
				return "", errors.New("procfs unavailable")
			}
			return "birth-one", nil
		},
		func(pid, _ int) (int, error) { return 10 + pid, nil },
		func(int, syscall.Signal) error { return nil },
		func(fd int) error {
			closed = append(closed, fd)
			return nil
		},
	)

	if identities, err := CaptureProcessIdentities([]int{1, 2}); err == nil || identities != nil {
		t.Fatalf("partial capture = %#v, err=%v; want failure", identities, err)
	}
	if want := []int{12, 11}; !reflect.DeepEqual(closed, want) {
		t.Fatalf("closed pidfds = %v, want %v", closed, want)
	}
}
