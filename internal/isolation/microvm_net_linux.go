//go:build linux

package isolation

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/hiver-sh/hiver/internal/proxy"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// runCmd runs argv and returns its trimmed combined output (for error context).
func runCmd(ctx context.Context, argv []string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	return bytes.TrimSpace(out), err
}

// Per-VM network for a packed microvm (design §7, "option 1"). Unlike the
// original model — one tap per VM in the shared pod netns, each guest re-IP'd to
// a distinct 172.16.<n>.2 so sbxproxy could key egress per source — every packed
// VM here keeps the base snapshot's baked guest IP (bootGuestIP) and runs in its
// own network namespace. Reusing 172.16.0.0/30 in every netns is safe because the
// namespaces are isolated, and because the guest IP never changes a prewarmed
// resident workload's sockets/DNS/TLS stay valid across resume. The host SNATs
// each VM's egress to its sourceIP (172.16.<n>.2) on the way to the pod netns, so
// the shared sbxproxy still keys egress rules per sandbox exactly as before.
//
// Topology (octet n, sourceIP = 172.16.<n>.2):
//
//	guest(bootGuestIP) ──tap(bootGatewayIP)── [netns fcsbx<n>] ──vp<n>(sourceIP)
//	                                                                  │ veth
//	[pod netns] ──vh<n>(172.16.<n>.1)── DNAT tcp → 127.0.0.1:proxyPort (sbxproxy)
//
// DNS: the guest's baked nameserver is bootGatewayIP (the tap), so a per-VM sink
// bound inside the netns at bootGatewayIP:53 answers it locally — no cross-netns
// DNS NAT, mirroring the container backend's "sink bound to the gateway" design.
//
// The link plumbing is done over netlink rather than by forking iproute2 once per
// command, and the firewall rules are loaded with a single (ip6)tables-restore per
// (netns-context × family) rather than one fork per rule. Both cut the per-claim
// process spawns this path used to pay (~22 → a handful) and, for iptables, the
// number of times the kernel-wide xtables lock is taken — the dominant cost on the
// resume/claim critical path. Only the tap is still created with `ip tuntap`, so
// its device flags exactly match what firecracker re-attaches to on resume.

// setupPackedNetMicrovm creates the netns, veth, and tap, installs the SNAT/DNAT
// rules, and starts the in-netns DNS sink. firecracker is launched into this netns
// by the cgroup wrap (see cgroupWrap/cgroupUnshareWrap), since it must open the tap
// there. ctx bounds the DNS sink's lifetime (the sandbox context).
func (m *microvm) setupPackedNetMicrovm(ctx context.Context, proxyPort, dnsPort, mark int) error {
	ns := m.netnsName
	n := netID(m.sourceIP)
	vh := "vh" + n                     // host (pod netns) end of the veth
	vp := "vp" + n                     // netns end, carries sourceIP
	hostGW := guestGateway(m.sourceIP) // 172.16.<n>.1 (pod-side veth)
	src := m.sourceIP                  // 172.16.<n>.2 (SNAT identity)
	tapGW := m.gatewayIP               // bootGatewayIP — the guest's baked gateway (tap IP)

	m.teardownPackedNetMicrovm(ctx) // discard any leak from a prior incarnation

	// ip_forward / route_localnet in the pod netns: cheap idempotent file writes
	// (no fork), needed for the DNAT-to-loopback delivery and tap→vp forwarding.
	if err := enableIPForward(); err != nil {
		return err
	}
	if err := enableRouteLocalnet(); err != nil {
		return err
	}

	// Link plumbing via netlink: create the netns (enabling its own ip_forward so
	// it routes guest tap→vp), wire the routed veth pair, address vp/lo, and add the
	// default route.
	if err := wirePackedLinks(ns, vh, vp, hostGW, src, true); err != nil {
		return fmt.Errorf("wire packed net %s: %w", ns, err)
	}

	// The guest's tap is created with iproute2 (not netlink) so its device flags
	// exactly match the persistent tap firecracker re-attaches to on resume — the
	// one data-path device we don't recreate with a different code path. Its
	// address/up are then set over netlink in the netns.
	if out, err := runCmd(ctx, []string{"ip", "netns", "exec", ns, "ip", "tuntap", "add", "dev", m.tapName, "mode", "tap"}); err != nil {
		return fmt.Errorf("create tap %s: %w (%s)", m.tapName, err, out)
	}
	if err := addrUpInNetns(ns, m.tapName, tapGW); err != nil {
		return fmt.Errorf("address tap %s: %w", m.tapName, err)
	}

	// Firewall rules, batched: one iptables-restore for this netns (nat POSTROUTING
	// SNAT + filter FORWARD drop), one for the pod netns (nat PREROUTING DNAT +
	// OUTPUT mark RETURN), and one ip6tables-restore for the in-netns v6 drop. All
	// use --noflush so other VMs' rules in the shared pod netns are preserved.
	if err := loadNetnsRules(ctx, ns, vp, m.tapName, src, m.guestIP); err != nil {
		return err
	}
	if err := loadPodRules(ctx, vh, proxyPort, mark); err != nil {
		return err
	}
	if err := loadNetnsRules6(ctx, ns, m.tapName); err != nil {
		return err
	}

	// Per-VM DNS sink bound inside the netns at the guest's baked nameserver
	// (bootGatewayIP:53), answering every lookup with the placeholder the proxy
	// re-resolves. Bound in the netns so no cross-netns DNS NAT is needed.
	pc, err := listenUDPInNetns(ns, tapGW+":53")
	if err != nil {
		return fmt.Errorf("dns sink in netns %s: %w", ns, err)
	}
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	go proxy.ServeSink(ctx, pc, net.ParseIP("192.0.2.1"), nil)
	return nil
}

// wirePackedLinks creates the named netns and wires the routed veth pair into it
// over netlink (no iproute2 fork per command): the host end (vh) is addressed with
// hostGW in the pod netns, the peer (vp) is moved into the netns and addressed with
// peerIP, and a default route via hostGW is added. When netnsForward is set the
// in-netns ip_forward sysctl is enabled too (the microvm path needs it to route the
// guest's tap→vp; the container path originates traffic on vp itself and does not).
//
// It runs on a locked OS thread because it briefly enters the new netns — NewNamed
// leaves the thread there, and the ip_forward sysctl must be written from inside it.
// The thread is returned to the original (pod) netns before unlocking; if that
// restore fails the thread is left locked (poisoned) so the Go runtime retires it
// rather than reusing a thread stuck in the VM netns, mirroring listenUDPInNetns.
func wirePackedLinks(ns, vh, vp, hostGW, peerIP string, netnsForward bool) (retErr error) {
	runtime.LockOSThread()
	keepLocked := false
	defer func() {
		if !keepLocked {
			runtime.UnlockOSThread()
		}
	}()

	orig, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer orig.Close()

	// NewNamed creates /run/netns/<ns> and switches this thread into the new netns.
	nsh, err := netns.NewNamed(ns)
	if err != nil {
		return fmt.Errorf("create netns: %w", err)
	}
	defer nsh.Close()
	// Optionally enable forwarding inside the new netns (the thread sits in it now),
	// then return to the pod netns for the veth work.
	var werr error
	if netnsForward {
		werr = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0)
	}
	if serr := netns.Set(orig); serr != nil {
		keepLocked = true // thread is stuck in the new netns; retire it
		return fmt.Errorf("restore netns: %w", serr)
	}
	if werr != nil {
		return fmt.Errorf("netns ip_forward: %w", werr)
	}

	// Pod netns: create the veth pair, address + up the host end, move the peer in.
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: vh}, PeerName: vp}); err != nil {
		return fmt.Errorf("add veth %s/%s: %w", vh, vp, err)
	}
	vhLink, err := netlink.LinkByName(vh)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", vh, err)
	}
	if err := addrUp(vhLink, hostGW); err != nil {
		return fmt.Errorf("address %s: %w", vh, err)
	}
	vpLink, err := netlink.LinkByName(vp)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", vp, err)
	}
	if err := netlink.LinkSetNsFd(vpLink, int(nsh)); err != nil {
		return fmt.Errorf("move %s into %s: %w", vp, ns, err)
	}

	// Inside the netns, via a handle bound to it (pure netlink — no thread switch):
	// address vp, bring up vp + lo, add the default route via the pod-side gateway.
	h, err := netlink.NewHandleAt(nsh)
	if err != nil {
		return fmt.Errorf("netns handle: %w", err)
	}
	defer h.Close()

	vpInner, err := h.LinkByName(vp)
	if err != nil {
		return fmt.Errorf("lookup %s in %s: %w", vp, ns, err)
	}
	addr, err := netlink.ParseAddr(peerIP + "/30")
	if err != nil {
		return err
	}
	if err := h.AddrAdd(vpInner, addr); err != nil {
		return fmt.Errorf("address %s: %w", vp, err)
	}
	if err := h.LinkSetUp(vpInner); err != nil {
		return fmt.Errorf("up %s: %w", vp, err)
	}
	lo, err := h.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("lookup lo in %s: %w", ns, err)
	}
	if err := h.LinkSetUp(lo); err != nil {
		return fmt.Errorf("up lo: %w", err)
	}
	if err := h.RouteAdd(&netlink.Route{Gw: net.ParseIP(hostGW)}); err != nil {
		return fmt.Errorf("default route via %s: %w", hostGW, err)
	}
	return nil
}

// addrUp assigns ip/30 to link and brings it up, in the caller's current netns.
func addrUp(link netlink.Link, ip string) error {
	addr, err := netlink.ParseAddr(ip + "/30")
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}

// addrUpInNetns assigns ip/30 to dev and brings it up inside the named netns,
// using a netlink handle bound to that netns (no thread switch, no fork).
func addrUpInNetns(ns, dev, ip string) error {
	nsh, err := netns.GetFromName(ns)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", ns, err)
	}
	defer nsh.Close()
	h, err := netlink.NewHandleAt(nsh)
	if err != nil {
		return err
	}
	defer h.Close()
	link, err := h.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("lookup %s in %s: %w", dev, ns, err)
	}
	addr, err := netlink.ParseAddr(ip + "/30")
	if err != nil {
		return err
	}
	if err := h.AddrAdd(link, addr); err != nil {
		return err
	}
	return h.LinkSetUp(link)
}

// loadNetnsRules installs this VM's in-netns NAT/filter rules with one
// iptables-restore: SNAT guest egress leaving via vp to sourceIP (so the pod netns
// and the shared sbxproxy see a distinct per-sandbox source), and drop non-TCP
// guest egress reaching FORWARD (DNS is delivered locally to the in-netns sink;
// guest TCP is forwarded out vp; everything else — UDP, ICMP, raw — has no path).
func loadNetnsRules(ctx context.Context, ns, vp, tap, src, guestIP string) error {
	// nat: POSTROUTING SNATs the guest's egress to this VM's source identity (src).
	// PREROUTING DNATs host-initiated ingress onward to the guest: the pod-side proxy
	// dials the sandbox at src (the netns veth address), but the workload listens in
	// the guest at its baked tap IP (guestIP), so without this the SYN hits the
	// netns's own veth stack and is RST. DNATing all ingress TCP to the guest makes
	// /proxy/<port> (e.g. the resident browser host on :9223) reach the in-guest
	// listener. filter: drop non-TCP forwarded from the guest tap.
	rules := fmt.Sprintf("*nat\n"+
		"-A POSTROUTING -o %s -j SNAT --to-source %s\n"+
		"-A PREROUTING -i %s -d %s -p tcp -j DNAT --to-destination %s\n"+
		"COMMIT\n"+
		"*filter\n"+
		"-A FORWARD -i %s ! -p tcp -j DROP\n"+
		"COMMIT\n", vp, src, vp, src, guestIP, tap)
	if out, err := runRestore(ctx, "iptables-restore", ns, rules); err != nil {
		return fmt.Errorf("netns %s iptables-restore: %w (%s)", ns, err, out)
	}
	return nil
}

// loadPodRules installs the pod-netns NAT rules with one iptables-restore: DNAT
// this VM's forwarded TCP (src sourceIP, arriving on vh) to the shared sbxproxy on
// loopback, and let proxy-originated upstream traffic (stamped with SO_MARK) escape
// the redirect so it isn't looped back.
func loadPodRules(ctx context.Context, vh string, proxyPort, mark int) error {
	rules := fmt.Sprintf("*nat\n"+
		"-A PREROUTING -i %s -p tcp -j DNAT --to-destination 127.0.0.1:%d\n"+
		"-A OUTPUT -m mark --mark 0x%x -j RETURN\n"+
		"COMMIT\n", vh, proxyPort, mark)
	if out, err := runRestore(ctx, "iptables-restore", "", rules); err != nil {
		return fmt.Errorf("pod iptables-restore: %w (%s)", err, out)
	}
	return nil
}

// loadNetnsRules6 drops guest IPv6 egress in the netns (the v4 DNAT/proxy path has
// no v6 peer). A kernel without IPv6 is not an error — there is nothing to block.
func loadNetnsRules6(ctx context.Context, ns, tap string) error {
	rules := fmt.Sprintf("*filter\n"+
		"-A FORWARD -i %s -j DROP\n"+
		"COMMIT\n", tap)
	out, err := runRestore(ctx, "ip6tables-restore", ns, rules)
	if err != nil {
		if bytes.Contains(out, []byte("Address family not supported")) || bytes.Contains(out, []byte("not supported")) {
			return nil
		}
		return fmt.Errorf("netns %s ip6tables-restore: %w (%s)", ns, err, out)
	}
	return nil
}

// runRestore feeds rules to (ip6)tables-restore on stdin with --noflush, so it
// appends to the live tables instead of replacing them (other VMs share the pod
// netns). When ns is non-empty it runs inside that netns via `ip netns exec`.
func runRestore(ctx context.Context, bin, ns, rules string) ([]byte, error) {
	var argv []string
	if ns != "" {
		argv = []string{"ip", "netns", "exec", ns, bin, "--noflush"}
	} else {
		argv = []string{bin, "--noflush"}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	return bytes.TrimSpace(out), err
}

// teardownPackedNetMicrovm removes the netns (which drops the tap, vp, and the
// in-netns rules) and the pod-side veth + per-veth pod-netns DNAT rule. Best-effort.
func (m *microvm) teardownPackedNetMicrovm(ctx context.Context) {
	if m.netnsName == "" {
		return
	}
	n := netID(m.sourceIP)
	vh := "vh" + n
	// Deleting the netns drops the tap, vp, and all in-netns rules with it. If the
	// name exists as a stale unmounted file (DeleteNamed only unmounts+removes a
	// live mount), remove it so a fresh NewNamed's O_EXCL create succeeds.
	if err := netns.DeleteNamed(m.netnsName); err != nil {
		_ = os.Remove(filepath.Join("/run/netns", m.netnsName))
	}
	// The host-side veth lives in the pod netns (its peer left with the netns).
	if link, err := netlink.LinkByName(vh); err == nil {
		_ = netlink.LinkDel(link)
	}
	// The pod-netns PREROUTING DNAT for this veth must be deleted explicitly
	// (restore --noflush only appends). Best-effort.
	_, _ = runCmd(ctx, []string{
		"iptables", "-t", "nat", "-D", "PREROUTING", "-i", vh, "-p", "tcp",
		"-j", "DNAT", "--to-destination", fmt.Sprintf("127.0.0.1:%d", m.proxyPort),
	})
}

// listenUDPInNetns binds a UDP socket inside the named network namespace and
// returns it. The bind runs on a locked OS thread that briefly setns()es into the
// target netns and back; the socket, once created, stays bound to that netns
// regardless of the thread's namespace. If the restore setns fails the thread is
// left locked (poisoned) so the Go runtime retires it rather than reusing a thread
// stuck in the VM netns.
func listenUDPInNetns(nsName, addr string) (net.PacketConn, error) {
	type result struct {
		pc  net.PacketConn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		keepLocked := true
		defer func() {
			if !keepLocked {
				runtime.UnlockOSThread()
			}
		}()
		cur, err := netns.Get()
		if err != nil {
			ch <- result{nil, fmt.Errorf("get current netns: %w", err)}
			return
		}
		defer cur.Close()
		target, err := netns.GetFromName(nsName)
		if err != nil {
			ch <- result{nil, fmt.Errorf("open netns %s: %w", nsName, err)}
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target), unix.CLONE_NEWNET); err != nil {
			ch <- result{nil, fmt.Errorf("setns %s: %w", nsName, err)}
			return
		}
		pc, lerr := net.ListenPacket("udp", addr)
		// Restore the thread's netns; only a clean restore makes it safe to reuse.
		if err := unix.Setns(int(cur), unix.CLONE_NEWNET); err == nil {
			keepLocked = false
		}
		ch <- result{pc, lerr}
	}()
	r := <-ch
	return r.pc, r.err
}

// listenTCPInNetns binds a TCP listener at addr inside the named netns (or the
// current/pod netns when nsName is empty), mirroring listenUDPInNetns. Used to
// host each VM's per-mount 9p endpoint on the netns gateway (bootGatewayIP),
// which the guest dials — 9p is guest-initiated.
func listenTCPInNetns(nsName, addr string) (net.Listener, error) {
	if nsName == "" {
		return net.Listen("tcp", addr)
	}
	type result struct {
		ln  net.Listener
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		keepLocked := true
		defer func() {
			if !keepLocked {
				runtime.UnlockOSThread()
			}
		}()
		cur, err := netns.Get()
		if err != nil {
			ch <- result{nil, fmt.Errorf("get current netns: %w", err)}
			return
		}
		defer cur.Close()
		target, err := netns.GetFromName(nsName)
		if err != nil {
			ch <- result{nil, fmt.Errorf("open netns %s: %w", nsName, err)}
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target), unix.CLONE_NEWNET); err != nil {
			ch <- result{nil, fmt.Errorf("setns %s: %w", nsName, err)}
			return
		}
		ln, lerr := net.Listen("tcp", addr)
		if err := unix.Setns(int(cur), unix.CLONE_NEWNET); err == nil {
			keepLocked = false
		}
		ch <- result{ln, lerr}
	}()
	r := <-ch
	return r.ln, r.err
}

// dialGuest opens a TCP connection to one of the guest agent's host->guest ports
// (control/exec/files/ready). For a packed VM it dials the VM's source IP stamped
// with the egress SO_MARK, so the netns ingress DNAT carries it to the guest and
// the pod REDIRECT exempts it (the same path the ingress proxy uses); for the
// base/boot VM (pod netns) it dials the guest's tap IP directly. Replaces the
// former Firecracker host-initiated vsock CONNECT, which did not survive resume.
func (m *microvm) dialGuest(ctx context.Context, port uint32) (net.Conn, error) {
	host := m.guestIP
	if m.sourceIP != "" {
		host = m.sourceIP
	}
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, m.mark)
		})
	}}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
}
