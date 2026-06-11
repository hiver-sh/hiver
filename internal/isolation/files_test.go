package isolation

import "testing"

func TestContainerFilesHostPath(t *testing.T) {
	f := containerFiles{upperDir: "/mnt/scratch/upper"}
	// /workspace is local-backed (reads the -backend buffer, ACL-bypass);
	// /data is remote-backed (reads the FUSE mount point so already-flushed
	// files the oplog evicted from the buffer are still visible).
	mounts := []MountRoute{
		{Mount: "/workspace", Remote: false},
		{Mount: "/data", Remote: true},
	}
	cases := []struct {
		path string
		want string
	}{
		{"/workspace", "/workspace-backend"},
		{"/workspace/sub/file.txt", "/workspace-backend/sub/file.txt"},
		// remote mount → mount point itself, no -backend suffix
		{"/data", "/data"},
		{"/data/sub/file.txt", "/data/sub/file.txt"},
		{"/home/user/notes.md", "/mnt/scratch/upper/home/user/notes.md"},
		{"/", "/mnt/scratch/upper"},
		// not a mount prefix despite the shared "/work" string
		{"/workspaces/x", "/mnt/scratch/upper/workspaces/x"},
	}
	for _, c := range cases {
		if got := f.hostPath(c.path, mounts); got != c.want {
			t.Errorf("hostPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
