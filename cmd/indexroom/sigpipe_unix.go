//go:build !windows && !plan9 && !js && !wasip1

package main

import (
	"os/signal"
	"syscall"
)

// ignoreSigpipe arranges for writes to a broken standard output to fail with
// EPIPE instead of killing the process. By default the runtime turns an EPIPE
// on file descriptor 1 or 2 into SIGPIPE and terminates the program (see the
// os/signal package documentation and os.epipecheck), which would deny the
// register command its promised exit status and stderr diagnostic when its
// result cannot be delivered. With SIGPIPE ignored the failed write surfaces as
// a regular error from json.Encoder. The register command is the last thing
// the process runs, so the process-wide setting never affects another command.
func ignoreSigpipe() {
	signal.Ignore(syscall.SIGPIPE)
}
