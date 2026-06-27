//go:build !linux

package isolation

import "errors"

// Stubs so the package compiles off Linux (e.g. for darwin tooling/tests). The
// microvm backend only runs on Linux; these never execute there.
var errLoopUnsupported = errors.New("loop-mounting an ext4 image is only supported on linux")

func loopAttach(string, bool) (string, error)      { return "", errLoopUnsupported }
func loopMountExt4(string, string) (string, error) { return "", errLoopUnsupported }
func loopUnmount(string, string)                   {}
func loopDetach(string) error                      { return errLoopUnsupported }
