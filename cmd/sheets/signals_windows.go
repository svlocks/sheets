//go:build windows

package main

import (
	"os"
	"syscall"
)

func ignoreBrokenPipeSignal() {}

func signalExitCode(received os.Signal) int {
	if value, ok := received.(syscall.Signal); ok {
		return 128 + int(value)
	}
	return 1
}
