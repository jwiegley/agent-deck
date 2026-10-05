package session

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
)

// Ownership receipts must not reopen the PID-reuse window that runtime teardown
// closes. Unsupported process handles leave the receipt available for recovery.
type pinnedOwnershipSignaler struct {
	signaler procowner.Signaler
}

func (pinnedOwnershipSignaler) Signal(int, syscall.Signal) error {
	return errors.New("ownership cleanup requires a retained process handle")
}

func (s pinnedOwnershipSignaler) Pin(pid int) (procowner.PinnedProcess, error) {
	pinned, ok := s.signaler.(procowner.PinnedSignaler)
	if !ok {
		return nil, errors.New("ownership process handles unavailable")
	}
	handle, err := pinned.Pin(pid)
	if errors.Is(err, procowner.ErrUnsupported) {
		// Do not wrap ErrUnsupported: Reap interprets it as permission to
		// fall back to signalling a reusable PID.
		return nil, fmt.Errorf("ownership process handles unavailable: %v", err)
	}
	return handle, err
}
