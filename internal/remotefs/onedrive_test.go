package remotefs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeGraph is a minimal Microsoft Graph stand-in covering the endpoints the
// OneDrive Put path exercises: the drive-root probe, simple content upload,
// and resumable upload sessions (createUploadSession + chunked PUTs). It
// reassembles uploaded bytes per blob path so tests can assert round-trips.
type fakeGraph struct {
	base     string
	uploads  map[string][]byte // final content keyed by "/root:/<path>"
	sessions map[string][]byte // in-progress session buffers keyed by session id
	nextID   int
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		uploads:  map[string][]byte{},
		sessions: map[string][]byte{},
	}
}

func (f *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	// Drive root probe used by ensureFolder("/").
	case r.Method == http.MethodGet && p == "/me/drive/root":
		writeJSON(w, http.StatusOK, map[string]any{"id": "root", "name": "root", "folder": map[string]any{}})

	// Create an upload session; hand back an uploadUrl on this same server.
	case r.Method == http.MethodPost && strings.HasSuffix(p, ":/createUploadSession"):
		f.nextID++
		id := "sess" + strconv.Itoa(f.nextID)
		blob := strings.TrimSuffix(strings.TrimPrefix(p, "/me/drive"), ":/createUploadSession")
		f.sessions[id] = nil
		uploadURL := f.base + "/upload/" + id + "?blob=" + blob
		writeJSON(w, http.StatusOK, map[string]any{"uploadUrl": uploadURL})

	// Resumable chunk upload. Content-Range drives assembly; the final chunk
	// (end == total-1) returns the item, intermediate chunks return 202.
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/upload/"):
		id := strings.TrimPrefix(p, "/upload/")
		blob := r.URL.Query().Get("blob")
		start, end, total := parseContentRange(r.Header.Get("Content-Range"))
		body, _ := io.ReadAll(r.Body)
		if int64(len(body)) != end-start+1 {
			http.Error(w, "chunk length mismatch", http.StatusBadRequest)
			return
		}
		f.sessions[id] = append(f.sessions[id], body...)
		if end == total-1 {
			f.uploads[blob] = f.sessions[id]
			delete(f.sessions, id)
			writeJSON(w, http.StatusCreated, map[string]any{"id": "item", "name": "x"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{})

	// Simple content upload.
	case r.Method == http.MethodPut && strings.HasSuffix(p, ":/content"):
		blob := strings.TrimSuffix(strings.TrimPrefix(p, "/me/drive"), ":/content")
		body, _ := io.ReadAll(r.Body)
		f.uploads[blob] = body
		writeJSON(w, http.StatusCreated, map[string]any{"id": "item", "name": "x"})

	default:
		http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// parseContentRange parses "bytes start-end/total".
func parseContentRange(h string) (start, end, total int64) {
	h = strings.TrimPrefix(h, "bytes ")
	rng, tot, _ := strings.Cut(h, "/")
	s, e, _ := strings.Cut(rng, "-")
	start, _ = strconv.ParseInt(s, 10, 64)
	end, _ = strconv.ParseInt(e, 10, 64)
	total, _ = strconv.ParseInt(tot, 10, 64)
	return start, end, total
}

// newTestOneDrive wires an OneDrive at the fake Graph server with no auth.
func newTestOneDrive(base string, chunk int, simpleMax int64) *OneDrive {
	return &OneDrive{
		client:     http.DefaultClient,
		rawClient:  http.DefaultClient,
		driveRoot:  "/me/drive",
		graphBase:  base,
		chunkBytes: chunk,
		simpleMax:  simpleMax,
	}
}

func TestOneDrivePutSimpleVsSession(t *testing.T) {
	fg := newFakeGraph()
	srv := httptest.NewServer(fg)
	defer srv.Close()
	fg.base = srv.URL

	ctx := context.Background()

	// Small payload (<= simpleMax) takes the single-PUT path.
	small := []byte("hello onedrive")
	od := newTestOneDrive(srv.URL, 8, int64(len(small)))
	if err := od.Put(ctx, "/small.txt", bytes.NewReader(small)); err != nil {
		t.Fatalf("Put small: %v", err)
	}
	if got := fg.uploads["/root:/small.txt"]; !bytes.Equal(got, small) {
		t.Errorf("small upload = %q, want %q", got, small)
	}

	// Larger payload with a tiny chunk size forces a multi-chunk session.
	// 25 bytes over an 8-byte chunk = 4 chunks (8+8+8+1).
	big := []byte("0123456789abcdefghijklmno")
	odBig := newTestOneDrive(srv.URL, 8, 4) // simpleMax=4 → session path
	if err := odBig.Put(ctx, "/big.bin", bytes.NewReader(big)); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	if got := fg.uploads["/root:/big.bin"]; !bytes.Equal(got, big) {
		t.Errorf("big upload reassembled = %q, want %q", got, big)
	}
}
