package e2e_test

import (
	"testing"

	"github.com/hiver-sh/hiver/internal/spec"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

func TestPythonSandboxE2E(t *testing.T) {
	setup.RunFixtureE2E(t, "agent-python")
}

func TestNodeSandboxE2E(t *testing.T) {
	setup.RunFixtureE2E(t, "agent-node")
}

func TestGdriveFsE2E(t *testing.T) {
	token := setup.GetEnv("HIVE_GDRIVE_ACCESS_TOKEN")
	if token == "" {
		t.Skip("set HIVE_GDRIVE_ACCESS_TOKEN [+ HIVE_GDRIVE_FOLDER_ID] to run")
	}
	setup.RunFixtureE2E(t, "agent-gdrive-fs", func(sp *spec.Spec) {
		f := &sp.FS[0]
		f.GdriveAccessToken = token
		f.GdriveRefreshToken = setup.GetEnv("HIVE_GDRIVE_REFRESH_TOKEN")
		f.GdriveClientID = setup.GetEnv("HIVE_GDRIVE_CLIENT_ID")
		f.GdriveClientSecret = setup.GetEnv("HIVE_GDRIVE_CLIENT_SECRET")
		f.GdriveFolderID = setup.GetEnv("HIVE_GDRIVE_FOLDER_ID")
	})
}
