//go:build !linux

package remotefs

import "net/http"

// markedHTTPClient on non-Linux is a no-op — SO_MARK is Linux-only and
// the FUSE workspace itself only mounts on Linux. Returning the default
// client keeps the package compiling on macOS for unit tests that
// don't exercise the remote backend.
func markedHTTPClient(_ int) *http.Client {
	return http.DefaultClient
}
