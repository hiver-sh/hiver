//go:build !linux

package runc

import "errors"

func MountOverlay() error  { return errors.New("overlayfs not supported on this platform") }
func UnmountOverlay() error { return nil }
