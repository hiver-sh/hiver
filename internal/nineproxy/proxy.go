package nineproxy

import (
	"errors"
	"io"
	"sync"
)

// errClosed is returned by the pumps once the proxy is shut down.
var errClosed = errors.New("nineproxy: closed")

// Proxy bridges the kernel v9fs transport (one end of a socketpair sbxguest owns)
// to a host 9p server connection, tracking the session so it can be replayed onto
// a fresh host connection after a snapshot resume. The kernel end never closes —
// from the kernel's view the transport only pauses while the host connection is
// down — so the mount and the workload's cwd survive.
type Proxy struct {
	kernel io.ReadWriteCloser
	sess   *Session

	mu      sync.Mutex
	cond    *sync.Cond
	host    io.ReadWriteCloser // current host conn; nil while disconnected
	hostGen uint64             // bumps on each (re)connect so stale failures are ignored
	closed  bool
}

// NewProxy wires a kernel-side transport to an initial host connection. host may
// be nil to start disconnected (then call Reconnect).
func NewProxy(kernel io.ReadWriteCloser, host io.ReadWriteCloser) *Proxy {
	p := &Proxy{kernel: kernel, sess: NewSession(), host: host}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Run pumps both directions until the kernel transport closes (mount torn down)
// or the proxy is Closed. Host-connection failures are survived: the pumps park
// until Reconnect supplies a fresh connection.
func (p *Proxy) Run() error {
	var wg sync.WaitGroup
	wg.Add(1)
	var hkErr error
	go func() { defer wg.Done(); hkErr = p.hostToKernel() }()
	khErr := p.kernelToHost()
	p.Close()
	wg.Wait()
	if khErr != nil && !errors.Is(khErr, errClosed) {
		return khErr
	}
	if hkErr != nil && !errors.Is(hkErr, errClosed) {
		return hkErr
	}
	return nil
}

// kernelToHost reads kernel→server messages, tracks them, and forwards to the
// host, re-delivering across a reconnect (the kernel may issue a request the
// instant the VM resumes, before the host link is back).
func (p *Proxy) kernelToHost() error {
	for {
		msg, err := ReadMsg(p.kernel)
		if err != nil {
			return err // kernel transport gone: the mount was torn down
		}
		p.mu.Lock()
		p.sess.ObserveClient(msg)
		p.mu.Unlock()
		if err := p.writeHost(msg); err != nil {
			return err
		}
	}
}

// hostToKernel reads server→kernel replies, tracks them, and forwards to the
// kernel. A host read error parks until a reconnect provides a new connection.
func (p *Proxy) hostToKernel() error {
	for {
		h, gen, ok := p.currentHost()
		if !ok {
			return errClosed
		}
		if h == nil {
			if !p.waitNewHost(gen) {
				return errClosed
			}
			continue
		}
		msg, err := ReadMsg(h)
		if err != nil {
			p.markHostDead(gen)
			continue
		}
		p.mu.Lock()
		p.sess.ObserveServer(msg)
		p.mu.Unlock()
		if _, err := p.kernel.Write(msg); err != nil {
			return err
		}
	}
}

// writeHost writes msg to the current host, blocking across a reconnect if the
// host link is down or the write fails.
func (p *Proxy) writeHost(msg []byte) error {
	for {
		h, gen, ok := p.currentHost()
		if !ok {
			return errClosed
		}
		if h != nil {
			if _, err := h.Write(msg); err == nil {
				return nil
			}
			p.markHostDead(gen)
		}
		if !p.waitNewHost(gen) {
			return errClosed
		}
	}
}

func (p *Proxy) currentHost() (io.ReadWriteCloser, uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host, p.hostGen, !p.closed
}

func (p *Proxy) markHostDead(gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hostGen == gen && p.host != nil {
		// Close it so the OTHER pump, which may be blocked reading this same dead
		// conn, unblocks promptly (a dead TCP read can otherwise hang until the
		// kernel's keepalive timeout) and parks for the reconnect.
		_ = p.host.Close()
		p.host = nil
		p.cond.Broadcast()
	}
}

// waitNewHost blocks until hostGen advances past gen (a reconnect) or the proxy
// closes. Returns false on close.
func (p *Proxy) waitNewHost(gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.hostGen == gen && !p.closed {
		p.cond.Wait()
	}
	return !p.closed
}

// Reconnect replays the tracked session onto newHost, then installs it as the
// live host connection and wakes the parked pumps. Called by sbxguest when the
// host signals (over the control channel) that the per-VM 9p listener has been
// re-bound after a resume.
func (p *Proxy) Reconnect(newHost io.ReadWriteCloser) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errClosed
	}
	// Pumps are parked (host went nil on failure) so the session is quiescent;
	// replay drives newHost directly (it is not yet the live host, so the
	// hostToKernel reader won't steal its replies).
	if err := p.sess.Replay(newHost); err != nil {
		return err
	}
	// Close the previous (dead) host conn if it wasn't already, so a pump still
	// blocked reading it unblocks and picks up newHost.
	if prev := p.host; prev != nil {
		_ = prev.Close()
	}
	p.host = newHost
	p.hostGen++
	p.cond.Broadcast()
	return nil
}

// Close tears the proxy down: closes the kernel transport and wakes the pumps.
func (p *Proxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.host != nil {
		_ = p.host.Close()
		p.host = nil
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	_ = p.kernel.Close()
}
