package main

import (
	"time"

	"github.com/gen2brain/beeep"
)

// notifyTimeout is how long a notification may hold up exiting.
const notifyTimeout = 3 * time.Second

// notify pops up a system notification. It is a nicety, so it never reports a
// problem: errors, panics (a missing notification daemon, say) and a slow or
// hung notifier are all ignored.
func notify(title, message string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		_ = beeep.Notify(title, message, "")
	}()
	select {
	case <-done:
	case <-time.After(notifyTimeout):
	}
}
