package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"sync"
	"time"
)

// DNS sinkhole.
//
// With transparent egress, the workload resolves names itself and the proxy
// dials whatever IP came back. That makes plain DNS an unfiltered, unaudited
// side channel: a jailbroken agent can encode data into query labels
// (`<secret>.exfil.attacker.com`) and the host resolver recurses it straight
// out, even when every TCP host is denied.
//
// Rather than turn DNS into a second policy engine, we remove it as an
// information channel. iptables redirects all workload DNS (to any resolver)
// here, and ServeDNSSink answers every query with a single constant placeholder
// address — a constant function leaks zero bits, so tunnelling is structurally
// impossible. The agent then connects to the placeholder; the existing
// REDIRECT/DNAT funnels that TCP to the proxy, which reads the real hostname
// from SNI/Host and re-resolves it itself (on its own SO_MARK'd resolver, which
// escapes this sink). Policy stays exactly where it already is: the host-pattern
// allowlist on the connection.
//
// The sink serves UDP only. DNS-over-TCP exists only as a fallback when a UDP
// answer is truncated, and the sink's answer is a single tiny record that never
// sets TC, so real resolvers never fall back to TCP. iptables still redirects
// TCP/53 to this port — that redirect has to sit above docker's embedded-DNS
// DNAT, otherwise TCP DNS to 127.0.0.11 would resolve for real — but nothing
// listens on TCP here, so a stray DNS-over-TCP attempt is refused rather than
// resolved. The hole stays closed without a second listener.
//
// The proxy's own resolver traffic carries OutboundMark and is excluded by the
// iptables rule before reaching this listener, so it resolves truthfully.

// sinkTTL is 0 so resolvers don't cache the placeholder: every lookup is
// re-asked, which keeps the audit stream honest about what the agent queried.
const sinkTTL = 0

// DNS qtypes we special-case. Everything else gets a NODATA (NOERROR, no
// answer) reply, which is a valid "the name exists but not for this type"
// response and keeps clients from erroring out.
const (
	dnsTypeA    = 1
	dnsTypeAAAA = 28
)

// ServeDNSSink listens for DNS on addr (UDP) and answers every query with
// sinkIP (an IPv4 address). It never forwards or recurses. Each query is audited
// as an allowed `DNS` egress event carrying the queried name, so the inspector's
// Network view shows exactly what the agent looked up. TCP/53 is intentionally
// not served (see the package comment). Returns when ctx is cancelled.
func (p *Proxy) ServeDNSSink(ctx context.Context, addr string, sinkIP net.IP) error {
	v4 := sinkIP.To4()
	if v4 == nil {
		return errors.New("proxy: DNS sink IP must be IPv4")
	}

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	logSinkStartup(addr, sinkIP)
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	go serveSinkUDP(pc, v4, p.auditDNS)
	return nil
}

// auditDNS records a sinkholed lookup as an allowed `DNS` egress request with
// an immediately paired response. The sink answers locally, so the outcome is
// known the moment the query arrives — emitting the response right away lets
// consumers close the event instead of holding it open. Status is 0: there is
// no HTTP status for a DNS answer. The real allow/deny still happens later,
// when the agent connects to the placeholder and the proxy matches the host.
//
// Repeated lookups of the same name are collapsed (see dnsDedupe): a chatty
// agent re-resolving the same host every request would otherwise flood the
// stream. Distinct names — the signal that matters for spotting tunnelling —
// still each emit at least once.
func (p *Proxy) auditDNS(name string) {
	if !p.dnsDedupe.shouldAudit(name, time.Now()) {
		return
	}
	ac := p.beginAudit("", "DNS", name, "", "")
	ac.allow()
	ac.response(0)
}

// dnsDedupe suppresses repeat audit events for the same queried name within a
// time window, while keeping memory bounded under a flood of unique names. Its
// zero value is ready to use.
type dnsDedupe struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const (
	// dnsAuditWindow is how long a name stays "recently audited"; a repeat
	// within the window is suppressed, after it the name surfaces again so the
	// stream still reflects ongoing activity.
	dnsAuditWindow = time.Minute
	// dnsAuditMaxEntries caps the tracking map so an agent spraying unique
	// names (the tunnelling shape) can't grow it without bound.
	dnsAuditMaxEntries = 4096
)

// shouldAudit reports whether name should be emitted now, recording the emit
// time when it returns true. Repeats within dnsAuditWindow return false. When
// the map is full it first drops expired entries; if it's still full (a burst
// of fresh unique names), it emits without caching so memory stays bounded —
// a tunnelling agent shouldn't be able to both evade the audit and exhaust
// memory.
func (d *dnsDedupe) shouldAudit(name string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = make(map[string]time.Time)
	}
	if last, ok := d.seen[name]; ok && now.Sub(last) < dnsAuditWindow {
		return false
	}
	if len(d.seen) >= dnsAuditMaxEntries {
		for k, t := range d.seen {
			if now.Sub(t) >= dnsAuditWindow {
				delete(d.seen, k)
			}
		}
		if len(d.seen) >= dnsAuditMaxEntries {
			return true // still full of fresh entries: emit, but don't grow
		}
	}
	d.seen[name] = now
	return true
}

// ServeSink runs the DNS sinkhole loop on an already-bound UDP PacketConn,
// answering every query with sinkIP, until ctx is cancelled. audit may be nil.
// sandboxd uses this to run a per-gateway sink for a packed sandbox — bound
// directly to <gateway>:53 — so the reply source is the gateway the guest
// queried (no DNAT, hence no fragile cross-netns conntrack un-NAT).
func ServeSink(ctx context.Context, pc net.PacketConn, sinkIP net.IP, audit func(string)) {
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	serveSinkUDP(pc, sinkIP.To4(), audit)
}

func serveSinkUDP(pc net.PacketConn, sinkIP net.IP, audit func(string)) {
	buf := make([]byte, 1500)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			return // listener closed on ctx cancel
		}
		resp, name, ok := buildSinkResponse(buf[:n], sinkIP)
		if !ok {
			continue // malformed query: drop, don't reflect
		}
		if audit != nil && name != "" {
			audit(name)
		}
		_, _ = pc.WriteTo(resp, client)
	}
}

// buildSinkResponse parses just enough of a DNS query to echo its header and
// question back with an answer. For A queries it appends one A record pointing
// at sinkIP; for any other qtype it returns NODATA (NOERROR, zero answers).
// Returns the response bytes, the queried name (for audit), and false if the
// query is too malformed to answer.
//
// The parser is deliberately minimal and bounds-checked: it never trusts a
// length field, never follows compression pointers (rejecting them instead of
// chasing offsets), and reads only the first question. It is the one piece of
// attacker-controlled parsing this adds, so it stays small and total.
func buildSinkResponse(query []byte, sinkIP net.IP) (resp []byte, name string, ok bool) {
	if len(query) < 12 {
		return nil, "", false
	}
	qdcount := binary.BigEndian.Uint16(query[4:6])
	if qdcount < 1 {
		return nil, "", false
	}

	// Walk the first question's QNAME to find where QTYPE/QCLASS sit.
	name, qend, ok := parseQName(query, 12)
	if !ok || qend+4 > len(query) {
		return nil, "", false
	}
	qtype := binary.BigEndian.Uint16(query[qend : qend+2])
	questionEnd := qend + 4 // QTYPE(2) + QCLASS(2)

	// Response header: copy ID, set QR=1, RD/RA mirrored, RCODE=0.
	resp = make([]byte, 0, questionEnd+16)
	hdr := make([]byte, 12)
	copy(hdr, query[:12])
	rd := query[2] & 0x01                   // preserve the client's recursion-desired bit
	hdr[2] = 0x80 | rd                      // QR=1, Opcode=0, AA=0, TC=0, RD=rd
	hdr[3] = 0x80                           // RA=1, RCODE=0 (NOERROR)
	binary.BigEndian.PutUint16(hdr[4:6], 1) // QDCOUNT=1 (echo one question)

	answer := qtype == dnsTypeA
	if answer {
		binary.BigEndian.PutUint16(hdr[6:8], 1) // ANCOUNT=1
	} else {
		binary.BigEndian.PutUint16(hdr[6:8], 0) // NODATA (e.g. AAAA → force A)
	}
	binary.BigEndian.PutUint16(hdr[8:10], 0)  // NSCOUNT
	binary.BigEndian.PutUint16(hdr[10:12], 0) // ARCOUNT
	resp = append(resp, hdr...)
	resp = append(resp, query[12:questionEnd]...) // echo the question verbatim

	if answer {
		// Answer RR: name as a compression pointer to the question (0xC00C),
		// TYPE A, CLASS IN, TTL, RDLENGTH 4, RDATA = sinkIP.
		rr := make([]byte, 0, 16)
		rr = append(rr, 0xC0, 0x0C)
		rr = append(rr, byte(dnsTypeA>>8), byte(dnsTypeA))
		rr = append(rr, 0x00, 0x01) // CLASS IN
		var ttl [4]byte
		binary.BigEndian.PutUint32(ttl[:], sinkTTL)
		rr = append(rr, ttl[:]...)
		rr = append(rr, 0x00, 0x04) // RDLENGTH
		rr = append(rr, sinkIP.To4()...)
		resp = append(resp, rr...)
	}
	return resp, name, true
}

// parseQName reads a DNS name starting at off, returning the dotted name, the
// offset just past the terminating zero label, and ok. Compression pointers
// are rejected (a query's first question is not legally compressed), and label
// lengths are bounds-checked against the buffer.
func parseQName(msg []byte, off int) (name string, end int, ok bool) {
	var out []byte
	for {
		if off >= len(msg) {
			return "", 0, false
		}
		l := int(msg[off])
		if l == 0 {
			return string(out), off + 1, true
		}
		if l&0xC0 != 0 {
			// Compression pointer or reserved bits: not expected in a query
			// question. Reject rather than chase an offset.
			return "", 0, false
		}
		off++
		if off+l > len(msg) || l > 63 {
			return "", 0, false
		}
		if len(out) > 0 {
			out = append(out, '.')
		}
		out = append(out, msg[off:off+l]...)
		off += l
		if len(out) > 255 {
			return "", 0, false
		}
	}
}

// logSinkStartup is a tiny helper so callers can log a consistent line.
func logSinkStartup(addr string, sinkIP net.IP) {
	log.Printf("sbxproxy DNS sink listening on %s, answering all queries with %s", addr, sinkIP)
}
