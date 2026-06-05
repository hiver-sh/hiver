package isolation

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// The microvm guest has its own network stack, so it can't reach the pod's
// loopback resolver (docker publishes its embedded DNS at 127.0.0.11, which in
// the guest is just the guest's own empty loopback). The guest's resolv.conf
// therefore points at the tap gateway, and this stateless relay — running in
// the pod netns, where 127.0.0.11 resolves — forwards queries to the pod's real
// resolver on the guest's behalf. It mirrors how the container backend gets DNS
// "for free" by sharing the pod netns.

const guestDNSPort = "53"

// startDNSForwarder binds a UDP+TCP DNS relay on listenIP:53 and forwards every
// query to upstream (the pod's own nameserver). It returns once the listeners
// are up; the relay runs until ctx is cancelled.
func startDNSForwarder(ctx context.Context, listenIP, upstream string) error {
	addr := net.JoinHostPort(listenIP, guestDNSPort)

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("dns udp listen %s: %w", addr, err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return fmt.Errorf("dns tcp listen %s: %w", addr, err)
	}

	go func() {
		<-ctx.Done()
		pc.Close()
		ln.Close()
	}()

	go serveDNSUDP(pc, upstream)
	go serveDNSTCP(ln, upstream)
	return nil
}

// serveDNSUDP relays each datagram to upstream and writes the reply back to the
// client. One short-lived upstream socket per query keeps it stateless; DNS
// answers are a single datagram, so no reassembly is needed.
func serveDNSUDP(pc net.PacketConn, upstream string) {
	buf := make([]byte, 4096)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			return // listener closed on ctx cancel
		}
		query := append([]byte(nil), buf[:n]...)
		go func() {
			u, err := net.Dial("udp", upstream)
			if err != nil {
				return
			}
			defer u.Close()
			_ = u.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := u.Write(query); err != nil {
				return
			}
			resp := make([]byte, 65535)
			m, err := u.Read(resp)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(resp[:m], client)
		}()
	}
}

// serveDNSTCP proxies each TCP DNS connection (used for large responses) to
// upstream, splicing bytes in both directions.
func serveDNSTCP(ln net.Listener, upstream string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // listener closed on ctx cancel
		}
		go func() {
			defer c.Close()
			u, err := net.Dial("tcp", upstream)
			if err != nil {
				return
			}
			defer u.Close()
			go io.Copy(u, c)
			_, _ = io.Copy(c, u)
		}()
	}
}

// podUpstreamDNS returns the pod's first configured nameserver as host:53, so
// the relay forwards to whatever resolver the pod itself uses (docker's
// 127.0.0.11 by default). Falls back to 127.0.0.11 if resolv.conf is unreadable.
func podUpstreamDNS() string {
	const fallback = "127.0.0.11:" + guestDNSPort
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return fallback
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			return net.JoinHostPort(f[1], guestDNSPort)
		}
	}
	return fallback
}

// resolvConfForGuest rewrites the pod's resolv.conf for the guest: the pod's
// nameservers (e.g. the unreachable 127.0.0.11) are replaced with the tap
// gateway, where startDNSForwarder listens. search/options/comment lines are
// preserved so short-name resolution matches the pod.
func resolvConfForGuest(podResolv []byte, gateway string) []byte {
	var b strings.Builder
	b.WriteString("nameserver " + gateway + "\n")
	for _, line := range strings.Split(string(podResolv), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "nameserver") || strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line + "\n")
	}
	return []byte(b.String())
}
