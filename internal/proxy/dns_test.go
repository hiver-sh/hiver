package proxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"
)

// dnsQuery builds a minimal single-question DNS query for name/qtype.
func dnsQuery(name string, qtype uint16) []byte {
	var b bytes.Buffer
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:2], 0x1234) // ID
	hdr[2] = 0x01                                // RD set
	binary.BigEndian.PutUint16(hdr[4:6], 1)      // QDCOUNT
	b.Write(hdr)
	for _, label := range splitLabels(name) {
		b.WriteByte(byte(len(label)))
		b.WriteString(label)
	}
	b.WriteByte(0) // root label
	var tc [4]byte
	binary.BigEndian.PutUint16(tc[0:2], qtype)
	binary.BigEndian.PutUint16(tc[2:4], 1) // CLASS IN
	b.Write(tc[:])
	return b.Bytes()
}

func splitLabels(name string) []string {
	var out []string
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			out = append(out, name[start:i])
			start = i + 1
		}
	}
	out = append(out, name[start:])
	return out
}

func TestBuildSinkResponse_A(t *testing.T) {
	sink := net.IPv4(192, 0, 2, 1)
	resp, name, ok := buildSinkResponse(dnsQuery("api.github.com", dnsTypeA), sink.To4())
	if !ok {
		t.Fatal("expected ok")
	}
	if name != "api.github.com" {
		t.Fatalf("name = %q, want api.github.com", name)
	}
	// QR bit set, NOERROR, ANCOUNT 1.
	if resp[2]&0x80 == 0 {
		t.Error("QR bit not set")
	}
	if rcode := resp[3] & 0x0F; rcode != 0 {
		t.Errorf("RCODE = %d, want 0", rcode)
	}
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 1 {
		t.Fatalf("ANCOUNT = %d, want 1", an)
	}
	// The placeholder IP must appear as the last 4 bytes (RDATA).
	if got := resp[len(resp)-4:]; !bytes.Equal(got, sink.To4()) {
		t.Errorf("RDATA = %v, want %v", got, sink.To4())
	}
	// RD bit must be echoed back so stub resolvers are happy.
	if resp[2]&0x01 == 0 {
		t.Error("RD bit not echoed")
	}
}

func TestBuildSinkResponse_AAAA_NoData(t *testing.T) {
	sink := net.IPv4(192, 0, 2, 1).To4()
	resp, name, ok := buildSinkResponse(dnsQuery("example.com", dnsTypeAAAA), sink)
	if !ok {
		t.Fatal("expected ok")
	}
	if name != "example.com" {
		t.Fatalf("name = %q", name)
	}
	// NODATA: NOERROR with zero answers, so clients fall back to A.
	if rcode := resp[3] & 0x0F; rcode != 0 {
		t.Errorf("RCODE = %d, want 0 (NOERROR)", rcode)
	}
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 0 {
		t.Errorf("ANCOUNT = %d, want 0", an)
	}
}

func TestBuildSinkResponse_Malformed(t *testing.T) {
	cases := map[string][]byte{
		"too short":       {0x12, 0x34},
		"truncated qname": {0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0x05, 'a', 'b'},
		"zero qdcount":    append(make([]byte, 12), 0),
		"compression ptr": {0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xC0, 0x0C, 0, 1, 0, 1},
	}
	for label, q := range cases {
		if _, _, ok := buildSinkResponse(q, net.IPv4(192, 0, 2, 1).To4()); ok {
			t.Errorf("%s: expected !ok", label)
		}
	}
}

func TestDNSDedupe(t *testing.T) {
	var d dnsDedupe
	t0 := time.Unix(1700000000, 0)

	if !d.shouldAudit("api.github.com", t0) {
		t.Fatal("first lookup should audit")
	}
	if d.shouldAudit("api.github.com", t0.Add(time.Second)) {
		t.Error("repeat within window should be suppressed")
	}
	if !d.shouldAudit("pypi.org", t0.Add(time.Second)) {
		t.Error("distinct name should audit even within window")
	}
	if !d.shouldAudit("api.github.com", t0.Add(dnsAuditWindow+time.Second)) {
		t.Error("repeat after window should audit again")
	}
}

func TestDNSDedupe_BoundedUnderFlood(t *testing.T) {
	var d dnsDedupe
	t0 := time.Unix(1700000000, 0)
	// Spray more unique fresh names than the cap; the map must not exceed it.
	for i := 0; i < dnsAuditMaxEntries*2; i++ {
		d.shouldAudit(fmt.Sprintf("n%d.exfil.example.com", i), t0)
	}
	d.mu.Lock()
	n := len(d.seen)
	d.mu.Unlock()
	if n > dnsAuditMaxEntries {
		t.Fatalf("dedupe map grew to %d, want <= %d", n, dnsAuditMaxEntries)
	}
}

func TestUpstreamAddr(t *testing.T) {
	tests := []struct {
		host, origDst, want string
	}{
		// SNI/Host name present: dial the name with the real port from origDst.
		{"api.github.com", "192.0.2.1:443", "api.github.com:443"},
		{"api.github.com:443", "192.0.2.1:443", "api.github.com:443"},
		// No name (fell back to origDst): dial origDst unchanged (IP literal).
		{"10.0.0.5:8080", "10.0.0.5:8080", "10.0.0.5:8080"},
		// Unknown port → fall back to origDst rather than guess.
		{"api.github.com", "192.0.2.1", "192.0.2.1"},
	}
	for _, tt := range tests {
		if got := upstreamAddr(tt.host, tt.origDst); got != tt.want {
			t.Errorf("upstreamAddr(%q, %q) = %q, want %q", tt.host, tt.origDst, got, tt.want)
		}
	}
}
