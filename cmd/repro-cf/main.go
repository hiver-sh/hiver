// Command repro-cf reproduces the exact WebSocket flow that Codex CLI uses:
// wss://api.openai.com/v1/responses with gpt-5.5 and ChatGPT auth.
// Credentials are read from ~/.codex/auth.json and ~/.codex/installation_id.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	utls "github.com/refraction-networking/utls"
)

const (
	host   = "chatgpt.com"
	wsPath = "/backend-api/codex/responses" // chatgpt auth mode (from Codex source)
	model  = "gpt-5.5"
)

func main() {
	token, installID, accountID := loadCodexAuth()
	fmt.Printf("auth: Bearer %s...%s\n", token[:12], token[len(token)-8:])
	fmt.Printf("installation_id: %s\n\n", installID)

	fmt.Println("dialing", host, "with uTLS Chrome fingerprint...")
	rawConn, err := net.Dial("tcp", host+":443")
	if err != nil {
		log.Fatalf("TCP dial: %v", err)
	}

	// Build a Chrome spec but override ALPN to http/1.1 so the proxy's
	// HTTP/1.1 inspection loop can read the inner requests.
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		log.Fatalf("utls spec: %v", err)
	}
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
			spec.Extensions[i] = alpn
			break
		}
	}

	roots, _ := x509.SystemCertPool()
	if roots == nil {
		roots = x509.NewCertPool()
	}
	// Fallback: load the Debian/Ubuntu bundle directly if the system pool is empty.
	for _, p := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt"} {
		if pem, err := os.ReadFile(p); err == nil {
			roots.AppendCertsFromPEM(pem)
			break
		}
	}
	uconn := utls.UClient(rawConn, &utls.Config{ServerName: host, RootCAs: roots}, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		log.Fatalf("utls apply preset: %v", err)
	}
	if err := uconn.Handshake(); err != nil {
		log.Fatalf("utls handshake: %v", err)
	}
	defer uconn.Close()

	cs := uconn.ConnectionState()
	fmt.Printf("TLS version=0x%04x cipher=0x%04x\n\n", cs.Version, cs.CipherSuite)

	conn := uconn

	// --- WebSocket upgrade with exact Codex TUI headers (lowercase, as Rust/hyper sends them) ---
	wsKey := randomWSKey()
	const sessionID = "019e5cad-3c91-7880-90c8-6bad6d8701e2"
	bw := bufio.NewWriter(conn)
	fmt.Fprintf(bw, "GET %s HTTP/1.1\r\n", wsPath)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "authorization: Bearer %s\r\n", token)
	fmt.Fprintf(bw, "chatgpt-account-id: %s\r\n", accountID)
	fmt.Fprintf(bw, "connection: Upgrade\r\n")
	fmt.Fprintf(bw, "openai-beta: responses_websockets=2026-02-06\r\n")
	fmt.Fprintf(bw, "originator: codex-tui\r\n")
	fmt.Fprintf(bw, "sec-websocket-extensions: permessage-deflate; client_max_window_bits\r\n")
	fmt.Fprintf(bw, "sec-websocket-key: %s\r\n", wsKey)
	fmt.Fprintf(bw, "sec-websocket-version: 13\r\n")
	fmt.Fprintf(bw, "session-id: %s\r\n", sessionID)
	fmt.Fprintf(bw, "thread-id: %s\r\n", sessionID)
	fmt.Fprintf(bw, "upgrade: websocket\r\n")
	fmt.Fprintf(bw, "user-agent: codex-tui/0.133.0 (Debian 12.0.0; aarch64) tmux/3.3a (codex-tui; 0.133.0)\r\n")
	fmt.Fprintf(bw, "version: 0.133.0\r\n")
	fmt.Fprintf(bw, "x-client-request-id: %s\r\n", sessionID)
	fmt.Fprintf(bw, "x-codex-beta-features: terminal_resize_reflow\r\n")
	fmt.Fprintf(bw, `x-codex-turn-metadata: {"session_id":%q,"thread_id":%q,"thread_source":"user","turn_id":%q,"sandbox":"seccomp","turn_started_at_unix_ms":1779671320227}`+"\r\n",
		sessionID, sessionID, sessionID)
	fmt.Fprintf(bw, "x-codex-window-id: %s:0\r\n", sessionID)
	fmt.Fprintf(bw, "\r\n")
	if err := bw.Flush(); err != nil {
		log.Fatalf("write upgrade: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		log.Fatalf("read upgrade response: %v", err)
	}
	resp.Body.Close()
	fmt.Printf("upgrade: %s\n", resp.Status)
	for k, vs := range resp.Header {
		fmt.Printf("  %s: %s\n", k, vs)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		fmt.Printf("body: %s\n", body)
		return
	}
	resp.Body.Close()
	fmt.Println()

	// --- Send response.create with a simple message ---
	sendJSON(conn, map[string]any{
		"type":         "response.create",
		"model":        model,
		"instructions": "You are a helpful assistant.",
		"store":        false,
		"input": []map[string]any{
			{"role": "user", "content": "Reply with exactly three words."},
		},
	})

	// --- Stream frames until response.completed ---
	fmt.Println("streaming response...")
	for {
		opcode, rsv1, data, err := readFrameFull(br)
		if err != nil {
			if err == io.EOF {
				fmt.Println("\nserver closed connection")
				return
			}
			log.Fatalf("read frame: %v", err)
		}
		// Close frame (opcode 0x8): payload = 2-byte status + reason.
		if opcode == 0x8 {
			if len(data) >= 2 {
				code := int(data[0])<<8 | int(data[1])
				fmt.Printf("\nclose frame: code=%d reason=%q\n", code, data[2:])
			} else {
				fmt.Println("\nclose frame (no payload)")
			}
			return
		}
		// Compressed frame (RSV1=1): can't parse as JSON without deflate.
		if rsv1 {
			fmt.Printf("  [compressed frame opcode=%d len=%d]\n", opcode, len(data))
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal(data, &evt); err != nil {
			fmt.Printf("(non-JSON opcode=%d): %q\n", opcode, data)
			continue
		}
		t, _ := evt["type"].(string)
		switch t {
		case "response.output_text.delta":
			fmt.Print(evt["delta"])
		case "response.output_text.done":
			fmt.Printf("\n\ntext: %q\n", evt["text"])
		case "response.completed":
			out, _ := json.MarshalIndent(evt, "", "  ")
			fmt.Printf("\nresponse.completed:\n%s\n", out)
			return
		case "error":
			out, _ := json.MarshalIndent(evt, "", "  ")
			fmt.Printf("\nerror:\n%s\n", out)
			return
		default:
			fmt.Printf("  [%s]\n", t)
		}
	}
}

func loadCodexAuth() (token, installID, accountID string) {
	home, _ := os.UserHomeDir()

	raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		log.Fatalf("read auth.json: %v", err)
	}
	var auth struct {
		AccountID string `json:"account_id"`
		Tokens    struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		log.Fatalf("parse auth.json: %v", err)
	}
	token = auth.Tokens.AccessToken
	if token == "" {
		log.Fatal("no access_token in auth.json")
	}

	id, err := os.ReadFile(filepath.Join(home, ".codex", "installation_id"))
	if err != nil {
		log.Fatalf("read installation_id: %v", err)
	}
	return token, string(id), auth.AccountID
}

func sendJSON(w io.Writer, v any) {
	payload, _ := json.Marshal(v)
	fmt.Printf("→ %s\n\n", payload)
	if err := writeFrame(w, payload); err != nil {
		log.Fatalf("write frame: %v", err)
	}
}

func printCFHeaders(h http.Header) {
	for _, k := range []string{"Cf-Ray", "Cf-Mitigated", "Server", "Content-Type"} {
		if v := h.Get(k); v != "" {
			fmt.Printf("  %-22s %s\n", k+":", v)
		}
	}
}

func randomWSKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// writeFrame writes a masked text frame (RFC 6455 §5).
func writeFrame(w io.Writer, payload []byte) error {
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	hdr := []byte{0x81} // FIN=1, opcode=text
	l := len(payload)
	switch {
	case l <= 125:
		hdr = append(hdr, byte(l|0x80))
	case l <= 65535:
		hdr = append(hdr, 0xFE)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(l))
	default:
		hdr = append(hdr, 0xFF)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(l))
	}
	hdr = append(hdr, mask...)
	masked := make([]byte, l)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_, err := w.Write(append(hdr, masked...))
	return err
}

// readFrameFull reads one server frame and returns opcode, rsv1 flag, unmasked payload, and error.
func readFrameFull(r io.Reader) (opcode byte, rsv1 bool, payload []byte, err error) {
	hdr := make([]byte, 2)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return 0, false, nil, err
	}
	opcode = hdr[0] & 0x0F
	rsv1 = (hdr[0] & 0x40) != 0
	masked := (hdr[1] & 0x80) != 0
	l := int(hdr[1] & 0x7f)
	switch l {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return opcode, rsv1, nil, err
		}
		l = int(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return opcode, rsv1, nil, err
		}
		l = int(binary.BigEndian.Uint64(ext))
	}
	var maskKey []byte
	if masked {
		maskKey = make([]byte, 4)
		if _, err = io.ReadFull(r, maskKey); err != nil {
			return opcode, rsv1, nil, err
		}
	}
	payload = make([]byte, l)
	if _, err = io.ReadFull(r, payload); err != nil {
		return opcode, rsv1, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, rsv1, payload, nil
}
