package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ExternalFSPort is the port the external file system host listens on.
// Pinned so the sandbox config can reference it literally via a
// `host-gateway` extra host.
const ExternalFSPort = 17083

// StartExternalFSHost binds an in-memory implementation of the external
// file system HTTP contract (api/external_file_system.yaml) on
// ExternalFSPort (all interfaces) and returns a stop function. It exists
// to back the `external` filesystem backend in e2e tests without standing
// up a real storage service.
//
// The host is a flat path → bytes store; directories are synthesized from
// key prefixes, mirroring how an object store presents folders. It is a
// deliberately independent re-implementation of the contract — the e2e
// test exercises the real sbxfuse client against it over the wire.
func StartExternalFSHost(t *testing.T) (stop func()) {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", ExternalFSPort))
	if err != nil {
		t.Fatalf("external-fs: listen :%d: %v", ExternalFSPort, err)
	}
	srv := &http.Server{Handler: &externalFSHost{files: map[string][]byte{}}}
	go func() { _ = srv.Serve(l) }()
	return func() { _ = srv.Close() }
}

// externalFSHost is the in-memory store behind StartExternalFSHost.
type externalFSHost struct {
	mu    sync.Mutex
	files map[string][]byte
}

type externalFileInfo struct {
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	IsDir bool      `json:"is_dir"`
}

func (h *externalFSHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/list":
		h.list(w, canonPath(q.Get("prefix")))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/directory":
		h.listDir(w, canonPath(q.Get("path")))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/stat":
		h.stat(w, canonPath(q.Get("path")))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/file":
		h.getFile(w, canonPath(q.Get("path")))
	case r.Method == http.MethodPut && r.URL.Path == "/v1/file":
		h.putFile(w, r, canonPath(q.Get("path")))
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/file":
		delete(h.files, canonPath(q.Get("path")))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/move":
		h.move(w, r)
	default:
		writeExternalErr(w, http.StatusNotFound, "unknown endpoint")
	}
}

func (h *externalFSHost) list(w http.ResponseWriter, prefix string) {
	var paths []string
	for p := range h.files {
		if prefix == "/" || p == prefix || strings.HasPrefix(p, strings.TrimRight(prefix, "/")+"/") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	writeJSON(w, map[string]any{"paths": paths})
}

func (h *externalFSHost) listDir(w http.ResponseWriter, dir string) {
	dirPrefix := strings.TrimRight(dir, "/") + "/"
	seen := map[string]externalFileInfo{}
	exists := dir == "/"
	for p, body := range h.files {
		if !strings.HasPrefix(p, dirPrefix) {
			continue
		}
		exists = true
		rest := strings.TrimPrefix(p, dirPrefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			// A nested entry — surface its first segment as a directory.
			name := dirPrefix + rest[:i]
			seen[name] = externalFileInfo{Path: name, IsDir: true}
		} else {
			seen[p] = externalFileInfo{Path: p, Size: int64(len(body))}
		}
	}
	if !exists {
		writeExternalErr(w, http.StatusNotFound, "no such directory")
		return
	}
	entries := make([]externalFileInfo, 0, len(seen))
	for _, e := range seen {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	writeJSON(w, map[string]any{"entries": entries})
}

func (h *externalFSHost) stat(w http.ResponseWriter, p string) {
	if body, ok := h.files[p]; ok {
		writeJSON(w, externalFileInfo{Path: p, Size: int64(len(body))})
		return
	}
	dirPrefix := strings.TrimRight(p, "/") + "/"
	for k := range h.files {
		if strings.HasPrefix(k, dirPrefix) {
			writeJSON(w, externalFileInfo{Path: p, IsDir: true})
			return
		}
	}
	writeExternalErr(w, http.StatusNotFound, "no such path")
}

func (h *externalFSHost) getFile(w http.ResponseWriter, p string) {
	body, ok := h.files[p]
	if !ok {
		writeExternalErr(w, http.StatusNotFound, "no such file")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(body)
}

func (h *externalFSHost) putFile(w http.ResponseWriter, r *http.Request, p string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeExternalErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	h.files[p] = body
	w.WriteHeader(http.StatusNoContent)
}

func (h *externalFSHost) move(w http.ResponseWriter, r *http.Request) {
	var body struct{ Src, Dst string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeExternalErr(w, http.StatusBadRequest, "bad body")
		return
	}
	src, dst := canonPath(body.Src), canonPath(body.Dst)
	content, ok := h.files[src]
	if !ok {
		writeExternalErr(w, http.StatusNotFound, "no such source")
		return
	}
	h.files[dst] = content
	delete(h.files, src)
	w.WriteHeader(http.StatusNoContent)
}

func canonPath(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeExternalErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
