package e2e_test

import (
	"testing"
)

func TestPythonSandboxE2E(t *testing.T) {
	runFixtureE2E(t, "agent-python")
}

func TestNodeSandboxE2E(t *testing.T) {
	runFixtureE2E(t, "agent-node")
}
