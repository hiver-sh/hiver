//go:build !linux

package uffd

import "errors"

// ErrUnsupported is returned on platforms without userfaultfd. The runtime only
// reaches this package on Linux (microVM isolation implies KVM); the stub exists
// so the tree still builds on a macOS dev machine.
var ErrUnsupported = errors.New("uffd: userfaultfd is only available on linux")

// Handler is a non-functional stand-in on non-Linux platforms.
type Handler struct{ sockPath string }

// Listen always fails off Linux.
func Listen(sockPath, memPath string, opts Options) (*Handler, error) { return nil, ErrUnsupported }

// SocketPath returns the socket this handler would have served.
func (h *Handler) SocketPath() string { return h.sockPath }

// Serve always fails off Linux.
func (h *Handler) Serve() error { return ErrUnsupported }

// Stats returns zero counters off Linux.
func (h *Handler) Stats() Stats { return Stats{} }

// Close is a no-op off Linux.
func (h *Handler) Close() error { return nil }
