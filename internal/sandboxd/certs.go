package sandboxd

import (
	"crypto/x509"
	"log"
	"os"

	"github.com/hiver-sh/hiver/internal/proxy"
)

// Generate a per-pod CA. sbxproxy uses it to mint leaf certs for
// TLS interception (any rule with paths/methods/headers triggers
// termination). The cert PEM is later spliced into the workload trust
// store by the isolation backend's InstallCA so the agent's TLS
// handshake validates the proxy-presented leaf.
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
