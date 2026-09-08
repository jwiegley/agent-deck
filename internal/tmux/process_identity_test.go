package tmux

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func stubProcessReapIO(t *testing.T, read func(int) (string, error), signal func(ProcessIdentity, os.Signal) (bool, error)) {
	t.Helper()
	oldRead := processStartIdentityFn
	oldCapture := processCaptureIdentityFn
	oldSignal := processSignalIdentityFn
	oldAlive := processIdentityAliveFn
	t.Cleanup(func() {
		processStartIdentityFn = oldRead
		processCaptureIdentityFn = oldCapture
		processSignalIdentityFn = oldSignal
		processIdentityAliveFn = oldAlive
	})
	processStartIdentityFn = read
	processCaptureIdentityFn = func(pid int) (ProcessIdentity, error) {
		token, err := read(pid)
		if err != nil {
			return ProcessIdentity{}, err
		}
		return ProcessIdentity{PID: pid, StartToken: token}, nil
	}
	processSignalIdentityFn = signal
	processIdentityAliveFn = func(identity ProcessIdentity) (bool, error) {
		current, err := read(identity.PID)
		return err == nil && current == identity.StartToken, err
	}
}

func TestRuntimeLifecycle_ReapProcessIdentitiesPIDReuseBeforeTERM(t *testing.T) {
	reads := 0
	var signals []os.Signal
	read := func(int) (string, error) {
		reads++
		if reads == 1 {
			return "birth-a", nil
		}
		return "birth-b", nil
	}
	stubProcessReapIO(t, read, func(identity ProcessIdentity, signal os.Signal) (bool, error) {
		if !ProcessIdentityMatches(identity) {
			return false, nil
		}
		signals = append(signals, signal)
		return true, nil
	})

	identity, err := CaptureProcessIdentity(4242)
	if err != nil {
		t.Fatal(err)
	}
	ReapProcessIdentities([]ProcessIdentity{identity}, ProcessReapTiming{})
	if len(signals) != 0 {
		t.Fatalf("reused pid received signals %v before TERM", signals)
	}
	if reads != 2 {
		t.Fatalf("identity reads = %d, want capture plus pre-TERM revalidation", reads)
	}
}

func TestRuntimeLifecycle_ReapProcessIdentitiesPIDReuseBeforeKILL(t *testing.T) {
	reads := 0
	var signals []os.Signal
	read := func(int) (string, error) {
		reads++
		if reads <= 2 {
			return "birth-a", nil
		}
		return "birth-b", nil
	}
	stubProcessReapIO(t, read, func(identity ProcessIdentity, signal os.Signal) (bool, error) {
		if !ProcessIdentityMatches(identity) {
			return false, nil
		}
		signals = append(signals, signal)
		return true, nil
	})

	identity, err := CaptureProcessIdentity(4242)
	if err != nil {
		t.Fatal(err)
	}
	ReapProcessIdentities([]ProcessIdentity{identity}, ProcessReapTiming{})
	want := []os.Signal{syscall.SIGTERM}
	if !reflect.DeepEqual(signals, want) {
		t.Fatalf("signals = %v, want TERM only after PID reuse before KILL", signals)
	}
	if reads != 3 {
		t.Fatalf("identity reads = %d, want capture plus TERM/KILL revalidations", reads)
	}
}

func TestRuntimeLifecycle_ReapProcessIdentitiesDelegatesFullIdentityForEachSignal(t *testing.T) {
	var identities []ProcessIdentity
	var signals []os.Signal
	stubProcessReapIO(t, func(pid int) (string, error) {
		return "birth-a", nil
	}, func(identity ProcessIdentity, signal os.Signal) (bool, error) {
		identities = append(identities, identity)
		signals = append(signals, signal)
		return true, nil
	})

	identity, err := CaptureProcessIdentity(4242)
	if err != nil {
		t.Fatal(err)
	}
	ReapProcessIdentities([]ProcessIdentity{identity}, ProcessReapTiming{})
	wantIdentities := []ProcessIdentity{identity, identity}
	if !reflect.DeepEqual(identities, wantIdentities) {
		t.Fatalf("signal identities = %#v, want %#v", identities, wantIdentities)
	}
	wantSignals := []os.Signal{syscall.SIGTERM, syscall.SIGKILL}
	if !reflect.DeepEqual(signals, wantSignals) {
		t.Fatalf("signals = %#v, want %#v", signals, wantSignals)
	}
}

func TestRuntimeLifecycle_ReapProcessIdentitiesReadFailureFailsClosed(t *testing.T) {
	var signals []os.Signal
	read := func(int) (string, error) {
		return "", errors.New("kernel identity unavailable")
	}
	stubProcessReapIO(t, read, func(identity ProcessIdentity, signal os.Signal) (bool, error) {
		if !ProcessIdentityMatches(identity) {
			return false, nil
		}
		signals = append(signals, signal)
		return true, nil
	})

	ReapProcessIdentities([]ProcessIdentity{{PID: 4242, StartToken: "birth-a"}}, ProcessReapTiming{})
	if len(signals) != 0 {
		t.Fatalf("identity read failure fell back to raw signals %v", signals)
	}
}

func TestRuntimeLifecycle_ReapProcessIdentitiesUnsupportedSignalFailsClosed(t *testing.T) {
	calls := 0
	buf := &bytes.Buffer{}
	originalLog := respawnLog
	respawnLog = slog.New(slog.NewJSONHandler(buf, nil))
	t.Cleanup(func() { respawnLog = originalLog })
	stubProcessReapIO(t, func(int) (string, error) {
		return "birth-a", nil
	}, func(ProcessIdentity, os.Signal) (bool, error) {
		calls++
		return false, errProcessIdentitySignalUnsupported
	})

	ReapProcessIdentities([]ProcessIdentity{
		{PID: 4242, StartToken: "birth-a"},
		{PID: 4343, StartToken: "birth-b"},
	}, ProcessReapTiming{})
	if calls != 2 {
		t.Fatalf("unsupported signal calls = %d, want one TERM attempt per process and no KILL fallback", calls)
	}
	if got := strings.Count(buf.String(), `"msg":"process_identity_signal_unsupported"`); got != 1 {
		t.Fatalf("unsupported-signal warnings = %d, want exactly one per reap; log=%s", got, buf.String())
	}
}

func TestRuntimeLifecycle_ReapProcessIdentitiesClosesSharedHandleExactlyOnce(t *testing.T) {
	closes := 0
	identity := ProcessIdentity{
		PID: 4242, StartToken: "birth-a",
		handle: &processIdentityHandle{closeFn: func() error {
			closes++
			return nil
		}},
	}
	stubProcessReapIO(t, func(int) (string, error) {
		return "birth-a", nil
	}, func(ProcessIdentity, os.Signal) (bool, error) {
		return false, errProcessIdentitySignalUnsupported
	})

	ReapProcessIdentities([]ProcessIdentity{identity, identity}, ProcessReapTiming{})
	if closes != 1 {
		t.Fatalf("shared retained handle closes = %d, want exactly one", closes)
	}
	if err := identity.Close(); err != nil || closes != 1 {
		t.Fatalf("idempotent close error=%v closes=%d", err, closes)
	}
}

func TestRuntimeLifecycle_CaptureProcessIdentitiesOmitsGoneButSurfacesIndeterminate(t *testing.T) {
	stubProcessReapIO(t, func(pid int) (string, error) {
		switch pid {
		case 1:
			return "", errProcessIdentityNotFound
		case 2:
			return "birth-two", nil
		default:
			return "", errors.New("permission denied")
		}
	}, func(ProcessIdentity, os.Signal) (bool, error) { return true, nil })

	identities, err := CaptureProcessIdentities([]int{1, 2})
	if err != nil || !reflect.DeepEqual(identities, []ProcessIdentity{{PID: 2, StartToken: "birth-two"}}) {
		t.Fatalf("gone-process capture = %#v, err=%v", identities, err)
	}
	if _, err := CaptureProcessIdentities([]int{2, 3}); err == nil {
		t.Fatal("indeterminate identity capture was silently omitted")
	}
}

func TestRuntimeLifecycle_IsProcessIdentityNotFoundDistinguishesIndeterminateFailures(t *testing.T) {
	if !IsProcessIdentityNotFound(errors.Join(errors.New("capture failed"), errProcessIdentityNotFound)) {
		t.Fatal("wrapped process-not-found error was not recognized")
	}
	if IsProcessIdentityNotFound(errors.New("permission denied")) {
		t.Fatal("indeterminate capture error was classified as process-not-found")
	}
}

func TestRuntimeLifecycle_CaptureStableProcessTreeAllowsFullyExitedTree(t *testing.T) {
	stubProcessReapIO(t, func(int) (string, error) {
		return "", errProcessIdentityNotFound
	}, func(ProcessIdentity, os.Signal) (bool, error) { return true, nil })
	probes := 0
	identities, err := captureStableProcessTree(func() ([]int, error) {
		probes++
		return []int{4242, 4343}, nil
	}, 4242, ErrRuntimeGenerationCandidateChanged)
	if err != nil || len(identities) != 0 || probes != 2 {
		t.Fatalf("exited tree capture = %#v, probes=%d, err=%v", identities, probes, err)
	}
}

func TestRuntimeLifecycle_CaptureStableProcessTreeRejectsPartialExit(t *testing.T) {
	stubProcessReapIO(t, func(pid int) (string, error) {
		if pid == 4242 {
			return "birth", nil
		}
		return "", errProcessIdentityNotFound
	}, func(ProcessIdentity, os.Signal) (bool, error) { return true, nil })
	_, err := captureStableProcessTree(func() ([]int, error) {
		return []int{4242, 4343}, nil
	}, 4242, ErrRuntimeGenerationCandidateChanged)
	if !errors.Is(err, ErrRuntimeGenerationCandidateChanged) {
		t.Fatalf("partial tree error = %v, want candidate changed", err)
	}
}
