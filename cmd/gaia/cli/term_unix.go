//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// resizeSignal is delivered on a terminal size change.
var resizeSignal os.Signal = syscall.SIGWINCH

// Resized delivers on every SIGWINCH.
func (t fdTerminal) Resized() (<-chan struct{}, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, resizeSignal)
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sig:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out, func() { signal.Stop(sig); close(done) }
}
