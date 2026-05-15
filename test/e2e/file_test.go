package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestFileE2E exercises `POST /v1/file` and `GET /v1/file` end-to-end
// against the mcp-server fixture. The fixture configures `/scratch`
// as a local backend, which is the natural target — uploads write to
// the host-side backend directory (`/scratch-backend`) and reads
// stream from the same place, so the round-trip is observable via
// both the API itself and the agent's MCP `read` tool through the
// FUSE mount.
func TestFileE2E(t *testing.T) {
	pod, session, ctx, cancel := startMcpFixture(t)
	defer pod.stop()
	defer cancel()
	defer session.Close()

	const apiURL = "http://127.0.0.1:8080"

	t.Run("upload_then_download_roundtrips", func(t *testing.T) {
		body := []byte("hello from POST /v1/file")
		resp := uploadFile(t, apiURL, "/scratch", "greeting.txt", body)
		if resp.Path != "/scratch/greeting.txt" {
			t.Errorf("path=%q, want /scratch/greeting.txt", resp.Path)
		}
		if resp.Bytes != int64(len(body)) {
			t.Errorf("bytes=%d, want %d", resp.Bytes, len(body))
		}

		// The agent must see the same bytes via the FUSE mount — that
		// proves the upload landed in the backend dir (not some host-
		// side scratch we never connected to the mount).
		got := mcpReadFile(t, ctx, session, "/scratch/greeting.txt")
		if got != string(body) {
			t.Errorf("agent read content = %q, want %q", got, string(body))
		}

		// And GET /v1/file returns the same bytes.
		downloaded, ct, cd := downloadFile(t, apiURL, "/scratch/greeting.txt")
		if !bytes.Equal(downloaded, body) {
			t.Errorf("GET body = %q, want %q", downloaded, body)
		}
		if ct != "application/octet-stream" {
			t.Errorf("Content-Type=%q, want application/octet-stream", ct)
		}
		if !strings.Contains(cd, `filename="greeting.txt"`) {
			t.Errorf("Content-Disposition=%q, want filename=greeting.txt", cd)
		}
	})

	t.Run("download_subpath_written_by_agent", func(t *testing.T) {
		// Write a nested file via the agent (i.e. through FUSE). GET
		// /v1/file should be able to fetch it back even though the
		// upload endpoint only writes at the mount root: GET supports
		// arbitrary subpaths under any configured mount.
		var w struct{ Bytes int }
		callMCP(t, ctx, session, "write", map[string]any{
			"path":    "/scratch/nested/note.md",
			"content": "from agent\n",
		}, &w)
		if w.Bytes == 0 {
			t.Fatalf("agent write returned bytes=0")
		}

		got, _, _ := downloadFile(t, apiURL, "/scratch/nested/note.md")
		if string(got) != "from agent\n" {
			t.Errorf("GET body = %q, want %q", got, "from agent\n")
		}
	})

	t.Run("upload_bypasses_fs_acls", func(t *testing.T) {
		// Lock down /scratch/locked/** via the public config API: the
		// agent can't write there.
		initial := getConfig(t, apiURL)
		defer func() {
			if r := putConfig(t, apiURL, initial); !r.Applied {
				t.Errorf("restore config: applied=false (error=%q)", derefString(r.Error))
			}
		}()

		denied := withScratchLockedDeny(initial)
		if r := putConfig(t, apiURL, denied); !r.Applied {
			t.Fatalf("apply deny: applied=false (error=%q)", derefString(r.Error))
		}
		// Wait for the reconciler to SIGHUP sbxfuse before asserting
		// the agent is locked out.
		eventuallyAgentBashFails(t, ctx, session,
			"mkdir -p /scratch/locked && touch /scratch/locked/probe.txt",
			5*time.Second)

		// Even though the agent is blocked, /v1/file writes through
		// the backend dir and lands the file successfully. The upload
		// endpoint only writes at the mount root, so the file goes to
		// /scratch/keyed.bin — not under locked/. The point is that
		// the FUSE policy isn't even consulted for the upload.
		body := []byte("escape hatch")
		resp := uploadFile(t, apiURL, "/scratch", "keyed.bin", body)
		if resp.Bytes != int64(len(body)) {
			t.Errorf("bytes=%d, want %d", resp.Bytes, len(body))
		}

		// Verify the file is on disk by reading it back through the
		// control plane (which also bypasses ACLs).
		got, _, _ := downloadFile(t, apiURL, "/scratch/keyed.bin")
		if !bytes.Equal(got, body) {
			t.Errorf("GET body = %q, want %q", got, body)
		}
	})

	t.Run("upload_unknown_mount_404", func(t *testing.T) {
		status, body := uploadStatus(t, apiURL, "/does-not-exist", "x.bin", []byte("x"))
		if status != http.StatusNotFound {
			t.Errorf("status=%d, want 404 (body=%s)", status, body)
		}
	})

	t.Run("upload_missing_destination_400", func(t *testing.T) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, err := mw.CreateFormFile("file", "x.bin")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		fw.Write([]byte("x"))
		mw.Close()
		req, err := http.NewRequest(http.MethodPost, apiURL+"/v1/file", &body)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status=%d, want 400 (body=%s)", resp.StatusCode, b)
		}
	})

	t.Run("download_missing_path_query_400", func(t *testing.T) {
		resp, err := http.Get(apiURL + "/v1/file")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status=%d, want 400 (body=%s)", resp.StatusCode, b)
		}
	})

	t.Run("download_unknown_mount_404", func(t *testing.T) {
		status, _ := downloadStatus(t, apiURL, "/does-not-exist/foo")
		if status != http.StatusNotFound {
			t.Errorf("status=%d, want 404", status)
		}
	})

	t.Run("download_missing_file_404", func(t *testing.T) {
		status, _ := downloadStatus(t, apiURL, "/scratch/never-uploaded.bin")
		if status != http.StatusNotFound {
			t.Errorf("status=%d, want 404", status)
		}
	})
}

// uploadFileResponse mirrors the inline schema in
// `POST /v1/file`'s 200 response. We don't have a generated type for
// it (the spec doesn't name it) so a local struct is the cleanest way
// to decode.
type uploadFileResponse struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// uploadFile POSTs a multipart/form-data request to /v1/file. The
// response is decoded into uploadFileResponse; a non-200 status fails
// the test.
func uploadFile(t *testing.T, baseURL, destination, filename string, content []byte) uploadFileResponse {
	t.Helper()
	status, body := uploadStatus(t, baseURL, destination, filename, content)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/file: status %d, body %s", status, body)
	}
	var out uploadFileResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	return out
}

// uploadStatus is the raw form of uploadFile: it returns the HTTP
// status and body without asserting success, for the error-path
// tests.
func uploadStatus(t *testing.T, baseURL, destination, filename string, content []byte) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("destination", destination); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/file", &body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// downloadFile GETs /v1/file?path=… and returns the body plus the
// observed Content-Type / Content-Disposition headers. Non-200
// statuses fail the test.
func downloadFile(t *testing.T, baseURL, path string) (body []byte, contentType, contentDisposition string) {
	t.Helper()
	status, body, ct, cd := downloadResponse(t, baseURL, path)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/file?path=%s: status %d, body %s", path, status, body)
	}
	return body, ct, cd
}

func downloadStatus(t *testing.T, baseURL, path string) (int, []byte) {
	t.Helper()
	status, body, _, _ := downloadResponse(t, baseURL, path)
	return status, body
}

func downloadResponse(t *testing.T, baseURL, path string) (int, []byte, string, string) {
	t.Helper()
	u := baseURL + "/v1/file?path=" + url.QueryEscape(path)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition")
}

// mcpReadFile calls the agent's MCP `read` tool and returns the file
// contents. Used to assert that an /v1/file upload is observable via
// the FUSE mount from inside the sandbox.
func mcpReadFile(t *testing.T, ctx context.Context, session *mcp.ClientSession, path string) string {
	t.Helper()
	var out struct {
		Content   string
		LineCount int
		Truncated bool
	}
	callMCP(t, ctx, session, "read", map[string]any{"path": path}, &out)
	return out.Content
}
