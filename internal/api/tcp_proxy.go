package api

import (
	"context"
	"io"
	"log"
	"net"
	"time"

	"github.com/blasten/hive/internal/netmark"
)

// TCPProxy accepts inbound TCP connections on listenAddr and pipes each one
// bidirectionally to targetAddr.
type TCPProxy struct {
	listenAddr   string
	targetAddr   string
	outboundMark int
}

// NewTCPProxy creates a proxy that listens on listenAddr and dials targetAddr
// for each accepted connection. outboundMark, when non-zero, stamps SO_MARK
// on upstream sockets so they escape the sandbox iptables REDIRECT rule.
func NewTCPProxy(listenAddr, targetAddr string, outboundMark int) *TCPProxy {
	return &TCPProxy{listenAddr: listenAddr, targetAddr: targetAddr, outboundMark: outboundMark}
}

// Run accepts connections until ctx is cancelled or the listener closes.
func (p *TCPProxy) Run(ctx context.Context) error {
	l, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			return nil
		}
		go p.pipe(conn)
	}
}

func (p *TCPProxy) pipe(client net.Conn) {
	defer client.Close()
	d := &net.Dialer{Timeout: 10 * time.Second}
	if p.outboundMark != 0 {
		d.Control = netmark.Control(p.outboundMark)
	}
	upstream, err := d.Dial("tcp", p.targetAddr)
	if err != nil {
		log.Printf("tcp proxy: dial %s: %v", p.targetAddr, err)
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
