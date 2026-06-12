//go:build linux

package proxy

import (
	"context"
	"fmt"
	"log"
	"net"

	utls "github.com/refraction-networking/utls"
)

// dialUpstreamTLS dials dialAddr over TCP and completes a TLS handshake. WS
// upgrades mirror the agent's ClientHello (with a Chrome fallback) so
// Cloudflare Bot Management sees the agent's JA3 paired with the agent's
// HTTP headers rather than Go's stdlib JA3. Regular HTTPS uses the Chrome
// fingerprint directly — fast path; the mirror buys nothing here because
// the agent's own stdlib JA3 would already trip CF.
//
// dialAddr is normally "<sni-host>:<port>" (the workload's DNS is sinkholed, so
// the proxy resolves the name itself); host is the TLS ServerName.
//
// ALPN is pinned to http/1.1 in both cases so the inspection loop can read
// the inner request after the handshake.
func (p *Proxy) dialUpstreamTLS(dialAddr, host string, clientHello []byte, ws bool) (net.Conn, error) {
	if ws {
		return dialMirroredWithChromeFallback(p.dialer, dialAddr, host, clientHello)
	}
	rawUp, err := p.dialer.DialContext(context.Background(), "tcp", dialAddr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	uc, err := chromeTLSHandshake(rawUp, host)
	if err != nil {
		_ = rawUp.Close()
		return nil, fmt.Errorf("chrome handshake: %w", err)
	}
	cs := uc.ConnectionState()
	log.Printf("upstream tls: chrome handshake ok host=%s version=0x%04x cipher=0x%04x", host, cs.Version, cs.CipherSuite)
	return uc, nil
}

// mirroredTLSHandshake connects to the upstream using the same TLS fingerprint
// the agent sent to us. allowBluntMimicry=true keeps extensions utls doesn't
// natively understand (e.g. encrypt_then_mac, ext 22) as raw GenericExtension
// bytes — required to mirror native-tls/OpenSSL faithfully.
func mirroredTLSHandshake(conn net.Conn, serverName string, clientHello []byte) (*utls.UConn, error) {
	var spec utls.ClientHelloSpec
	if err := spec.FromRaw(clientHello, true); err != nil {
		return nil, fmt.Errorf("utls mirror spec: %w", err)
	}
	pinALPN(&spec, "http/1.1")
	return applyUTLSSpec(conn, serverName, &spec)
}

// chromeTLSHandshake connects to the upstream using a Chrome ClientHello
// fingerprint so Cloudflare-protected hosts (e.g. chatgpt.com) don't block
// the proxy via JA3 detection.
func chromeTLSHandshake(conn net.Conn, serverName string) (*utls.UConn, error) {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		return nil, fmt.Errorf("utls spec: %w", err)
	}
	pinALPN(&spec, "http/1.1")
	return applyUTLSSpec(conn, serverName, &spec)
}

// applyUTLSSpec wraps conn in a utls client, applies the spec, and does the
// handshake. Shared tail of the two ClientHello strategies above.
func applyUTLSSpec(conn net.Conn, serverName string, spec *utls.ClientHelloSpec) (*utls.UConn, error) {
	uconn := utls.UClient(conn, &utls.Config{ServerName: serverName}, utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		return nil, fmt.Errorf("utls apply: %w", err)
	}
	if err := uconn.Handshake(); err != nil {
		return nil, err
	}
	return uconn, nil
}

// pinALPN overwrites the ALPN extension in spec with proto, appending the
// extension if absent. Forces HTTP/1.1 negotiation upstream so the
// inspection loop can read the inner request after the handshake.
func pinALPN(spec *utls.ClientHelloSpec, proto string) {
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{proto}
			spec.Extensions[i] = alpn
			return
		}
	}
	spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: []string{proto}})
}

// dialMirroredWithChromeFallback dials origDst and tries to complete a TLS
// handshake that mirrors the client's ClientHello. On any failure — parse,
// apply, or handshake — it redials and retries with the Chrome fingerprint.
// A redial is required because a failed handshake leaves the TCP connection
// in an indeterminate state (server bytes may have been written and
// consumed); a fresh socket is the only way to send a clean second hello.
//
// The two-stage attempt is what gets a Rust native-tls client all the way
// through to Cloudflare-protected hosts. Mirroring is preferred because it
// pairs the agent's JA3 with the agent's HTTP headers; Chrome is a much
// better fallback than stdlib TLS, which is widely fingerprinted as bot.
func dialMirroredWithChromeFallback(dialer *net.Dialer, dialAddr, host string, clientHello []byte) (net.Conn, error) {
	rawUp, err := dialer.DialContext(context.Background(), "tcp", dialAddr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	uc, mirrorErr := mirroredTLSHandshake(rawUp, host, clientHello)
	if mirrorErr == nil {
		cs := uc.ConnectionState()
		log.Printf("upstream tls: mirrored handshake ok host=%s version=0x%04x cipher=0x%04x", host, cs.Version, cs.CipherSuite)
		return uc, nil
	}
	log.Printf("upstream tls: mirrored handshake failed host=%s err=%v — falling back to chrome", host, mirrorErr)
	_ = rawUp.Close()

	rawUp2, err := dialer.DialContext(context.Background(), "tcp", dialAddr)
	if err != nil {
		return nil, fmt.Errorf("redial for chrome fallback: %w", err)
	}
	uc2, chromeErr := chromeTLSHandshake(rawUp2, host)
	if chromeErr != nil {
		_ = rawUp2.Close()
		return nil, fmt.Errorf("mirror+chrome both failed: mirror=%v chrome=%v", mirrorErr, chromeErr)
	}
	cs := uc2.ConnectionState()
	log.Printf("upstream tls: chrome fallback handshake ok host=%s version=0x%04x cipher=0x%04x", host, cs.Version, cs.CipherSuite)
	return uc2, nil
}
