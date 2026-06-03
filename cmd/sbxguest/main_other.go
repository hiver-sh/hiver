//go:build !linux

// sbxguest is the in-guest agent for the Firecracker microvm backend. It is
// Linux-only: it runs as the guest init and relies on Linux mount, pivot_root,
// and AF_VSOCK. This stub lets `go build ./...` succeed on other platforms.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "sbxguest is only supported on linux (it runs as the microvm guest init)")
	os.Exit(1)
}
