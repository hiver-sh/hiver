package e2e_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestEgressNonTCPBlockedE2E verifies that the sandbox firewall drops all
// non-TCP workload egress (UDP and ICMP are exercised here) while leaving TCP
// egress intact.
//
// The workload has no non-TCP egress path: TCP is funneled to the proxy and
// DNS is answered by the local sink, so any UDP or ICMP a workload emits toward
// a real off-box address is meant to be dropped outright. ICMP matters
// specifically because the workload holds CAP_NET_RAW — without the drop it
// could open a raw socket and tunnel data out over a ping, bypassing the proxy
// entirely.
//
// The host binds two listeners on the SAME random port — a TCP HTTP server and
// a UDP packet socket — and exposes that host:port to the sandbox via an
// ExtraHosts host-gateway alias (so the name resolves through /etc/hosts,
// independent of the DNS sinkhole). The sandbox then:
//
//   - TCP (positive control): an HTTP GET reaches the capture server, proving
//     the alias resolves and the host:port is actually reachable.
//   - UDP: several datagrams are sent to the same host:port; none must arrive,
//     proving the drop is protocol-specific, not just unreachability.
//   - ICMP: a raw echo request is sent to the same host and must not round-trip
//     a reply, proving the CAP_NET_RAW ping-tunnel path is closed too.
func TestEgressNonTCPBlockedE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	// TCP capture server. Bound on all interfaces so Docker can reach it via
	// the host-gateway alias.
	tcpL, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	capturePort := tcpL.Addr().(*net.TCPAddr).Port

	tcpHitCh := make(chan struct{}, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case tcpHitCh <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = srv.Serve(tcpL) }()
	t.Cleanup(func() { _ = srv.Close() })

	// UDP capture socket on the same port number (TCP and UDP ports are
	// independent). Any datagram that reaches it means UDP egress leaked.
	udpConn, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", capturePort))
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = udpConn.Close() })

	udpHitCh := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := udpConn.ReadFrom(buf)
			if err != nil {
				return // socket closed on cleanup
			}
			if n > 0 {
				select {
				case udpHitCh <- struct{}{}:
				default:
				}
			}
		}
	}()

	const hostAlias = "udp-egress-target"
	key := fmt.Sprintf("e2e-egress-udp-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "python",
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		// Resolve the host-side capture server through /etc/hosts so the name
		// lands on the real host-gateway IP rather than the DNS sink.
		ExtraHosts: []string{
			hostAlias + ":host-gateway",
		},
		// Allow TCP to the capture host so the positive control can reach it
		// through the proxy. UDP has no allowlist — it is dropped unconditionally.
		Egress: []hiverclient.EgressRule{
			{
				Access: "allow",
				Host:   hostAlias,
				Ports:  []int{capturePort},
			},
		},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}
	// Tear the sandbox down via its own API (no controller involvement).
	t.Cleanup(func() { _ = sbx.Shutdown(context.Background()) })

	// ── Positive control: TCP reaches the host ────────────────────────────────
	tcpCmd := fmt.Sprintf(
		`python3 -c "import urllib.request; urllib.request.urlopen('http://%s:%d/probe', timeout=10)"`,
		hostAlias, capturePort,
	)
	tcpRes, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: tcpCmd})
	if err != nil {
		t.Fatalf("Exec (tcp): %v", err)
	}
	if tcpRes.ExitCode != 0 {
		t.Fatalf("TCP control request failed (exit=%d stderr=%q): host:port not reachable, UDP result would be meaningless",
			tcpRes.ExitCode, tcpRes.Stderr)
	}
	select {
	case <-tcpHitCh:
		// host reachable over TCP — good.
	case <-time.After(5 * time.Second):
		t.Fatal("TCP capture server did not receive the control request")
	}

	// ── UDP egress must be dropped ────────────────────────────────────────────
	// The block manifests differently per backend, so the send's exit code is
	// not the invariant — only host silence is:
	//   - container (shared netns): the filter OUTPUT DROP makes the workload's
	//     sendto fail immediately with EPERM ("Operation not permitted"); the
	//     packet never leaves.
	//   - microvm: the datagram leaves the guest and is dropped on the tap's
	//     FORWARD chain, so the guest sendto succeeds but nothing egresses.
	// Either way the host capture socket must see nothing.
	//
	// Three independent sends via a shell loop: a per-send python invocation
	// means a local EPERM rejection (`|| true`) doesn't stop the rest, and the
	// burst guards against a single datagram being lost legitimately.
	udpCmd := fmt.Sprintf(
		`for i in 1 2 3; do python3 -c "import socket; socket.socket(socket.AF_INET, socket.SOCK_DGRAM).sendto(b'leak', ('%s', %d))" 2>&1 || true; done`,
		hostAlias, capturePort,
	)
	udpRes, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: udpCmd})
	if err != nil {
		t.Fatalf("Exec (udp): %v", err)
	}
	t.Logf("UDP send result: exit=%d output=%q (a local EPERM rejection is an expected manifestation of the block)",
		udpRes.ExitCode, udpRes.Stdout)

	select {
	case <-udpHitCh:
		t.Fatal("UDP datagram reached the host: workload UDP egress is NOT blocked")
	case <-time.After(3 * time.Second):
		// No datagram arrived — UDP egress is blocked as intended.
	}

	// ── ICMP egress must be dropped (CAP_NET_RAW ping-tunnel) ─────────────────
	// No host listener is needed: the host kernel auto-replies to echo requests,
	// so a working ICMP egress path would round-trip a reply. We send a raw echo
	// request from the workload (which holds CAP_NET_RAW) and require that no
	// reply comes back. The block manifests per backend but the invariant is the
	// same "no reply":
	//   - container: the filter OUTPUT drop fails the sendto with EPERM; the echo
	//     never leaves, so nothing replies.
	//   - microvm: the echo leaves the guest but is dropped on the tap's FORWARD
	//     chain, so it never reaches the host and nothing replies.
	// The script prints REPLY only if a datagram actually returns; a send error
	// (EPERM) or a recv timeout both print BLOCKED. The TCP control above already
	// proved the alias resolves to a real, reachable host, so "no reply" here is
	// the drop, not a bogus address.
	// The host comes in via an env var, not fmt.Sprintf, so the % verbs in the
	// Python body aren't mangled by Go's formatter.
	icmpCmd := "TARGET=" + hostAlias + ` python3 - <<'PY'
import socket, struct, os, sys
host = os.environ['TARGET']
def csum(b):
    if len(b) % 2: b += b'\x00'
    s = sum(struct.unpack('!%dH' % (len(b)//2), b))
    s = (s >> 16) + (s & 0xffff); s += s >> 16
    return ~s & 0xffff
try:
    s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)
except OSError as e:
    print("BLOCKED", "socket:", e); sys.exit(0)
s.settimeout(3)
hdr = struct.pack('!BBHHH', 8, 0, 0, os.getpid() & 0xffff, 1)
pkt = struct.pack('!BBHHH', 8, 0, csum(hdr + b'leak'), os.getpid() & 0xffff, 1) + b'leak'
try:
    s.sendto(pkt, (host, 0))
except OSError as e:
    print("BLOCKED", "sendto:", e); sys.exit(0)
try:
    s.recvfrom(2048)
    print("REPLY")
except socket.timeout:
    print("BLOCKED", "timeout")
except OSError as e:
    print("BLOCKED", "recv:", e)
PY`
	icmpRes, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: icmpCmd})
	if err != nil {
		t.Fatalf("Exec (icmp): %v", err)
	}
	t.Logf("ICMP probe result: exit=%d output=%q", icmpRes.ExitCode, icmpRes.Stdout)
	if strings.Contains(icmpRes.Stdout, "REPLY") {
		t.Fatal("ICMP echo round-tripped a reply: workload ICMP egress is NOT blocked")
	}
}
