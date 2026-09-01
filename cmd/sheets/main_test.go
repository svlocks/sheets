package main

import (
	"errors"
	"syscall"
	"testing"
)

func TestCommandExitCode(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		signalCode int32
		want       int
	}{
		{name: "success", want: 0},
		{name: "broken pipe", err: syscall.EPIPE, want: 0},
		{name: "failure", err: errors.New("failed"), want: 1},
		{name: "signal after graceful command return", signalCode: 130, want: 130},
		{name: "signal with command error", err: errors.New("canceled"), signalCode: 143, want: 143},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := commandExitCode(test.err, test.signalCode); got != test.want {
				t.Fatalf("commandExitCode(%v, %d) = %d, want %d", test.err, test.signalCode, got, test.want)
			}
		})
	}
}
