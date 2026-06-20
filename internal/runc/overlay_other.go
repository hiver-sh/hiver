//go:build !linux

package runc

import "errors"

func MountOverlay(Overlay) error   { return errors.New("overlayfs not supported on this platform") }
func UnmountOverlay(Overlay) error { return nil }
