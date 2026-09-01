//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func ignoreBrokenPipeSignal() {
	// Convert a closed stdout pipe into an EPIPE write error so main can treat
	// an early-closing consumer (for example `sheets ... | head`) as success.
	signal.Ignore(syscall.SIGPIPE)
}

func signalExitCode(received os.Signal) int {
	if value, ok := received.(syscall.Signal); ok {
		return 128 + int(value)
	}
	return 1
}
