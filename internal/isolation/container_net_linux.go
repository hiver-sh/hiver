//go:build linux

package isolation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Pod-local network for packed sandboxes (design §6). Each packed sandbox gets a
// routed veth pair — no shared bridge — exactly mirroring the microvm tap model
// (internal/isolation/microvm.go): the host end carries the gateway IP of a
// dedicated /30, the container end carries the guest IP, and the host DNATs the
// guest's forwarded egress to sbxproxy on loopback. A bridge was avoided on
// purpose: br_netfilter mangles the DNAT reply path, whereas a routed veth (like
// the tap) does not.
//
// The /30 is 172.16.<n>.0/30 where <n> is the sandbox's index (the 3rd octet of
// its guest IP 172.16.<n>.2); the gateway is 172.16.<n>.1. Distinct guest IPs let
// the shared sbxproxy key egress rules per sandbox.

// guestGateway returns the host-side gateway IP for a packed guest IP
// (172.16.<n>.2 → 172.16.<n>.1).
func guestGateway(guestIP string) string {
	if i := strings.LastIndexByte(guestIP, '.'); i >= 0 {
		return guestIP[:i] + ".1"
	}
	return guestIP
}

// setupPackedNet brings up this sandbox's netns + routed veth and installs the
// host DNAT rules that funnel its egress to sbxproxy/the DNS sink on loopback.
func (c *container) setupPackedNet(ctx context.Context, proxyPort, dnsPort int) error {
	id := netID(c.guestIP) // the per-sandbox index (3rd octet)
	ns := c.netnsName
	vh := "vh" + id // host end
	vc := "vc" + id // container end
	gw := guestGateway(c.guestIP)

	c.teardownPackedNet(ctx) // recreate cleanly in case a prior incarnation leaked

	// Link plumbing via netlink (no iproute2 fork per command): create the netns,
	// wire the routed veth pair, give the container end (vc) the guest IP, and add
	// its default route via the gateway. No in-netns ip_forward — the container
	// originates its egress on vc directly rather than forwarding it.
	if err := wirePackedLinks(ns, vh, vc, gw, c.guestIP, false); err != nil {
		return fmt.Errorf("wire packed net %s: %w", ns, err)
	}

	// Pod-netns rules, batched into a single iptables-restore (instead of one fork
	// per rule): DNAT all TCP off this veth to sbxproxy (which binds 0.0.0.0 in pack
	// mode, so it receives on this veth's gateway IP), and drop non-TCP egress —
	// TCP+DNS were DNAT'd/delivered locally and never reach FORWARD. DNS (UDP/53) is
	// NOT DNAT'd: sandboxd binds a sink at <gateway>:53, so the reply source is the
	// address the guest queried and no cross-netns conntrack un-NAT is needed.
	if err := loadContainerPodRules(ctx, vh, gw, proxyPort); err != nil {
		return err
	}

	// route_localnet lets the kernel deliver the DNAT-to-127.0.0.1 packets
	// instead of dropping them as martians. The pod is created with
	// conf.all.route_localnet=1 (a --sysctl), so this is normally already set and
	// /proc/sys is read-only; only write (best-effort) if it isn't.
	const rl = "/proc/sys/net/ipv4/conf/all/route_localnet"
	if v, err := os.ReadFile(rl); err != nil || strings.TrimSpace(string(v)) != "1" {
		if err := os.WriteFile(rl, []byte("1"), 0); err != nil {
			return fmt.Errorf("enable route_localnet: %w", err)
		}
	}
	return nil
}

// loadContainerPodRules installs the packed container's pod-netns NAT/filter rules
// with one iptables-restore: DNAT its forwarded TCP (arriving on vh) to sbxproxy on
// the gateway IP, and drop non-TCP egress reaching FORWARD. --noflush (in
// runRestore) preserves the other sandboxes' rules in the shared pod netns.
func loadContainerPodRules(ctx context.Context, vh, gw string, proxyPort int) error {
	rules := fmt.Sprintf("*nat\n"+
		"-A PREROUTING -i %s -p tcp -j DNAT --to-destination %s:%d\n"+
		"COMMIT\n"+
		"*filter\n"+
		"-A FORWARD -i %s ! -p tcp -j DROP\n"+
		"COMMIT\n", vh, gw, proxyPort, vh)
	if out, err := runRestore(ctx, "iptables-restore", "", rules); err != nil {
		return fmt.Errorf("pod iptables-restore: %w (%s)", err, out)
	}
	return nil
}

// teardownPackedNet removes this sandbox's netns + host veth (which also drops
// its peer and the netns's rules) and the per-veth pod-netns FORWARD drop.
// Best-effort.
func (c *container) teardownPackedNet(ctx context.Context) {
	id := netID(c.guestIP)
	vh := "vh" + id
	// Deleting the netns drops vc and its in-netns state; remove a stale unmounted
	// name (DeleteNamed only unmounts+removes a live mount) so a fresh NewNamed's
	// O_EXCL create succeeds.
	if err := netns.DeleteNamed(c.netnsName); err != nil {
		_ = os.Remove(filepath.Join("/run/netns", c.netnsName))
	}
	// The host-side veth lives in the pod netns (its peer left with the netns).
	if link, err := netlink.LinkByName(vh); err == nil {
		_ = netlink.LinkDel(link)
	}
	// The pod-netns FORWARD drop for this veth must be deleted explicitly
	// (restore --noflush only appends). Best-effort.
	_, _ = runCmd(ctx, []string{
		"iptables", "-t", "filter", "-D", "FORWARD", "-i", vh, "!", "-p", "tcp", "-j", "DROP",
	})
}
