//go:build !windows

package main

import (
	"os/signal"
	"syscall"
)

func ignoreBrokenPipeSignal() {
	// Convert a closed stdout pipe into an EPIPE write error so main can treat
	// an early-closing consumer (for example `sheets ... | head`) as success.
	signal.Ignore(syscall.SIGPIPE)
}
