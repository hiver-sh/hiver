//go:build !linux

package isolation

import "errors"

// Stub so the package compiles off Linux (e.g. for darwin tooling/tests). The
// microvm backend only runs on Linux; cowOrCopy falls back to a full copy here.
var errReflinkUnsupported = errors.New("reflink is only supported on linux")

func reflinkFile(string, string) error { return errReflinkUnsupported }
