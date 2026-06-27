package nineproxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

// Linux open flags whose meaning is "at creation" and must be stripped when
// REopening an already-created file on replay (the file exists; reopening with
// these would re-create/truncate it).
const (
	oCreat = 0o100
	oExcl  = 0o200
	oTrunc = 0o1000
)

const notag = uint16(0xFFFF)

// ReadMsg reads one size-framed 9p message (size[4,le] including itself).
func ReadMsg(r io.Reader) ([]byte, error) {
	var sz [4]byte
	if _, err := io.ReadFull(r, sz[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(sz[:])
	if n < headerLen || n > (1<<24) {
		return nil, fmt.Errorf("nineproxy: bad message size %d", n)
	}
	m := make([]byte, n)
	copy(m, sz[:])
	if _, err := io.ReadFull(r, m[4:]); err != nil {
		return nil, err
	}
	return m, nil
}

// builder assembles a single 9p message (little-endian) for replay.
type builder struct{ b []byte }

func (w *builder) u8(v uint8)   { w.b = append(w.b, v) }
func (w *builder) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *builder) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *builder) str(s string) {
	w.u16(uint16(len(s)))
	w.b = append(w.b, s...)
}

// frame finalises a message of the given type+tag, prefixing the size header.
func frame(typ byte, tag uint16, body func(*builder)) []byte {
	w := &builder{}
	w.u32(0) // size placeholder
	w.u8(typ)
	w.u16(tag)
	body(w)
	binary.LittleEndian.PutUint32(w.b[0:], uint32(len(w.b)))
	return w.b
}

// roundTrip writes req and reads the matching reply, erroring on Rlerror or an
// unexpected type. Replay is synchronous (one request outstanding), so tag
// correlation is trivial.
func roundTrip(conn io.ReadWriter, req []byte, wantType byte) ([]byte, error) {
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	resp, err := ReadMsg(conn)
	if err != nil {
		return nil, err
	}
	if msgType(resp) == tRlerror {
		c := &cursor{b: msgBody(resp)}
		return nil, fmt.Errorf("nineproxy: replay Rlerror errno=%d", c.u32())
	}
	if msgType(resp) != wantType {
		return nil, fmt.Errorf("nineproxy: replay unexpected reply type %d (want %d)", msgType(resp), wantType)
	}
	return resp, nil
}

// Replay re-establishes the tracked session on a fresh server connection: it
// re-negotiates the version, re-attaches the root fid, then re-walks and re-opens
// every live fid using the SAME fid numbers the kernel still holds, so subsequent
// proxied traffic (which references those fids) resolves on the new server. The
// kernel never observed a disconnect, so its fids are unchanged.
func (s *Session) Replay(conn io.ReadWriter) error {
	if !s.haveAttach {
		return fmt.Errorf("nineproxy: cannot replay before attach")
	}
	// Tversion
	if _, err := roundTrip(conn, frame(tTversion, notag, func(w *builder) {
		w.u32(s.msize)
		w.str(s.version)
	}), tRversion); err != nil {
		return fmt.Errorf("version: %w", err)
	}
	// Tattach(rootFid)
	if _, err := roundTrip(conn, frame(tTattach, 0, func(w *builder) {
		w.u32(s.rootFid)
		w.u32(s.afid)
		w.str(s.uname)
		w.str(s.aname)
		w.u32(s.nUname)
	}), tRattach); err != nil {
		return fmt.Errorf("attach: %w", err)
	}

	// Re-walk every non-root fid from root, deterministically (sorted) so a temp
	// fid never collides with a real one and tests are stable.
	var fidNums []uint32
	for fid, st := range s.fids {
		if st.root {
			continue
		}
		fidNums = append(fidNums, fid)
	}
	sort.Slice(fidNums, func(i, j int) bool { return fidNums[i] < fidNums[j] })
	for _, fid := range fidNums {
		if err := s.replayWalk(conn, fid, s.fids[fid].names); err != nil {
			return fmt.Errorf("walk fid %d: %w", fid, err)
		}
	}
	// Re-open every open fid (root included).
	for _, fid := range sortedFids(s.fids) {
		st := s.fids[fid]
		if !st.open {
			continue
		}
		flags := st.flags &^ (oCreat | oExcl | oTrunc)
		if _, err := roundTrip(conn, frame(tTlopen, 0, func(w *builder) {
			w.u32(fid)
			w.u32(flags)
		}), tRlopen); err != nil {
			return fmt.Errorf("lopen fid %d: %w", fid, err)
		}
	}
	return nil
}

// replayWalk walks root→names into target, chunking names into ≤maxWElem groups
// through temp fids (9p caps names per Twalk). The common case (≤16 components)
// is a single walk root→target.
func (s *Session) replayWalk(conn io.ReadWriter, target uint32, names []string) error {
	src := s.rootFid
	tmp := tempFidBase
	for start := 0; start < len(names); start += maxWElem {
		end := start + maxWElem
		if end > len(names) {
			end = len(names)
		}
		chunk := names[start:end]
		last := end >= len(names)
		dst := tmp
		if last {
			dst = target
		}
		resp, err := roundTrip(conn, frame(tTwalk, 0, func(w *builder) {
			w.u32(src)
			w.u32(dst)
			w.u16(uint16(len(chunk)))
			for _, n := range chunk {
				w.str(n)
			}
		}), tRwalk)
		if err != nil {
			return err
		}
		if got := int(binary.LittleEndian.Uint16(msgBody(resp))); got != len(chunk) {
			return fmt.Errorf("partial walk: %d/%d", got, len(chunk))
		}
		if src != s.rootFid && src != target {
			// Clunk the previous temp fid; ignore its reply best-effort.
			_, _ = roundTrip(conn, frame(tTclunk, 0, func(w *builder) { w.u32(src) }), tRclunk)
		}
		src = dst
		tmp++
	}
	// Zero-name walk (a clone of root): emit an explicit empty Twalk.
	if len(names) == 0 {
		resp, err := roundTrip(conn, frame(tTwalk, 0, func(w *builder) {
			w.u32(s.rootFid)
			w.u32(target)
			w.u16(0)
		}), tRwalk)
		if err != nil {
			return err
		}
		_ = resp
	}
	return nil
}

func sortedFids(m map[uint32]*fidState) []uint32 {
	out := make([]uint32, 0, len(m))
	for f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
