package session

import (
	"errors"
	"syscall"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
)

type unsupportedOwnershipSignaler struct {
	rawCalls int
}

func (s *unsupportedOwnershipSignaler) Signal(int, syscall.Signal) error {
	s.rawCalls++
	return nil
}

func (*unsupportedOwnershipSignaler) Pin(int) (procowner.PinnedProcess, error) {
	return nil, procowner.ErrUnsupported
}

func TestOwnershipSignalerRejectsRawFallback(t *testing.T) {
	raw := &unsupportedOwnershipSignaler{}
	signaler := pinnedOwnershipSignaler{raw}
	if handle, err := signaler.Pin(123); handle != nil || err == nil || errors.Is(err, procowner.ErrUnsupported) {
		t.Fatalf("unsupported pin must refuse without permitting fallback: handle=%v err=%v", handle, err)
	}
	if err := signaler.Signal(123, syscall.SIGTERM); err == nil || raw.rawCalls != 0 {
		t.Fatalf("raw PID signal was permitted: err=%v calls=%d", err, raw.rawCalls)
	}
}
