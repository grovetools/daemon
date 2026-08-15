package cmd

import (
	"os"
	"syscall"
	"testing"
)

func TestDaemonSignalName(t *testing.T) {
	for sig, want := range map[os.Signal]string{
		os.Interrupt:    "SIGINT",
		syscall.SIGTERM: "SIGTERM",
		syscall.SIGUSR1: "SIGUSR1",
	} {
		if got := daemonSignalName(sig); got != want {
			t.Fatalf("daemonSignalName(%v) = %q, want %q", sig, got, want)
		}
	}
}
