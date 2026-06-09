package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hiver-sh/hiver/internal/firecracker"
	"github.com/hiver-sh/hiver/internal/vsockfile"
)

// Files serves the management file API by proxying every operation to the
// in-guest file service over vsock. The guest sees the assembled workload root
// — the overlay plus the 9p-mounted workspaces — at its real agent paths, so a
// single path handles every request without distinguishing workspace mounts
// from the guest overlay. The mounts argument the interface passes is unused
// for the same reason: the guest resolves the path itself.
func (m *microvm) Files() FileBridge { return microvmGuestFiles{vsockUDS: m.vsockUDS} }

type microvmGuestFiles struct {
	vsockUDS string
}

// dial opens a fresh connection to the guest file service, retrying until the
// guest agent is listening (it starts the service right after boot) or the
// deadline passes.
func (f microvmGuestFiles) dial() (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		conn, err := firecracker.DialGuest(ctx, f.vsockUDS, vsockfile.GuestPort)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func readResult(conn net.Conn) (vsockfile.Result, error) {
	t, payload, err := vsockfile.ReadFrame(conn)
	if err != nil {
		return vsockfile.Result{}, err
	}
	if t != vsockfile.FrameResult {
		return vsockfile.Result{}, fmt.Errorf("expected result frame, got %d", t)
	}
	var res vsockfile.Result
	if err := json.Unmarshal(payload, &res); err != nil {
		return vsockfile.Result{}, err
	}
	if res.Err != "" {
		return res, errors.New(res.Err)
	}
	return res, nil
}

func (f microvmGuestFiles) List(agentPath string, _ []string) ([]FileEntry, error) {
	conn, err := f.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := vsockfile.WriteJSON(conn, vsockfile.FrameRequest, vsockfile.Request{Op: vsockfile.OpList, Path: agentPath}); err != nil {
		return nil, err
	}
	res, err := readResult(conn)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, len(res.Entries))
	for i, e := range res.Entries {
		out[i] = FileEntry{Name: e.Name, IsDir: e.IsDir, Size: e.Size}
	}
	return out, nil
}

func (f microvmGuestFiles) Stat(agentPath string, _ []string) (FileEntry, error) {
	conn, err := f.dial()
	if err != nil {
		return FileEntry{}, err
	}
	defer conn.Close()
	if err := vsockfile.WriteJSON(conn, vsockfile.FrameRequest, vsockfile.Request{Op: vsockfile.OpStat, Path: agentPath}); err != nil {
		return FileEntry{}, err
	}
	res, err := readResult(conn)
	if err != nil {
		return FileEntry{}, err
	}
	if res.Entry == nil {
		return FileEntry{}, fmt.Errorf("stat: empty result")
	}
	return FileEntry{Name: res.Entry.Name, IsDir: res.Entry.IsDir, Size: res.Entry.Size}, nil
}

func (f microvmGuestFiles) Open(agentPath string, _ []string) (io.ReadCloser, int64, error) {
	conn, err := f.dial()
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	if err := vsockfile.WriteJSON(conn, vsockfile.FrameRequest, vsockfile.Request{Op: vsockfile.OpRead, Path: agentPath}); err != nil {
		return nil, 0, err
	}
	res, err := readResult(conn)
	if err != nil {
		return nil, 0, err
	}
	// The body is read into memory and the connection released here: the
	// management API hands the whole file back to one HTTP response, so a
	// streaming reader tied to the live vsock conn buys nothing.
	var buf bytes.Buffer
	for {
		t, payload, rerr := vsockfile.ReadFrame(conn)
		if rerr != nil {
			return nil, 0, rerr
		}
		if t == vsockfile.FrameEnd {
			break
		}
		if t != vsockfile.FrameData {
			return nil, 0, fmt.Errorf("expected data frame, got %d", t)
		}
		buf.Write(payload)
	}
	return io.NopCloser(&buf), res.Size, nil
}

func (f microvmGuestFiles) Save(agentDir, name string, _ []string, r io.Reader) (int64, error) {
	conn, err := f.dial()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if err := vsockfile.WriteJSON(conn, vsockfile.FrameRequest, vsockfile.Request{Op: vsockfile.OpWrite, Path: agentDir, Name: name}); err != nil {
		return 0, err
	}
	buf := make([]byte, vsockfile.ChunkSize)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := vsockfile.WriteFrame(conn, vsockfile.FrameData, buf[:n]); err != nil {
				return 0, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, rerr
		}
	}
	if err := vsockfile.WriteFrame(conn, vsockfile.FrameEnd, nil); err != nil {
		return 0, err
	}
	res, err := readResult(conn)
	if err != nil {
		return 0, err
	}
	return res.Size, nil
}
