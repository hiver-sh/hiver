// Command sbxproxy is the per-sandbox MITM egress proxy.
//
// Prototype scope (T56, T58, T59, T68): HTTP forward proxy + HTTPS via
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
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sandbox-platform/agent-sandbox/internal/proxy"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:3128", "listen address")
		rulesPath   = flag.String("rules", "", "path to JSON file containing []proxy.EgressRule")
		auditPath   = flag.String("audit", "", "audit log file (default: stderr)")
		transparent = flag.Bool("transparent", false, "accept iptables-redirected TCP and dispatch by protocol sniff (Linux only)")
		mark        = flag.Int("mark", 0, "SO_MARK to stamp on upstream sockets so iptables can skip them; required when -transparent is set")
		caCertPath  = flag.String("ca-cert", "", "PEM CA cert; with -ca-key enables TLS interception for inspectable rules")
		caKeyPath   = flag.String("ca-key", "", "PEM CA private key (paired with -ca-cert)")
	)
	flag.Parse()

	var auditOut io.Writer = os.Stderr
	if *auditPath != "" {
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
		Allow:        rules,
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
// orchestrator forgot to wire rules in.
func loadRules(path string) []proxy.EgressRule {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read rules: %v", err)
	}
	var rules []proxy.EgressRule
	if err := json.Unmarshal(data, &rules); err != nil {
		log.Fatalf("parse rules: %v", err)
	}
	return rules
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
