//go:build windows || plan9 || js || wasip1

package main

import (
	"os"
	"testing"
)

// sigpipePlatform is false on platforms without SIGPIPE: a failed write
// surfaces as an ordinary error there already, so the process-level delivery
// tests skip the signal and "broken pipe" text assertions.
const sigpipePlatform = false

// assertRegisterNotSignaled is a no-op on platforms without SIGPIPE.
func assertRegisterNotSignaled(t *testing.T, state *os.ProcessState) {
	t.Helper()
}
