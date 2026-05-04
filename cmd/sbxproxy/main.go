// Command sbxproxy is the per-sandbox MITM egress proxy.
//
// Prototype scope (T56, T58, T59, T68): HTTP forward proxy + HTTPS via
// CONNECT tunneling, host-pattern allowlist, JSON-line audit log, default
// inbound auth-header stripping.
//
// Out of scope for v0: TLS body interception (T57), credential broker
// (T60), body inspectors (T61–T65), per-(sandbox,dest) rate limiting (T67).
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sandbox-platform/agent-sandbox/internal/proxy"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:3128", "listen address")
		allowlist   = flag.String("allow", "", "comma-separated host allowlist (supports *.example.com)")
		auditPath   = flag.String("audit", "", "audit log file (default: stderr)")
		transparent = flag.Bool("transparent", false, "accept iptables-redirected TCP and dispatch by protocol sniff (Linux only)")
		mark        = flag.Int("mark", 0, "SO_MARK to stamp on upstream sockets so iptables can skip them; required when -transparent is set")
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

	allow := splitAllow(*allowlist)
	p, err := proxy.New(proxy.Config{
		Addr:         *addr,
		Allow:        allow,
		Audit:        auditOut,
		OutboundMark: *mark,
	})
	if err != nil {
		log.Fatalf("proxy.New: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *transparent {
		log.Printf("sbxproxy listening (transparent) on %s, allow=%v, mark=0x%x", *addr, allow, *mark)
		if err := p.ServeTransparent(ctx, *addr); err != nil {
			log.Fatalf("proxy.ServeTransparent: %v", err)
		}
		return
	}

	if err := p.Listen(); err != nil {
		log.Fatalf("proxy.Listen: %v", err)
	}
	log.Printf("sbxproxy listening on %s, allow=%v", p.Addr(), allow)
	if err := p.Run(ctx); err != nil {
		log.Fatalf("proxy.Run: %v", err)
	}
}

func splitAllow(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
