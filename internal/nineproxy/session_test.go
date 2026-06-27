package nineproxy

import (
	"encoding/binary"
	"net"
	"reflect"
	"testing"
)

// qid writes a dummy 13-byte qid.
func qid(w *builder) { w.u8(0); w.u32(1); w.b = append(w.b, make([]byte, 8)...) }

const nofidVal = ^uint32(0)

// feed drives a tracked client request + its server reply into the session.
func trackVersion(s *Session) {
	s.ObserveClient(frame(tTversion, notag, func(w *builder) { w.u32(262144); w.str("9p2000.L") }))
	s.ObserveServer(frame(tRversion, notag, func(w *builder) { w.u32(262144); w.str("9p2000.L") }))
}
func trackAttach(s *Session, fid uint32) {
	s.ObserveClient(frame(tTattach, 1, func(w *builder) {
		w.u32(fid)
		w.u32(nofidVal)
		w.str("agent")
		w.str("workspace")
		w.u32(0)
	}))
	s.ObserveServer(frame(tRattach, 1, qid))
}
func trackWalk(s *Session, fid, newfid uint32, names ...string) {
	s.ObserveClient(frame(tTwalk, 2, func(w *builder) {
		w.u32(fid)
		w.u32(newfid)
		w.u16(uint16(len(names)))
		for _, n := range names {
			w.str(n)
		}
	}))
	s.ObserveServer(frame(tRwalk, 2, func(w *builder) {
		w.u16(uint16(len(names)))
		for range names {
			qid(w)
		}
	}))
}
func trackLopen(s *Session, fid, flags uint32) {
	s.ObserveClient(frame(tTlopen, 3, func(w *builder) { w.u32(fid); w.u32(flags) }))
	s.ObserveServer(frame(tRlopen, 3, func(w *builder) { qid(w); w.u32(0) }))
}

func TestSessionTracksFids(t *testing.T) {
	s := NewSession()
	trackVersion(s)
	trackAttach(s, 1)
	trackWalk(s, 1, 2, "home", "agent") // /home/agent
	trackLopen(s, 2, 0o2)               // O_RDWR
	trackWalk(s, 1, 3)                  // clone of root
	trackWalk(s, 2, 4, "notes.txt")     // /home/agent/notes.txt

	if !s.haveAttach || s.rootFid != 1 {
		t.Fatalf("attach not tracked: %+v", s)
	}
	if got := s.fids[2].names; !reflect.DeepEqual(got, []string{"home", "agent"}) {
		t.Errorf("fid2 names = %v", got)
	}
	if !s.fids[2].open || s.fids[2].flags != 0o2 {
		t.Errorf("fid2 open state wrong: %+v", s.fids[2])
	}
	if got := s.fids[3].names; len(got) != 0 {
		t.Errorf("fid3 (root clone) names = %v, want empty", got)
	}
	if got := s.fids[4].names; !reflect.DeepEqual(got, []string{"home", "agent", "notes.txt"}) {
		t.Errorf("fid4 names = %v", got)
	}

	// Clunk fid 4 → gone.
	s.ObserveClient(frame(tTclunk, 5, func(w *builder) { w.u32(4) }))
	s.ObserveServer(frame(tRclunk, 5, func(w *builder) {}))
	if _, ok := s.fids[4]; ok {
		t.Error("fid4 should be clunked")
	}

	// A failed walk must NOT establish the new fid.
	s.ObserveClient(frame(tTwalk, 6, func(w *builder) { w.u32(1); w.u32(7); w.u16(1); w.str("missing") }))
	s.ObserveServer(frame(tRlerror, 6, func(w *builder) { w.u32(2 /*ENOENT*/) }))
	if _, ok := s.fids[7]; ok {
		t.Error("fid7 must not exist after a failed walk")
	}
}

// fakeServer answers the replay handshake over conn and records the fids it was
// asked to (re)establish, so the test can assert the replay reproduced them.
type fakeServer struct {
	attachFid uint32
	walks     map[uint32][]string // newfid -> names (only root-origin walks)
	opens     []uint32
}

func serveFake(conn net.Conn) *fakeServer {
	fs := &fakeServer{walks: map[uint32][]string{}}
	go func() {
		root := uint32(0)
		tmpNames := map[uint32][]string{} // track fid->names to resolve chunked walks
		for {
			m, err := ReadMsg(conn)
			if err != nil {
				return
			}
			tag := msgTag(m)
			c := &cursor{b: msgBody(m)}
			switch msgType(m) {
			case tTversion:
				_ = c.u32()
				ver := c.str()
				_, _ = conn.Write(frame(tRversion, tag, func(w *builder) { w.u32(262144); w.str(ver) }))
			case tTattach:
				root = c.u32()
				fs.attachFid = root
				tmpNames[root] = nil
				_, _ = conn.Write(frame(tRattach, tag, qid))
			case tTwalk:
				fid := c.u32()
				newfid := c.u32()
				n := int(c.u16())
				names := make([]string, n)
				for i := range names {
					names[i] = c.str()
				}
				full := append(append([]string(nil), tmpNames[fid]...), names...)
				tmpNames[newfid] = full
				if fid == root {
					fs.walks[newfid] = full
				}
				_, _ = conn.Write(frame(tRwalk, tag, func(w *builder) {
					w.u16(uint16(n))
					for range names {
						qid(w)
					}
				}))
			case tTlopen:
				fid := c.u32()
				_ = c.u32()
				fs.opens = append(fs.opens, fid)
				_, _ = conn.Write(frame(tRlopen, tag, func(w *builder) { qid(w); w.u32(0) }))
			case tTclunk:
				_ = c.u32()
				_, _ = conn.Write(frame(tRclunk, tag, func(w *builder) {}))
			}
		}
	}()
	return fs
}

func TestReplayReestablishesSession(t *testing.T) {
	s := NewSession()
	trackVersion(s)
	trackAttach(s, 1)
	trackWalk(s, 1, 2, "home", "agent")
	trackLopen(s, 2, 0o2|oTrunc) // O_RDWR|O_TRUNC — replay must strip O_TRUNC
	trackWalk(s, 1, 3)           // root clone

	client, server := net.Pipe()
	fs := serveFake(server)
	defer client.Close()

	if err := s.Replay(client); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if fs.attachFid != 1 {
		t.Errorf("replay attached fid %d, want 1", fs.attachFid)
	}
	if got := fs.walks[2]; !reflect.DeepEqual(got, []string{"home", "agent"}) {
		t.Errorf("replay walk for fid2 = %v", got)
	}
	if _, ok := fs.walks[3]; !ok {
		t.Errorf("replay did not re-walk root clone fid3; walks=%v", fs.walks)
	}
	if len(fs.opens) != 1 || fs.opens[0] != 2 {
		t.Errorf("replay opens = %v, want [2]", fs.opens)
	}
}

var _ = binary.LittleEndian
