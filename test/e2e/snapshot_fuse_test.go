package e2e_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestSnapshotFuseE2E verifies that a snapshot can be captured to and restored
// from a FUSE drive instead of the sandbox host's local snapshot directory,
// using an `internal` file system as the snapshot target.
//
// The drive is an `external`-backed file system mounted at /snapshot-drive and
// marked internal: sandboxd mounts it host-side (so it can read/write the
// tarball) but never exports it into the agent, so the agent can't see
// /snapshot-drive. snapshot.mount points the snapshot machinery at /snapshot-drive, so
// the tarball is written through sbxfuse to the in-process external host and
// read back from it on the next boot.
//
// The same in-memory external host backs both phases, standing in for a remote
// object store that outlives any single sandbox:
//
//  1. A sandbox writes a file to /workspace and shuts down. sandboxd captures
//     /workspace into a tarball at /snapshot-drive/snapshot-<key>.tar.gz, which
//     sbxfuse persists to the external host. The agent is asserted not to see
//     /snapshot-drive.
//
//  2. A fresh sandbox restores from the same key. The tarball is fetched from
//     the external host through the FUSE drive and /workspace is restored.
func TestSnapshotFuseE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	// One external host backs both phases; its in-memory store keeps the
	// captured tarball between the write and restore sandboxes.
	stop := setup.StartExternalFSHost(t)
	defer stop()

	host := fmt.Sprintf("http://external-fs:%d", setup.ExternalFSPort)
	key := fmt.Sprintf("snapfuse%d", time.Now().UnixNano())
	file := "/workspace/note.txt"
	content := "hello-from-fuse-snapshot"

	// snapshotDrive is the internal, remote-backed file system used purely as the
	// snapshot target. Shared by both phases.
	// No ACLs: the mount is internal, so the agent can't reach it and an ACL
	// policy is moot — sandboxd gets full access for snapshot I/O regardless.
	snapshotDrive := hiverclient.FileSystem{
		Mount:    "/snapshot-drive",
		Backend:  "external",
		Internal: true,
		Host:     host,
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	wKey := "w" + key
	t.Cleanup(func() { _ = c.Shutdown(context.Background(), wKey) })

	writer, err := c.GetOrCreateSandbox(ctx, wKey, hiverclient.SandboxConfig{
		Image:      "hiversh/node:alpine",
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		ExtraHosts: []string{"external-fs:host-gateway"},
		FS:         []hiverclient.FileSystem{snapshotDrive},
		Snapshot: &hiverclient.Snapshot{
			WriteKey: key,
			Mount:    "/snapshot-drive",
			Include:  []string{"/workspace/**"},
		},
	})
	if err != nil {
		t.Fatalf("write: GetOrCreateSandbox: %v", err)
	}

	res, err := writer.Exec(ctx, hiverclient.ExecRequest{
		Command: fmt.Sprintf("mkdir -p /workspace && echo %s > %s", content, file),
	})
	if err != nil {
		t.Fatalf("write: Exec echo: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("write: echo failed (exit=%d stderr=%q)", res.ExitCode, res.Stderr)
	}

	// The internal mount must be invisible to the agent.
	res, err = writer.Exec(ctx, hiverclient.ExecRequest{Command: "test -e /snapshot-drive"})
	if err != nil {
		t.Fatalf("write: Exec test -e /snapshot-drive: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("internal mount /snapshot-drive must not be visible to the agent")
	}

	if err := c.Shutdown(ctx, wKey); err != nil {
		t.Fatalf("write: Shutdown: %v", err)
	}

	// The snapshot must actually have landed on the external host — assert it
	// directly against the host's own /v1/list contract, not just implicitly via
	// the restore below. The tarball is keyed snapshot-<writeKey>.tar.gz under the
	// mount, which the (prefix-less) external backend stores at that path.
	wantTarball := fmt.Sprintf("snapshot-%s.tar.gz", key)
	if !externalHostHasPath(t, wantTarball) {
		t.Fatalf("external host never received the snapshot %q; stored paths: %v",
			wantTarball, externalHostPaths(t))
	}

	// Pull the tarball back off the host and inspect it: it must be a real
	// gzip-tar carrying the snapshotted /workspace tree (entries are rooted at
	// the container-absolute path without the leading slash), with note.txt
	// holding the bytes we wrote — not an empty or truncated archive.
	entries := inspectSnapshotTarball(t, wantTarball)
	t.Logf("snapshot tarball %s entries: %v", wantTarball, tarEntryNames(entries))
	if got := strings.TrimSpace(entries["workspace/note.txt"]); got != content {
		t.Errorf("snapshot tarball workspace/note.txt = %q, want %q (entries: %v)",
			got, content, tarEntryNames(entries))
	}

	rKey := "r" + key
	t.Cleanup(func() { _ = c.Shutdown(context.Background(), rKey) })

	reader, err := c.GetOrCreateSandbox(ctx, rKey, hiverclient.SandboxConfig{
		Image:      "hiversh/node:alpine",
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		ExtraHosts: []string{"external-fs:host-gateway"},
		FS:         []hiverclient.FileSystem{snapshotDrive},
		Snapshot: &hiverclient.Snapshot{
			RestoreKey: key,
			Mount:      "/snapshot-drive",
		},
	})
	if err != nil {
		t.Fatalf("restore: GetOrCreateSandbox: %v", err)
	}

	res, err = reader.Exec(ctx, hiverclient.ExecRequest{Command: "cat " + file})
	if err != nil {
		t.Fatalf("restore: Exec cat: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("restore: cat %s: exit=%d stderr=%q", file, res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != content {
		t.Errorf("restored content: got %q, want %q", got, content)
	}
}

// externalHostPaths queries the in-process external FS host (StartExternalFSHost)
// over its own /v1/list contract and returns every stored path. The host binds
// all interfaces on ExternalFSPort, so the test process reaches it on loopback.
func externalHostPaths(t *testing.T) []string {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/list?prefix=/", setup.ExternalFSPort)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("external-fs list: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("external-fs list decode: %v", err)
	}
	return out.Paths
}

// externalHostHasPath reports whether the external host stores a path containing
// want, polling briefly so a snapshot upload that lands just after the shutdown
// RPC returns isn't flagged as missing.
func externalHostHasPath(t *testing.T, want string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, p := range externalHostPaths(t) {
			if strings.Contains(p, want) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// inspectSnapshotTarball fetches the tarball whose stored path contains want off
// the external host (/v1/file) and returns its regular-file entries as
// name → content, decoding the gzip-tar the snapshot package produces.
func inspectSnapshotTarball(t *testing.T, want string) map[string]string {
	t.Helper()
	var stored string
	for _, p := range externalHostPaths(t) {
		if strings.Contains(p, want) {
			stored = p
			break
		}
	}
	if stored == "" {
		t.Fatalf("snapshot %q not on external host", want)
	}

	u := fmt.Sprintf("http://127.0.0.1:%d/v1/file?path=%s",
		setup.ExternalFSPort, url.QueryEscape(stored))
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("external-fs get %s: %v", stored, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("external-fs get %s: status %d", stored, resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("snapshot %s: not gzip: %v", stored, err)
	}
	defer gz.Close()

	entries := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("snapshot %s: read tar: %v", stored, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("snapshot %s: read entry %s: %v", stored, hdr.Name, err)
			}
			entries[hdr.Name] = string(b)
		} else {
			entries[hdr.Name] = ""
		}
	}
	return entries
}

func tarEntryNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
