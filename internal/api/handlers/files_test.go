package handlers

import (
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
)

func fsMount(mount string, backend gen.Backend) gen.FileSystem {
	var fs gen.FileSystem
	switch backend {
	case gen.BackendGdrive:
		_ = fs.FromGDriveFileSystem(gen.GDriveFileSystem{Mount: mount, Backend: gen.Gdrive})
	case gen.BackendGcs:
		_ = fs.FromGCSFileSystem(gen.GCSFileSystem{Mount: mount, Backend: gen.Gcs})
	default:
		_ = fs.FromLocalFileSystem(gen.LocalFileSystem{Mount: mount, Backend: gen.Local})
	}
	return fs
}

func TestResolveMount(t *testing.T) {
	h := &Sandbox{}
	cfg := gen.SandboxConfig{Fs: []gen.FileSystem{
		fsMount("/workspace", gen.BackendLocal),
		fsMount("/workspace/drive", gen.BackendGdrive),
		fsMount("/data/", gen.BackendGcs), // trailing slash tolerated
	}}

	cases := []struct {
		name        string
		path        string
		wantMount   string
		wantBackend gen.Backend
		wantOK      bool
	}{
		{"file under workspace", "/workspace/test.txt", "/workspace", gen.BackendLocal, true},
		{"mount root itself", "/workspace", "/workspace", gen.BackendLocal, true},
		{"longest prefix wins", "/workspace/drive/report.csv", "/workspace/drive", gen.BackendGdrive, true},
		{"nested mount root", "/workspace/drive", "/workspace/drive", gen.BackendGdrive, true},
		{"trailing slash mount", "/data/x", "/data", gen.BackendGcs, true},
		{"prefix-but-not-child not matched", "/workspace-other/x", "", "", false},
		{"outside any mount", "/etc/passwd", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMount, gotBackend, gotOK := h.resolveMount(cfg, tc.path)
			if gotOK != tc.wantOK {
				t.Errorf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotMount != tc.wantMount {
				t.Errorf("mount = %q, want %q", gotMount, tc.wantMount)
			}
			if gotBackend != tc.wantBackend {
				t.Errorf("backend = %q, want %q", gotBackend, tc.wantBackend)
			}
		})
	}
}

// emitFSEvent must be a no-op (and not panic) when the handler has no broker,
// which is the case for lightweight/early-lifecycle handlers.
func TestEmitFSEventNilBroker(t *testing.T) {
	h := &Sandbox{}
	done := h.emitFSEvent(gen.SandboxConfig{}, gen.Write, "/workspace/x")
	done(nil) // must not panic
}

// emitFSEvent publishes a request/response pair only for paths under a
// configured mount; operations outside every mount publish nothing.
func TestEmitFSEventOnlyMountedPaths(t *testing.T) {
	b := events.New(0, 0)
	h := &Sandbox{broker: b}
	cfg := gen.SandboxConfig{Fs: []gen.FileSystem{fsMount("/workspace", gen.BackendLocal)}}

	count := func() int {
		replay, _, cancel := b.Subscribe(0)
		cancel()
		return len(replay)
	}

	// Path outside any mount: no events.
	h.emitFSEvent(cfg, gen.Write, "/etc/passwd")(nil)
	if got := count(); got != 0 {
		t.Fatalf("unmounted path published %d events, want 0", got)
	}

	// Path under /workspace: exactly the request + response pair.
	h.emitFSEvent(cfg, gen.Write, "/workspace/test.txt")(nil)
	if got := count(); got != 2 {
		t.Fatalf("mounted path published %d events, want 2", got)
	}
}
