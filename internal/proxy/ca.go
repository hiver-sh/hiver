package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// GenerateCA creates a self-signed root CA suitable for signing per-host
// leaf certificates. The CA is per-sandbox and only valid for as long
// as the pod runs — a fresh one is generated on every sandboxd start.
//
// ECDSA P-256 keeps the key small (and TLS handshakes fast); the 7-day
// validity window is short enough that an exfiltrated CA can't outlive
// the workload by much.
func GenerateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: keygen: %w", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "sandbox-pod-ca",
			Organization: []string{"sandbox-platform"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(7 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: sign: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// EncodeCertPEM returns the certificate in PEM form.
func EncodeCertPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// EncodeKeyPEM returns an ECDSA private key in PEM form.
func EncodeKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// DecodeCertPEM parses a single CERTIFICATE block.
func DecodeCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("ca: no PEM block in cert")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// DecodeKeyPEM parses a single EC PRIVATE KEY block.
func DecodeKeyPEM(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("ca: no PEM block in key")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}
