package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"sync"
	"time"
)

// CertMinter generates per-host leaf certificates signed by a sandbox CA
// and caches them by hostname. Used by the transparent TLS-intercept
// path: `tls.Server` with `GetCertificate` calls into Mint(SNI).
//
// The cache is unbounded for the prototype — egress allowlists are tiny
// and the pod is short-lived; LRU eviction is a follow-up if a workload
// hits dozens of unique HTTPS hosts.
type CertMinter struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

func NewCertMinter(ca *x509.Certificate, caKey *ecdsa.PrivateKey) *CertMinter {
	return &CertMinter{ca: ca, caKey: caKey, cache: map[string]*tls.Certificate{}}
}

// Mint returns a leaf certificate for host, generating one if absent.
// The returned cert chains to the sandbox CA and is what TLS termination
// presents to the agent during the inner handshake.
func (m *CertMinter) Mint(host string) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[host]; ok && c.Leaf != nil && c.Leaf.NotAfter.After(time.Now()) {
		return c, nil
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, &leafKey.PublicKey, m.caKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{
		Certificate: [][]byte{der, m.ca.Raw},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
	m.cache[host] = cert
	return cert, nil
}
