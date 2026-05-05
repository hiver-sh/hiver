package e2e_test

import (
	"os"
	"testing"

	"github.com/sandbox-platform/agent-sandbox/internal/spec"
)

func TestPythonSandboxE2E(t *testing.T) {
	runFixtureE2E(t, "agent-python")
}

func TestNodeSandboxE2E(t *testing.T) {
	runFixtureE2E(t, "agent-node")
}

// TestGdriveFsE2E exercises the gdrive backend end-to-end against a
// real Google Drive folder. Skipped unless the host has set
// HIVE_GDRIVE_ACCESS_TOKEN — auth tokens can't be checked in, so this
// test is opt-in. Set HIVE_GDRIVE_FOLDER_ID too to scope the workspace
// to a specific Drive folder (recommended; otherwise it lands in My
// Drive root).
//
// The fixture only runs write + read probes; the assertions live in
// the fixture's expectations.yaml.
func TestGdriveFsE2E(t *testing.T) {
	token := os.Getenv("HIVE_GDRIVE_ACCESS_TOKEN")
	if token == "" {
		t.Skip("set HIVE_GDRIVE_ACCESS_TOKEN [+ HIVE_GDRIVE_FOLDER_ID] to run")
	}
	runFixtureE2E(t, "agent-gdrive-fs", func(sp *spec.Spec) {
		sp.FS.AccessToken = token
		sp.FS.RefreshToken = os.Getenv("HIVE_GDRIVE_REFRESH_TOKEN")
		sp.FS.ClientID = os.Getenv("HIVE_GDRIVE_CLIENT_ID")
		sp.FS.ClientSecret = os.Getenv("HIVE_GDRIVE_CLIENT_SECRET")
		sp.FS.FolderID = os.Getenv("HIVE_GDRIVE_FOLDER_ID")
	})
}
