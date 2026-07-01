// Command chromehost is the resident Chrome host, driven over CDP. It is the
// playwright image entrypoint, so it launches Chromium and stays resident — a
// sandbox that captures a VM snapshot (via the snapshot API) resumes with Chromium
// already launched and listening.
//
// It is the stdlib-only Go replacement for the former Node.js chrome-cdp.cjs: a
// static binary with no dependencies, so the resident process captured in a VM
// snapshot is lean instead of a Node + V8 + playwright-core heap. It does
// exactly what the .cjs did:
//
//   - spawn the Playwright-managed Chromium binary directly with its DevTools
//     (CDP) endpoint open and supervise it (no Playwright at runtime);
//
//   - front Chrome's loopback CDP endpoint with a 0.0.0.0 relay that exposes a
//     STABLE /cdp alias, so clients attach with a single durable URL:
//
//     const browser = await chromium.connectOverCDP("ws://<host>:9223/cdp")
//
//     reached through the sandbox ingress proxy (/v1/<key>/proxy/9223/cdp);
//
// Why a relay (not a plain forwarder): Chrome's real browser endpoint is
// /devtools/browser/<uuid>, with a fresh <uuid> each launch, so a normal client
// would have to GET /json/version and re-derive that path every time. The relay
// exposes the fixed /cdp alias and rewrites it onto the current GUID (resolved
// once at startup). It also binds 0.0.0.0: Chrome keeps its DevTools socket on
// loopback regardless of --remote-debugging-address (a known --headless quirk),
// so a cross-netns dial to the guest IP gets "connection refused"; the relay is
// what the ingress proxy actually reaches.
//
// Readiness: Chrome prints "DevTools listening on ws://..." to stderr once the
// endpoint is up; only then do we resolve the stable path and bring up the relay.
// The process then stays resident as the workload.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// stablePath is the fixed alias clients connect to; the relay rewrites it onto
	// Chrome's per-launch /devtools/browser/<uuid> path.
	stablePath = "/cdp"
	// chromeBin is the stable symlink to the Chromium binary, created at image
	// build time so nothing at runtime depends on Playwright.
	chromeBin = "/opt/hiver/chrome"

	// chromeMajorVersion / chromeFullVersion track the actual bundled Chromium
	// build. The UA string (defaultUserAgent) and the Sec-CH-UA client-hint
	// metadata (userAgentMetadata) are both derived from these, so the UA header
	// and the client hints never drift apart — keep them in sync with the image's
	// Chromium when it's upgraded.
	chromeMajorVersion = "149"
	chromeFullVersion  = "149.0.7827.0"

	// defaultUserAgent overrides Chrome's built-in default, which under
	// --headless=new advertises "HeadlessChrome/..." — an obvious bot tell. This
	// is the same underlying build presented as an ordinary desktop Chrome: real
	// platform (X11; Linux x86_64) and real major version, just without the
	// "Headless" token and with the reduced ".0.0.0" version format real Chrome
	// emits. Keeping the real platform means the UA stays consistent with the
	// Sec-CH-UA-Platform / Sec-CH-UA-Mobile client hints (derived from the OS, not
	// this string). Override with HIVER_BROWSER_USER_AGENT.
	//
	// The Sec-CH-UA *brand* list is separate: it's Chrome's built-in brand list
	// (which contains "HeadlessChrome" under --headless=new) and cannot be changed
	// by any launch flag. It's fixed at runtime via CDP — see applyUserAgentOverride.
	defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromeMajorVersion + ".0.0.0 Safari/537.36"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func chromeArgs(chromePort int, userDataDir, userAgent string) []string {
	return []string{
		// DevTools/CDP endpoint. Chrome binds this on loopback only (see header);
		// the 0.0.0.0 relay is what the ingress proxy reaches. --remote-allow-origins
		// is required by Chrome 111+ or it 403s the connectOverCDP WebSocket upgrade.
		fmt.Sprintf("--remote-debugging-port=%d", chromePort),
		"--remote-allow-origins=*",
		"--headless=new",
		"--user-data-dir=" + userDataDir,
		// Present as an ordinary desktop Chrome instead of the default
		// "HeadlessChrome/..." UA. See defaultUserAgent.
		"--user-agent=" + userAgent,

		// cost-minimizing flags.
		"--no-sandbox",
		"--single-process",
		"--no-zygote",
		"--disable-features=site-per-process,IsolateOrigins,Translate,BackForwardCache,MediaRouter,OptimizationHints,OptimizationGuideModelDownloading,AcceptCHFrame,AudioServiceOutOfProcess,AutofillServerCommunication,InterestFeedContentSuggestions,CalculateNativeWinOcclusion",
		"--disable-site-isolation-trials",
		"--process-per-site",
		"--renderer-process-limit=1",
		"--in-process-gpu",
		"--disable-gpu",
		"--disable-software-rasterizer",
		"--disable-accelerated-2d-canvas",
		// Enable the HTTP/media cache (it was pinned to ~off with size=1). A resident
		// browser warmed at snapshot time then serves repeat navigations to the same
		// origin (e.g. the benchmark's example.com, captured in the snapshot) from
		// cache instead of a cold refetch — the dominant cost in goto. Bounded at
		// 128 MiB so it can't grow unbounded on the guest overlay.
		"--disk-cache-size=134217728",
		"--js-flags=--max-old-space-size=512",
		"--window-size=800,600",
		"--disable-background-networking",
		"--enable-automation",
		"--disable-component-extensions-with-background-pages",
		"--password-store=basic",
		"--use-mock-keychain",
		"--disable-domain-reliability",
		"--disable-field-trial-config",
		"--no-service-autorun",
		"--disable-component-update",
		"--disable-sync",
		"--disable-default-apps",
		"--disable-extensions",
		"--metrics-recording-only",
		"--disable-breakpad",
		"--no-first-run",
		"--no-default-browser-check",
		"--mute-audio",
		"--disable-hang-monitor",
		"--disable-client-side-phishing-detection",
		"--disable-ipc-flooding-protection",
		"--disable-dev-shm-usage",
	}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("chromehost: ")

	port := envInt("HIVER_BROWSER_PORT", 9223)
	chromePort := envInt("HIVER_CHROME_CDP_PORT", 9222)

	// Fresh, empty throwaway profile per launch — a clean dir has no GCM/sign-in
	// state for headless Chrome to reload, so it stays quiet.
	userDataDir, err := os.MkdirTemp("", "hiver-chrome-")
	if err != nil {
		log.Fatalf("create user-data-dir: %v", err)
	}

	userAgent := env("HIVER_BROWSER_USER_AGENT", defaultUserAgent)
	cmd := exec.Command(env("HIVER_CHROME_BIN", chromeBin), chromeArgs(chromePort, userDataDir, userAgent)...)
	cmd.Stdout = os.Stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to launch chromium: %v", err)
	}

	// Forward termination so a stopped container/VM cleanly closes Chrome.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		if cmd.Process != nil {
			_ = cmd.Process.Signal(s)
		}
	}()

	// Chrome prints "DevTools listening on ws://..." once the CDP endpoint accepts
	// connections. Only then resolve the stable path and bring up the relay.
	var once sync.Once
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(os.Stderr, line)
		if strings.Contains(line, "DevTools listening on ws://") {
			once.Do(func() { go onReady(port, chromePort, userAgent) })
		}
	}

	// stderr closed → Chrome exited. Reap it and exit with its code.
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		log.Fatalf("chrome: %v", err)
	}
	os.Exit(0)
}

// onReady wires up the relay once Chrome's CDP endpoint is up.
func onReady(port, chromePort int, userAgent string) {
	browserWsPath, err := resolveBrowserWsPath(chromePort)
	if err != nil {
		log.Printf("resolve browser ws failed: %v", err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		log.Fatalf("relay listen: %v", err)
	}
	go serveRelay(ln, chromePort, browserWsPath)

	// Fix the Sec-CH-UA client hints, which the --user-agent flag can't touch.
	go applyUserAgentOverride(chromePort, browserWsPath, userAgent)

	disp := browserWsPath
	if disp == "" {
		disp = "/devtools/browser/<uuid>"
	}
	log.Printf("relay 0.0.0.0:%d%s -> 127.0.0.1:%d%s; browser ready", port, stablePath, chromePort, disp)
}

// resolveBrowserWsPath resolves Chrome's current browser WebSocket path
// (/devtools/browser/<uuid>) from /json/version.
func resolveBrowserWsPath(chromePort int) (string, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", chromePort))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	u, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil {
		return "", err
	}
	return u.Path, nil
}

// serveRelay accepts connections and bridges each to Chrome's loopback CDP
// endpoint, rewriting a /cdp request line onto the resolved browser GUID path.
func serveRelay(ln net.Listener, chromePort int, browserWsPath string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("relay accept: %v", err)
			return
		}
		go handleRelay(conn, chromePort, browserWsPath)
	}
}

// handleRelay rewrites the HTTP request line (only) — a connection to stablePath
// has its path swapped for Chrome's real browser GUID path; everything else
// (/json/version, /json/list, an already-resolved /devtools/... path) passes
// through untouched. After the request line it's a raw byte pipe, so the CDP
// WebSocket frames flow through unmodified.
func handleRelay(client net.Conn, chromePort int, browserWsPath string) {
	defer client.Close()

	// The request line should arrive promptly; bound the wait so a connection that
	// never sends one can't leak a goroutine.
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(client)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	_ = client.SetReadDeadline(time.Time{}) // clear: the WS stream is long-lived

	head := line
	if parts := strings.Fields(strings.TrimRight(line, "\r\n")); len(parts) == 3 &&
		browserWsPath != "" && (parts[1] == stablePath || parts[1] == stablePath+"/") {
		head = parts[0] + " " + browserWsPath + " " + parts[2] + "\r\n"
	}

	upstream, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", chromePort))
	if err != nil {
		return
	}
	defer upstream.Close()

	if _, err := upstream.Write([]byte(head)); err != nil {
		return
	}
	// br holds the rest of the client stream (headers + frames) after the request
	// line; copy from br (buffer then conn), not the raw conn, so nothing is lost.
	go func() {
		_, _ = io.Copy(upstream, br)
		if c, ok := upstream.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, upstream)
}

// applyUserAgentOverride fixes the one thing the --user-agent launch flag can't:
// the Sec-CH-UA client hints (and navigator.userAgentData), whose brand list is
// baked into the Chrome build and reports "HeadlessChrome" under --headless=new.
// The only lever is CDP Emulation.setUserAgentOverride with a userAgentMetadata
// object, applied to a page target.
//
// This is a one-shot: it connects to Chrome's browser endpoint, attaches to the
// existing page target(s), sets the override, and then keeps the CDP connection
// open for the life of the process — the override is scoped to the attached
// session, so it would be reverted the moment we detached. The resident-browser
// flow (clients that connectOverCDP and reuse contexts()[0]) drives that same
// pre-existing target, so it inherits the override. Freshly created
// newContext() targets are not covered (that would need Target.setAutoAttach,
// with a per-target round-trip we deliberately avoid here).
//
// Failures are logged, never fatal: a broken override must not take Chrome down.
func applyUserAgentOverride(chromePort int, browserWsPath, userAgent string) {
	if browserWsPath == "" {
		log.Printf("ua-override: no browser ws path; skipping client-hint override")
		return
	}
	addr := fmt.Sprintf("127.0.0.1:%d", chromePort)

	ws, err := wsConnect(addr, browserWsPath)
	if err != nil {
		log.Printf("ua-override: connect: %v", err)
		return
	}

	params := map[string]any{
		"userAgent":         userAgent,
		"userAgentMetadata": userAgentMetadata(),
	}

	// Chrome opens an initial page target, but it may not be present the instant
	// the CDP endpoint accepts connections — retry briefly.
	var applied int
	for range 20 {
		targets, err := ws.pageTargets()
		if err != nil {
			log.Printf("ua-override: getTargets: %v", err)
			ws.close()
			return
		}
		for _, tid := range targets {
			sid, err := ws.attach(tid)
			if err != nil {
				log.Printf("ua-override: attach %s: %v", tid, err)
				continue
			}
			if _, err := ws.call("Emulation.setUserAgentOverride", params, sid); err != nil {
				log.Printf("ua-override: setUserAgentOverride on %s: %v", tid, err)
				continue
			}
			applied++
		}
		if applied > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if applied == 0 {
		log.Printf("ua-override: no page target to override; Sec-CH-UA still reports HeadlessChrome")
		ws.close()
		return
	}
	log.Printf("ua-override: Sec-CH-UA client hints overridden on %d page target(s)", applied)

	// Hold the connection (and thus the session-scoped override) open. Draining
	// incoming frames replies to pings and detects Chrome going away.
	for {
		if _, err := ws.readMessage(); err != nil {
			return
		}
	}
}

// userAgentMetadata is the Sec-CH-UA / navigator.userAgentData payload: the real
// Chrome build's brands with the "HeadlessChrome" entry replaced by "Google
// Chrome", the standard GREASE brand, and Linux platform hints consistent with
// defaultUserAgent (so header UA and client hints agree). Versions come from the
// chrome*Version constants so this can't drift from the UA string.
func userAgentMetadata() map[string]any {
	type brand struct {
		Brand   string `json:"brand"`
		Version string `json:"version"`
	}
	return map[string]any{
		"brands": []brand{
			{"Google Chrome", chromeMajorVersion},
			{"Chromium", chromeMajorVersion},
			{"Not)A;Brand", "24"},
		},
		"fullVersionList": []brand{
			{"Google Chrome", chromeFullVersion},
			{"Chromium", chromeFullVersion},
			{"Not)A;Brand", "24.0.0.0"},
		},
		"fullVersion":     chromeFullVersion,
		"platform":        "Linux",
		"platformVersion": "6.6.0",
		"architecture":    "x86",
		"bitness":         "64",
		"model":           "",
		"mobile":          false,
		"wow64":           false,
	}
}

// --- minimal CDP-over-WebSocket client (stdlib only) ---
//
// chromehost is intentionally dependency-free (see file header), so rather than
// pull in a WebSocket library for a handful of CDP commands, this implements just
// enough of RFC 6455: a client handshake, masked client frames, and frame reads
// (handling fragmentation + ping). Commands are issued synchronously (one setup
// goroutine), so there is only ever a single writer.

type wsConn struct {
	conn   net.Conn
	br     *bufio.Reader
	mu     sync.Mutex // serializes frame writes
	nextID int
}

// wsConnect performs the RFC 6455 opening handshake against Chrome's CDP endpoint.
func wsConnect(addr, path string) (*wsConn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		conn.Close()
		return nil, err
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, " 101 ") {
		conn.Close()
		return nil, fmt.Errorf("ws upgrade failed: %s", strings.TrimSpace(status))
	}
	// Drain the rest of the response headers; frames follow the blank line. The
	// same br is reused for frame reads so nothing buffered past the headers is lost.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &wsConn{conn: conn, br: br}, nil
}

func (w *wsConn) close() { _ = w.conn.Close() }

// writeFrame writes a single final client frame (always masked, per RFC 6455).
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	hdr := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 65536:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, ext[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i := range n {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(hdr); err != nil {
		return err
	}
	_, err := w.conn.Write(masked)
	return err
}

// readMessage reads one complete application message, reassembling fragments and
// transparently answering pings. Returns the message payload (text/binary).
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.br, h[:]); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		opcode := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		n := int(h[1] & 0x7f)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			n = int(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			n = int(binary.BigEndian.Uint64(ext[:]))
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, mask[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x0, 0x1, 0x2: // continuation / text / binary
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		case 0x8: // close
			return nil, io.EOF
		case 0x9: // ping -> pong
			if err := w.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
		case 0xA: // pong; ignore
		}
	}
}

// call issues a CDP command and blocks until the matching response arrives,
// discarding unrelated events/responses (fine for sequential setup).
func (w *wsConn) call(method string, params any, sessionID string) (map[string]any, error) {
	w.nextID++
	id := w.nextID
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := w.writeFrame(0x1, b); err != nil {
		return nil, err
	}
	for {
		data, err := w.readMessage()
		if err != nil {
			return nil, err
		}
		var resp map[string]any
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		fid, ok := resp["id"].(float64)
		if !ok || int(fid) != id {
			continue
		}
		if e, ok := resp["error"]; ok {
			return nil, fmt.Errorf("cdp %s: %v", method, e)
		}
		return resp, nil
	}
}

// pageTargets returns the targetIds of all current page targets.
func (w *wsConn) pageTargets() ([]string, error) {
	resp, err := w.call("Target.getTargets", nil, "")
	if err != nil {
		return nil, err
	}
	result, _ := resp["result"].(map[string]any)
	infos, _ := result["targetInfos"].([]any)
	var ids []string
	for _, ti := range infos {
		t, _ := ti.(map[string]any)
		if t["type"] == "page" {
			if id, ok := t["targetId"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

// attach opens a flat session to a target and returns its sessionId.
func (w *wsConn) attach(targetID string) (string, error) {
	resp, err := w.call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "")
	if err != nil {
		return "", err
	}
	result, _ := resp["result"].(map[string]any)
	sid, ok := result["sessionId"].(string)
	if !ok {
		return "", fmt.Errorf("attachToTarget: no sessionId in response")
	}
	return sid, nil
}
