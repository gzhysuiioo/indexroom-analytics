//go:build !windows && !plan9 && !js && !wasip1

package main

import (
	"os"
	"syscall"
	"testing"
)

// sigpipePlatform reports whether this build has SIGPIPE semantics: a write
// to a broken standard output would, by default, kill the process with
// SIGPIPE instead of returning EPIPE. On these platforms the delivery tests
// also assert the process survives the broken pipe and that the underlying
// "broken pipe" error text reaches the diagnostic.
const sigpipePlatform = true

// assertRegisterNotSignaled verifies the register process finished by
// exiting, not by being killed by a signal — in particular SIGPIPE, whose
// default disposition would deny the caller the contracted exit status and
// stderr diagnostic when the result pipe breaks.
func assertRegisterNotSignaled(t *testing.T, state *os.ProcessState) {
	t.Helper()
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		t.Fatalf("register process killed by signal %v instead of reporting the delivery failure via exit status and stderr", ws.Signal())
	}
}
