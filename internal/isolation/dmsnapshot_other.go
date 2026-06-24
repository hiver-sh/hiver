//go:build !linux

package isolation

import "errors"

// Stubs so the package compiles off Linux (e.g. for darwin tooling/tests). The
// microvm backend only runs on Linux; these never execute there.
var errDMUnsupported = errors.New("dm-snapshot overlay is only supported on linux")

func loopAttach(string, bool) (string, error)                        { return "", errDMUnsupported }
func loopMountExt4(string, string) (string, error)                   { return "", errDMUnsupported }
func loopUnmount(string, string)                                     {}
func loopDetach(string) error                                        { return errDMUnsupported }
func loopFindByBacking(string) []string                              { return nil }
func dmCreateSnapshot(string, string, string, int64) (string, error) { return "", errDMUnsupported }
func dmRemove(string) error                                          { return errDMUnsupported }
