// Command sbxproxy is the per-sandbox MITM egress proxy.
//
// Scope (T56, T58, T59, T68): HTTP forward proxy + HTTPS via
// CONNECT tunneling, host-pattern allowlist with optional method/path
// filters and request-header overrides, JSON-line audit log, default
// inbound auth-header stripping.
//
// Out of scope for v0: TLS body interception (T57), credential broker
// (T60), body inspectors (T61–T65), per-(sandbox,dest) rate limiting (T67).
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/hiver-sh/hiver/internal/proxy"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:3128", "listen address")
		rulesPath   = flag.String("rules", "", "path to JSON file containing []proxy.EgressRule")
		auditPath   = flag.String("audit", "", "audit log file (default: stderr)")
		eventsFD    = flag.Int("events-fd", 0, "inherited fd for streaming audit events; overrides -audit when >0 (set by sandboxd)")
		transparent = flag.Bool("transparent", false, "accept iptables-redirected TCP and dispatch by protocol sniff (Linux only)")
		mark        = flag.Int("mark", 0, "SO_MARK to stamp on upstream sockets so iptables can skip them; required when -transparent is set")
		caCertPath  = flag.String("ca-cert", "", "PEM CA cert; with -ca-key enables TLS interception for inspectable rules")
		caKeyPath   = flag.String("ca-key", "", "PEM CA private key (paired with -ca-cert)")
		dnsAddr     = flag.String("dns-addr", "", "listen address for the DNS sinkhole (UDP+TCP); when set, all workload DNS is redirected here and answered with -dns-sink")
		dnsSink     = flag.String("dns-sink", proxy.DefaultDNSSink, "IPv4 placeholder returned for every DNS query (must be public unicast so client SSRF guards accept it; the agent connects to it and the proxy re-resolves the real name)")
		poolScope   = flag.String("upstream-pool-scope", "vm", "upstream connection-pool scope: \"vm\" keys by source IP (co-tenant sandboxes isolated, default); \"pod\" shares warm connections across all sandboxes in the pod (faster first goto, gives up per-VM connection isolation)")
	)
	flag.Parse()

	// -events-fd is the sandboxd-driven path: a socketpair fd inherited
	// from the parent. Stream audit events there instead of a file —
	// no disk I/O, backpressure is the kernel socket buffer. Falls back
	// to -audit (and then stderr) for standalone runs.
	var auditOut io.Writer = os.Stderr
	switch {
	case *eventsFD > 0:
		f := os.NewFile(uintptr(*eventsFD), "events")
		if f == nil {
			log.Fatalf("events-fd=%d not open", *eventsFD)
		}
		defer f.Close()
		auditOut = f
	case *auditPath != "":
		f, err := os.OpenFile(*auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("audit log: %v", err)
		}
		defer f.Close()
		auditOut = f
	}

	rules, _ := loadRules(*rulesPath)
	caCert, caKey := loadCA(*caCertPath, *caKeyPath)
	p, err := proxy.New(proxy.Config{
		Addr:               *addr,
		Audit:              auditOut,
		OutboundMark:       *mark,
		CACert:             caCert,
		CAKey:              caKey,
		UpstreamPoolShared: *poolScope == "pod",
	})
	if err != nil {
		log.Fatalf("proxy.New: %v", err)
	}
	p.SetRulesBySource(rules)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// SIGHUP → reload the egress allowlist from -rules. sandboxd
	// signals this after writing a fresh rules file in response to a
	// /v1/config PUT. Re-reading from disk on each signal (vs. a
	// passed-in pointer) keeps the on-wire contract simple and lets
	// an operator hand-edit and SIGHUP for debugging.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				newRules, gen, err := readRules(*rulesPath)
				if err != nil {
					log.Printf("sbxproxy: SIGHUP reload failed (keeping current rules): %v", err)
					continue
				}
				p.SetRulesBySource(newRules)
				log.Printf("sbxproxy: reloaded %d rules across %d source(s) from %s", countRules(newRules), len(newRules), *rulesPath)
				// Echo the applied generation back to sandboxd so a coalesced
				// reload can be awaited (the pack-mode create path blocks until its
				// rules are live before marking the sandbox ready). Emitted only
				// after SetRulesBySource returns, so an ack implies the rules are
				// enforced. gen==0 is the boot/single-sandbox array shape, which has
				// no waiters — stay silent there.
				if gen > 0 {
					p.EmitControl(map[string]any{"type": "control", "control": "egress_reload", "generation": gen})
				}
			}
		}
	}()

	// DNS sinkhole: iptables redirects all workload DNS here, and we answer
	// every query with a constant placeholder so DNS can't be a tunnel. The
	// agent then connects to the placeholder and the proxy re-resolves the
	// real host on its own SO_MARK'd resolver. Runs alongside ServeTransparent.
	if *dnsAddr != "" {
		sinkIP := net.ParseIP(*dnsSink)
		if sinkIP == nil || sinkIP.To4() == nil {
			log.Fatalf("sbxproxy: -dns-sink %q is not a valid IPv4 address", *dnsSink)
		}
		if err := p.ServeDNSSink(ctx, *dnsAddr, sinkIP); err != nil {
			log.Fatalf("proxy.ServeDNSSink: %v", err)
		}
	}

	if *transparent {
		log.Printf("sbxproxy listening (transparent) on %s, %d rules, mark=0x%x", *addr, countRules(rules), *mark)
		if err := p.ServeTransparent(ctx, *addr); err != nil {
			log.Fatalf("proxy.ServeTransparent: %v", err)
		}
		return
	}

	if err := p.Listen(); err != nil {
		log.Fatalf("proxy.Listen: %v", err)
	}
	log.Printf("sbxproxy listening on %s, %d rules", p.Addr(), countRules(rules))
	if err := p.Run(ctx); err != nil {
		log.Fatalf("proxy.Run: %v", err)
	}
}

// loadRules reads the egress rules. An empty path yields a deny-everything
// proxy, which is the right default if the orchestrator forgot to wire rules in.
// Errors are fatal — initial load failures shouldn't be papered over.
func loadRules(path string) (map[string][]proxy.EgressRule, uint64) {
	rules, gen, err := readRules(path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return rules, gen
}

// readRules is the non-fatal sibling of loadRules; SIGHUP reloads use it so a
// bad on-disk write doesn't crash the proxy. It accepts three on-disk shapes:
//
//   - a bare array `[rule...]` — the single-sandbox form, mapped to the
//     all-sources "" bucket (generation 0);
//   - a per-source object `{"<srcIP>": [rule...], ...}` — one allowlist per
//     sandbox source IP (the multi-sandbox form, design §8), generation 0; and
//   - a generation envelope `{"generation": N, "sources": {"<srcIP>": [...]}}`
//     — the pack-mode form, where N lets sandboxd await a coalesced reload.
//
// The array vs. object split is detected from the first non-space byte; an
// object carrying a "sources" key is the envelope (no source IP is the literal
// "sources"). The generation rides inside the same file as its rules, so an
// echoed generation always corresponds to the rule set just applied.
func readRules(path string) (map[string][]proxy.EgressRule, uint64, error) {
	if path == "" {
		return nil, 0, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read rules %s: %w", path, err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var rules []proxy.EgressRule
		if err := json.Unmarshal(trimmed, &rules); err != nil {
			return nil, 0, fmt.Errorf("parse rules %s: %w", path, err)
		}
		return map[string][]proxy.EgressRule{"": rules}, 0, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, 0, fmt.Errorf("parse rules %s: %w", path, err)
	}
	if _, ok := probe["sources"]; ok {
		var env struct {
			Generation uint64                        `json:"generation"`
			Sources    map[string][]proxy.EgressRule `json:"sources"`
		}
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, 0, fmt.Errorf("parse rules %s: %w", path, err)
		}
		return env.Sources, env.Generation, nil
	}
	var bySource map[string][]proxy.EgressRule
	if err := json.Unmarshal(trimmed, &bySource); err != nil {
		return nil, 0, fmt.Errorf("parse rules %s: %w", path, err)
	}
	return bySource, 0, nil
}

// countRules totals the rules across all source buckets, for logging.
func countRules(m map[string][]proxy.EgressRule) int {
	n := 0
	for _, rs := range m {
		n += len(rs)
	}
	return n
}

// loadCA reads the PEM CA cert + key. Returns (nil, nil) when both
// paths are empty — TLS interception is then disabled and transparent
// HTTPS is raw-forwarded after SNI host match.
func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey) {
	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		log.Fatalf("ca: -ca-cert and -ca-key must both be set or both omitted")
	}
	cb, err := os.ReadFile(certPath)
	if err != nil {
		log.Fatalf("read ca cert: %v", err)
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatalf("read ca key: %v", err)
	}
	cert, err := proxy.DecodeCertPEM(cb)
	if err != nil {
		log.Fatalf("parse ca cert: %v", err)
	}
	key, err := proxy.DecodeKeyPEM(kb)
	if err != nil {
		log.Fatalf("parse ca key: %v", err)
	}
	return cert, key
}
