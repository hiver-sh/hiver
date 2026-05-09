package sandboxd

import (
	"crypto/x509"
	"log"
	"os"
	"path/filepath"

	"github.com/sandbox-platform/agent-sandbox/internal/proxy"
)

// Generate a per-pod CA. sbxproxy uses it to mint leaf certs for
// TLS interception (any rule with paths/methods/headers triggers
// termination). The cert PEM is also spliced into the agent rootfs
// trust store below so the agent's TLS handshake validates the
// proxy-presented leaf.
func GenerateCaCert(caCertPath string, caKeyPath string) *x509.Certificate {
	caCert, caKey, err := proxy.GenerateCA()
	if err != nil {
		log.Fatalf("generate CA: %v", err)
	}
	if err := os.WriteFile(caCertPath, proxy.EncodeCertPEM(caCert), 0o644); err != nil {
		log.Fatalf("write ca cert: %v", err)
	}
	caKeyPEM, err := proxy.EncodeKeyPEM(caKey)
	if err != nil {
		log.Fatalf("encode ca key: %v", err)
	}
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0o600); err != nil {
		log.Fatalf("write ca key: %v", err)
	}
	return caCert
}

// Splice the per-pod CA into the agent rootfs trust store so the
// agent's TLS handshakes validate the leaf certs sbxproxy mints
// during interception. We append (not replace) so the agent keeps
// trust for unintercepted TLS to public hosts. Best effort: an
// image without ca-certificates installed gets a warning — its TLS
// requests will fail anyway, with or without our addition.
func AddCA(rootfsDir string, caCert *x509.Certificate) {
	caBundle := filepath.Join(rootfsDir, "etc/ssl/certs/ca-certificates.crt")
	if existing, err := os.ReadFile(caBundle); err == nil {
		merged := append(existing, '\n')
		merged = append(merged, proxy.EncodeCertPEM(caCert)...)
		if err := os.WriteFile(caBundle, merged, 0o644); err != nil {
			log.Fatalf("install sandbox CA into agent rootfs: %v", err)
		}
		log.Printf("sandboxd: installed sandbox CA into %s", caBundle)
	} else {
		log.Printf("sandboxd: agent rootfs has no %s — TLS interception will fail; install ca-certificates in the agent image", caBundle)
	}
}
