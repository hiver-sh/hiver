package isolation

import (
	"strings"
)

// The microvm guest has its own network stack, so it can't reach the pod's
// loopback resolver. Its resolv.conf therefore points at the tap gateway, and
// the backend's RedirectEgress DNATs both UDP/53 and TCP/53 from the tap to the
// in-pod DNS sinkhole (sbxproxy's -dns-addr). Every guest lookup is answered
// with a constant placeholder, so DNS carries no data off-box; the agent then
// connects to the placeholder and the proxy re-resolves the real host itself.

const guestDNSPort = "53"

// resolvConfForGuest rewrites the pod's resolv.conf for the guest: the pod's
// nameservers (e.g. the unreachable 127.0.0.11) are replaced with the tap
// gateway, where the backend DNATs DNS to the in-pod sinkhole. search/options/
// comment lines are preserved so short-name resolution matches the pod.
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
