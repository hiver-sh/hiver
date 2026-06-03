package isolation

import "testing"

func TestContainerFilesHostPath(t *testing.T) {
	f := containerFiles{upperDir: "/mnt/scratch/upper"}
	mounts := []string{"/workspace", "/data"}
	cases := []struct {
		path string
		want string
	}{
		{"/workspace", "/workspace-backend"},
		{"/workspace/sub/file.txt", "/workspace-backend/sub/file.txt"},
		{"/data", "/data-backend"},
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
