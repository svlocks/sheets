//go:build !windows

package main

import (
	"os"
	"syscall"
	"testing"
)

func TestSignalExitCode(t *testing.T) {
	tests := []struct {
		signal os.Signal
		want   int
	}{
		{signal: os.Interrupt, want: 130},
		{signal: syscall.SIGTERM, want: 143},
	}
	for _, test := range tests {
		if got := signalExitCode(test.signal); got != test.want {
			t.Errorf("signalExitCode(%v) = %d, want %d", test.signal, got, test.want)
		}
	}
}
