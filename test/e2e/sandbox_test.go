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

func TestGdriveFsE2E(t *testing.T) {
	token := os.Getenv("HIVE_GDRIVE_ACCESS_TOKEN")
	if token == "" {
		t.Skip("set HIVE_GDRIVE_ACCESS_TOKEN [+ HIVE_GDRIVE_FOLDER_ID] to run")
	}
	runFixtureE2E(t, "agent-gdrive-fs", func(sp *spec.Spec) {
		sp.FS.GdriveAccessToken = token
		sp.FS.GdriveRefreshToken = os.Getenv("HIVE_GDRIVE_REFRESH_TOKEN")
		sp.FS.GdriveClientID = os.Getenv("HIVE_GDRIVE_CLIENT_ID")
		sp.FS.GdriveClientSecret = os.Getenv("HIVE_GDRIVE_CLIENT_SECRET")
		sp.FS.GdriveFolderID = os.Getenv("HIVE_GDRIVE_FOLDER_ID")
	})
}
