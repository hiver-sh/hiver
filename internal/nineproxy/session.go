// Package nineproxy is an in-guest 9p2000.L reconnect proxy.
//
// The workspace is a kernel v9fs mount (trans=fd) whose transport is a TCP
// connection to a per-VM host 9p server. A snapshot resume destroys that host
// connection (new netns/tap/IP, old listener gone), and kernel v9fs cannot
// reconnect a dropped session itself — fids and in-flight tags are lost, so the
// mount would error permanently, stranding the workload's cwd.
//
// nineproxy fixes that by interposing: kernel v9fs mounts over a socketpair whose
// other end the (long-lived, snapshot-surviving) sbxguest agent holds. sbxguest
// proxies bytes between that socketpair and the host 9p server while parsing the
// 9p2000.L stream to track session state (the version negotiation, the attach
// parameters, and every live fid's path + open flags). When the host connection
// dies the kernel sees only a pause on its fd; on resume sbxguest dials the
// re-bound host listener and Replays the session — re-Tversion, re-Tattach, and
// re-walk + re-open every live fid using the SAME fid numbers the kernel still
// holds — then resumes proxying. The mount, and the workload's cwd, never break.
//
// Correctness assumption: the guest is quiesced before the snapshot (the snapshot
// action flushes first), so no request is in flight across the cut. fid mutations
// are committed only on a successful reply, matched by tag.
package nineproxy

import (
	"encoding/binary"
	"io"
)

// 9p2000.L message types (subset we parse; others pass through opaquely).
const (
	tRlerror    = 7
	tRlopen     = 13
	tTlopen     = 12
	tTlcreate   = 14
	tTxattrwalk = 30
	tTversion   = 100
	tRversion   = 101
	tTattach    = 104
	tRattach    = 105
	tTwalk      = 110
	tRwalk      = 111
	tTclunk     = 120
	tRclunk     = 121
	tTremove    = 122

	maxWElem      = 16 // 9p caps names per Twalk
	headerLen     = 7  // size[4] type[1] tag[2]
	tempFidBase   = uint32(0xFFFF0000)
	defaultMsize  = 262144
	versionString = "9p2000.L"
)

// fidState records what a live fid points at, enough to re-establish it on a
// fresh server: its path from the attach root (the sequence of walked names) and,
// if opened, the open flags.
type fidState struct {
	names []string // walk names from root; empty == the attach root itself
	root  bool
	open  bool
	flags uint32
}

// Session tracks the live 9p session so it can be replayed onto a fresh server
// connection. It observes both directions of the proxied stream; it is not safe
// for concurrent use (the proxy serialises observe calls through one goroutine
// per direction with a shared lock — see proxy.go).
type Session struct {
	msize   uint32
	version string

	// attach params, captured from the first successful Tattach so Replay can
	// reissue the exact same attach.
	rootFid uint32
	afid    uint32
	uname   string
	aname   string
	nUname  uint32
	haveAttach bool

	fids    map[uint32]*fidState
	pending map[uint16]*pending // outstanding requests by tag
}

// pending is an in-flight request whose fid mutation is applied only when its
// matching reply arrives successfully.
type pending struct {
	typ    byte
	fid    uint32
	newfid uint32
	names  []string
	flags  uint32
	name   string
	// attach
	afid   uint32
	uname  string
	aname  string
	nUname uint32
	// version
	msize   uint32
	version string
}

// NewSession returns an empty tracker.
func NewSession() *Session {
	return &Session{
		msize:   defaultMsize,
		version: versionString,
		fids:    map[uint32]*fidState{},
		pending: map[uint16]*pending{},
	}
}

// cursor reads little-endian 9p fields sequentially from a message body.
type cursor struct {
	b   []byte
	off int
	err error
}

func (c *cursor) u16() uint16 {
	if c.err != nil || c.off+2 > len(c.b) {
		c.err = io.ErrUnexpectedEOF
		return 0
	}
	v := binary.LittleEndian.Uint16(c.b[c.off:])
	c.off += 2
	return v
}
func (c *cursor) u32() uint32 {
	if c.err != nil || c.off+4 > len(c.b) {
		c.err = io.ErrUnexpectedEOF
		return 0
	}
	v := binary.LittleEndian.Uint32(c.b[c.off:])
	c.off += 4
	return v
}
func (c *cursor) str() string {
	n := int(c.u16())
	if c.err != nil || c.off+n > len(c.b) {
		c.err = io.ErrUnexpectedEOF
		return ""
	}
	s := string(c.b[c.off : c.off+n])
	c.off += n
	return s
}

// msgType/msgTag read the header fields of a full framed message.
func msgType(m []byte) byte   { return m[4] }
func msgTag(m []byte) uint16  { return binary.LittleEndian.Uint16(m[5:]) }
func msgBody(m []byte) []byte { return m[headerLen:] }

// ObserveClient updates pending state from a host→… T-message (kernel→server).
// It records the intended mutation; ObserveServer commits it on success.
func (s *Session) ObserveClient(m []byte) {
	if len(m) < headerLen {
		return
	}
	tag := msgTag(m)
	c := &cursor{b: msgBody(m)}
	switch msgType(m) {
	case tTversion:
		ms := c.u32()
		ver := c.str()
		s.pending[tag] = &pending{typ: tTversion, msize: ms, version: ver}
	case tTattach:
		fid := c.u32()
		afid := c.u32()
		uname := c.str()
		aname := c.str()
		nUname := c.u32()
		s.pending[tag] = &pending{typ: tTattach, fid: fid, afid: afid, uname: uname, aname: aname, nUname: nUname}
	case tTwalk:
		fid := c.u32()
		newfid := c.u32()
		nw := int(c.u16())
		names := make([]string, 0, nw)
		for i := 0; i < nw; i++ {
			names = append(names, c.str())
		}
		s.pending[tag] = &pending{typ: tTwalk, fid: fid, newfid: newfid, names: names}
	case tTlopen:
		fid := c.u32()
		flags := c.u32()
		s.pending[tag] = &pending{typ: tTlopen, fid: fid, flags: flags}
	case tTlcreate:
		fid := c.u32()
		name := c.str()
		flags := c.u32()
		s.pending[tag] = &pending{typ: tTlcreate, fid: fid, name: name, flags: flags}
	case tTclunk:
		fid := c.u32()
		s.pending[tag] = &pending{typ: tTclunk, fid: fid}
	case tTremove:
		fid := c.u32()
		s.pending[tag] = &pending{typ: tTremove, fid: fid}
	case tTxattrwalk:
		// An xattr fid is created on newfid; we cannot cleanly re-walk it. Mark
		// newfid untracked so Replay skips it (xattr fids are short-lived and very
		// unlikely to be live at a quiesced snapshot). Record nothing.
		_ = c
	}
}

// ObserveServer commits or discards a pending mutation based on a …→host
// R-message (server→kernel).
func (s *Session) ObserveServer(m []byte) {
	if len(m) < headerLen {
		return
	}
	tag := msgTag(m)
	p := s.pending[tag]
	if p == nil {
		return
	}
	delete(s.pending, tag)
	typ := msgType(m)
	c := &cursor{b: msgBody(m)}

	// Tremove clunks its fid regardless of success/failure.
	if p.typ == tTremove {
		delete(s.fids, p.fid)
		return
	}
	if typ == tRlerror {
		// Request failed: no fid mutation (a failed clunk still releases the fid).
		if p.typ == tTclunk {
			delete(s.fids, p.fid)
		}
		return
	}
	switch p.typ {
	case tTversion:
		s.msize = p.msize
		if p.version != "" {
			s.version = p.version
		}
	case tTattach:
		s.rootFid = p.fid
		s.afid = p.afid
		s.uname = p.uname
		s.aname = p.aname
		s.nUname = p.nUname
		s.haveAttach = true
		s.fids[p.fid] = &fidState{root: true}
	case tTwalk:
		nwqid := int(c.u16())
		if nwqid != len(p.names) {
			return // partial/failed walk → newfid not established
		}
		base := s.fids[p.fid]
		if base == nil {
			return
		}
		names := append(append([]string(nil), base.names...), p.names...)
		s.fids[p.newfid] = &fidState{names: names}
	case tTlopen:
		if f := s.fids[p.fid]; f != nil {
			f.open = true
			f.flags = p.flags
		}
	case tTlcreate:
		if f := s.fids[p.fid]; f != nil {
			f.names = append(append([]string(nil), f.names...), p.name)
			f.open = true
			f.flags = p.flags
		}
	case tTclunk:
		delete(s.fids, p.fid)
	}
}
