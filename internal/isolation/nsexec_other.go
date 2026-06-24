//go:build !linux

package isolation

// MaybeRunNSExec is a no-op off Linux. The namespace-launch helper only runs inside
// the sandbox (Linux); this stub lets the command compile for darwin tooling/tests.
func MaybeRunNSExec() {}
