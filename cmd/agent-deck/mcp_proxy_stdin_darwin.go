//go:build darwin

package main

import "golang.org/x/sys/unix"

func mcpProxyStdinClosed() bool {
	// poll reports readable buffered bytes before HUP on macOS. Kqueue's
	// EV_EOF reports the last writer closing even while bytes remain queued.
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	defer unix.Close(kq)
	changes := []unix.Kevent_t{{Ident: 0, Filter: unix.EVFILT_READ, Flags: unix.EV_ADD | unix.EV_ONESHOT}}
	events := make([]unix.Kevent_t, 1)
	zero := unix.Timespec{}
	n, err := unix.Kevent(kq, changes, events, &zero)
	return err == nil && n != 0 && events[0].Flags&unix.EV_EOF != 0
}
