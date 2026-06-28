//go:build linux

package main

import (
	"net"
	"sync"

	"github.com/hiver-sh/hiver/internal/vsockexec"
)

// workloadLogs captures the entrypoint workload's stdout/stderr inside the guest
// so the host can stream them as stdio events — the microvm equivalent of the
// container backend forwarding the agent child's stdout. Without it the guest
// entrypoint's output (including a crashing Chrome's stderr) dies on the guest
// console and never reaches the inspector, leaving failures invisible.
//
// A bounded backlog is replayed to each connecting host so output produced before
// the host attached — early startup, an immediate crash — is still delivered.
// Writes never block the workload: a slow or absent host only costs buffered
// backlog, and a full subscriber queue drops rather than stalls.
type workloadLogs struct {
	mu       sync.Mutex
	backlog  []logFrame
	backlogN int // total bytes buffered, capped at logBacklogMax
	subs     map[chan logFrame]struct{}
}

type logFrame struct {
	typ  vsockexec.FrameType
	data []byte
}

const (
	logBacklogMax = 256 * 1024 // cap backlog bytes so a chatty/looping workload can't grow it unbounded
	logSubBuffer  = 512        // per-subscriber queue depth before dropping (slow host)
)

var guestLogs = &workloadLogs{subs: map[chan logFrame]struct{}{}}

func (w *workloadLogs) write(typ vsockexec.FrameType, p []byte) {
	if len(p) == 0 {
		return
	}
	cp := append([]byte(nil), p...)
	f := logFrame{typ, cp}
	w.mu.Lock()
	w.backlog = append(w.backlog, f)
	w.backlogN += len(cp)
	for w.backlogN > logBacklogMax && len(w.backlog) > 1 {
		w.backlogN -= len(w.backlog[0].data)
		w.backlog = w.backlog[1:]
	}
	for ch := range w.subs {
		select {
		case ch <- f:
		default: // slow host: drop rather than block the workload
		}
	}
	w.mu.Unlock()
}

// subscribe returns the current backlog snapshot plus a channel of subsequent
// frames. Taken under one lock so no frame is both in the snapshot and the
// channel (and none is lost between them).
func (w *workloadLogs) subscribe() ([]logFrame, chan logFrame) {
	ch := make(chan logFrame, logSubBuffer)
	w.mu.Lock()
	snapshot := append([]logFrame(nil), w.backlog...)
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	return snapshot, ch
}

func (w *workloadLogs) unsubscribe(ch chan logFrame) {
	w.mu.Lock()
	delete(w.subs, ch)
	w.mu.Unlock()
}

// logWriter tees one workload stream into the hub; the caller keeps the console
// copy via an io.MultiWriter so kernel/serial debugging is unaffected.
type logWriter struct{ typ vsockexec.FrameType }

func (lw logWriter) Write(p []byte) (int, error) {
	guestLogs.write(lw.typ, p)
	return len(p), nil
}

// streamWorkloadLogs streams the workload's stdout/stderr to one host connection
// (the ChannelLogs handler on the shared guest port): it replays the backlog,
// then forwards live frames until the host disconnects. The host
// (microvm.StreamWorkloadLogs) publishes each frame as a stdio event. A
// reconnecting host re-attaches and replays (the hub survives snapshot resume in
// guest RAM).
func streamWorkloadLogs(conn net.Conn) {
	backlog, ch := guestLogs.subscribe()
	defer guestLogs.unsubscribe(ch)
	for _, f := range backlog {
		if vsockexec.WriteFrame(conn, f.typ, f.data) != nil {
			return
		}
	}
	for f := range ch {
		if vsockexec.WriteFrame(conn, f.typ, f.data) != nil {
			return
		}
	}
}
