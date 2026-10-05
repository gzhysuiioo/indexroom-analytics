//go:build windows || plan9 || js || wasip1

package main

// ignoreSigpipe is a no-op on platforms without SIGPIPE; failed writes surface
// as ordinary errors there already.
func ignoreSigpipe() {}
