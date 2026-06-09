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
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
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

	rules := loadRules(*rulesPath)
	caCert, caKey := loadCA(*caCertPath, *caKeyPath)
	p, err := proxy.New(proxy.Config{
		Addr:         *addr,
		Rules:        rules,
		Audit:        auditOut,
		OutboundMark: *mark,
		CACert:       caCert,
		CAKey:        caKey,
	})
	if err != nil {
		log.Fatalf("proxy.New: %v", err)
	}

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
				newRules, err := readRules(*rulesPath)
				if err != nil {
					log.Printf("sbxproxy: SIGHUP reload failed (keeping current rules): %v", err)
					continue
				}
				p.SetRules(newRules)
				log.Printf("sbxproxy: reloaded %d rules from %s", len(newRules), *rulesPath)
			}
		}
	}()

	if *transparent {
		log.Printf("sbxproxy listening (transparent) on %s, %d rules, mark=0x%x", *addr, len(rules), *mark)
		if err := p.ServeTransparent(ctx, *addr); err != nil {
			log.Fatalf("proxy.ServeTransparent: %v", err)
		}
		return
	}

	if err := p.Listen(); err != nil {
		log.Fatalf("proxy.Listen: %v", err)
	}
	log.Printf("sbxproxy listening on %s, %d rules", p.Addr(), len(rules))
	if err := p.Run(ctx); err != nil {
		log.Fatalf("proxy.Run: %v", err)
	}
}

// loadRules reads a JSON-encoded []proxy.EgressRule. An empty path
// yields a deny-everything proxy, which is the right default if the
// orchestrator forgot to wire rules in. Errors are fatal — initial
// load failures shouldn't be papered over.
func loadRules(path string) []proxy.EgressRule {
	rules, err := readRules(path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return rules
}

// readRules is the non-fatal sibling of loadRules; SIGHUP reloads use
// it so a bad on-disk write doesn't crash the proxy.
func readRules(path string) ([]proxy.EgressRule, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rules %s: %w", path, err)
	}
	var rules []proxy.EgressRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("parse rules %s: %w", path, err)
	}
	return rules, nil
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
