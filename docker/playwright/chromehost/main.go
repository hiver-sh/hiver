// Command chromehost is the resident Chrome host, driven over CDP. It is the
// playwright image entrypoint, so it runs during the prewarm boot and stays
// resident — every sandbox claimed from the warm pool inherits Chromium already
// launched and listening (captured in the microvm snapshot, or kept alive in the
// runc container).
//
// It is the stdlib-only Go replacement for the former Node.js chrome-cdp.cjs: a
// static binary with no dependencies, so the resident process captured in every
// sandbox snapshot is lean instead of a Node + V8 + playwright-core heap. It does
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
//   - warm the CDP attach path + open a page once before the snapshot, so a
//     resumed sandbox starts warm.
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
// Readiness signal: Chrome prints "DevTools listening on ws://..." to stderr once
// the endpoint is up; only then do we resolve the stable path, bring up the relay,
// warm it, and write READY_FILE. Under microvm isolation sbxguest waits for that
// file before letting the host snapshot the (now warm) VM. Under runc isolation
// the file is unused — container readiness is the poststart fifo.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
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
	"path/filepath"
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
	// warmupTimeout bounds the whole pre-snapshot warmup; on timeout we proceed to
	// signal readiness regardless (warmup is best-effort).
	warmupTimeout = 10 * time.Second

	readyFile = "/run/hiver/prewarm-ready"
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

// chromeArgs are the launch flags, tuned to run as cheaply as possible: collapse
// the process model to a single process (no zygote/renderer/GPU forks — lowest
// resident memory), drop the GPU/raster path (no paint is needed for
// DOMContentLoaded automation), disable image decode, shrink every cache, and
// strip background CPU/network chatter (incl. Google phone-homes blackholed via
// --host-resolver-rules). --single-process headless is not officially supported
// and can crash on heavy pages; we accept that for the memory win on simple
// navigation workloads — dropping --single-process/--no-zygote is the first revert
// if a workload starts crashing.
func chromeArgs(chromePort int, userDataDir string) []string {
	return []string{
		// DevTools/CDP endpoint. Chrome binds this on loopback only (see header);
		// the 0.0.0.0 relay is what the ingress proxy reaches. --remote-allow-origins
		// is required by Chrome 111+ or it 403s the connectOverCDP WebSocket upgrade.
		fmt.Sprintf("--remote-debugging-port=%d", chromePort),
		"--remote-allow-origins=*",
		"--headless=new",
		"--user-data-dir=" + userDataDir,

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
		"--blink-settings=imagesEnabled=false",
		"--enable-low-end-device-mode",
		"--disk-cache-size=1",
		"--media-cache-size=1",
		"--js-flags=--max-old-space-size=128",
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
		"--host-resolver-rules=MAP accounts.google.com 0.0.0.0, MAP *.googleapis.com 0.0.0.0, MAP *.clients.google.com 0.0.0.0, MAP mtalk.google.com 0.0.0.0, MAP clients*.google.com 0.0.0.0",
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

	cmd := exec.Command(env("HIVER_CHROME_BIN", chromeBin), chromeArgs(chromePort, userDataDir)...)
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
	// connections. Only then resolve the stable path, bring up the relay, warm it,
	// and signal readiness — so we never snapshot before Chrome (and a warm page)
	// can serve, and the warmup exercises the same relay path real clients use.
	var once sync.Once
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(os.Stderr, line)
		if strings.Contains(line, "DevTools listening on ws://") {
			once.Do(func() { go onReady(port, chromePort, cmd.Process.Pid) })
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

// onReady wires up the relay, warms the CDP path, and writes the readiness file,
// in that order, once Chrome's CDP endpoint is up.
func onReady(port, chromePort int, chromePid int) {
	browserWsPath, err := resolveBrowserWsPath(chromePort)
	if err != nil {
		log.Printf("resolve browser ws failed: %v", err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		log.Fatalf("relay listen: %v", err)
	}
	go serveRelay(ln, chromePort, browserWsPath)

	// Warm the CDP attach path + open a page through the relay before the snapshot,
	// so a resumed sandbox is already warm on the first client attach.
	warmup(port)

	if err := os.MkdirAll(filepath.Dir(readyFile), 0o755); err != nil {
		log.Printf("mkdir ready dir: %v", err)
	}
	if err := os.WriteFile(readyFile, []byte(strconv.Itoa(chromePid)), 0o644); err != nil {
		log.Printf("write ready file: %v", err)
	}
	disp := browserWsPath
	if disp == "" {
		disp = "/devtools/browser/<uuid>"
	}
	log.Printf("relay 0.0.0.0:%d%s -> 127.0.0.1:%d%s; browser ready", port, stablePath, chromePort, disp)
}

// resolveBrowserWsPath resolves Chrome's current browser WebSocket path
// (/devtools/browser/<uuid>) from /json/version. Hitting this also warms Chrome's
// CDP HTTP surface before the snapshot.
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

// warmup drives one real CDP attach through the relay before the snapshot is
// taken, so a resumed sandbox starts warm instead of paying cold-start on the
// first client attach: connecting makes Chrome accept its first WebSocket upgrade
// and warm the relay path, and creating a page spawns that page's renderer — both
// captured in the snapshot. Replaces the old Playwright/raw-/json/new warmup with
// a minimal stdlib WebSocket CDP client. Best-effort and time-bounded so a warmup
// failure never blocks readiness.
func warmup(port int) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		log.Printf("warmup dial: %v", err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(warmupTimeout))

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		return
	}
	req := "GET " + stablePath + " HTTP/1.1\r\n" +
		"Host: 127.0.0.1\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(keyRaw) + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		log.Printf("warmup write upgrade: %v", err)
		return
	}

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101") {
		log.Printf("warmup: no upgrade (%q): %v", strings.TrimSpace(status), err)
		return
	}
	for { // drain the rest of the upgrade response headers
		h, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if h == "\r\n" || h == "\n" {
			break
		}
	}

	id := 0
	call := func(method string, params map[string]any, sessionID string) (map[string]any, error) {
		id++
		want := id
		msg := map[string]any{"id": want, "method": method}
		if params != nil {
			msg["params"] = params
		}
		if sessionID != "" {
			msg["sessionId"] = sessionID
		}
		payload, _ := json.Marshal(msg)
		if err := writeFrame(conn, payload); err != nil {
			return nil, err
		}
		for { // read frames until the matching response id (skip events/others)
			data, err := readFrame(br)
			if err != nil {
				return nil, err
			}
			var resp map[string]any
			if json.Unmarshal(data, &resp) != nil {
				continue
			}
			if rid, ok := resp["id"].(float64); ok && int(rid) == want {
				return resp, nil
			}
		}
	}

	if _, err := call("Browser.getVersion", nil, ""); err != nil {
		log.Printf("warmup getVersion: %v", err)
		return
	}
	ct, err := call("Target.createTarget", map[string]any{"url": "about:blank"}, "")
	if err != nil {
		log.Printf("warmup createTarget: %v", err)
		return
	}
	targetID := digString(ct, "result", "targetId")
	if targetID == "" {
		return
	}
	at, err := call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "")
	if err != nil {
		log.Printf("warmup attachToTarget: %v", err)
		return
	}
	sessionID := digString(at, "result", "sessionId")
	if sessionID == "" {
		return
	}
	// Enable the core domains on the page session to spin up the renderer's
	// DevTools agent — the costly part of a cold first attach.
	_, _ = call("Page.enable", nil, sessionID)
	_, _ = call("Runtime.enable", nil, sessionID)
}

// digString walks nested map[string]any by keys and returns the string leaf, or
// "" if any hop is missing or mistyped.
func digString(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

// writeFrame writes one client→server WebSocket text frame (FIN, opcode 1).
// Client frames must be masked per RFC 6455, so each carries a random 4-byte mask.
func writeFrame(w io.Writer, payload []byte) error {
	n := len(payload)
	var header []byte
	switch {
	case n < 126:
		header = []byte{0x81, 0x80 | byte(n)}
	case n < 65536:
		header = []byte{0x81, 0x80 | 126, byte(n >> 8), byte(n)}
	default:
		header = []byte{0x81, 0x80 | 127,
			byte(n >> 56), byte(n >> 48), byte(n >> 40), byte(n >> 32),
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i&3]
	}
	buf := make([]byte, 0, len(header)+4+n)
	buf = append(buf, header...)
	buf = append(buf, mask[:]...)
	buf = append(buf, masked...)
	_, err := w.Write(buf)
	return err
}

// readFrame reads one server→client text frame, skipping ping/pong/continuation.
// Server frames are unmasked, but we honor the mask bit defensively. A close frame
// returns io.EOF.
func readFrame(br *bufio.Reader) ([]byte, error) {
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(br, h); err != nil {
			return nil, err
		}
		opcode := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		n := int(h[1] & 0x7f)
		switch n {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(br, ext); err != nil {
				return nil, err
			}
			n = int(ext[0])<<8 | int(ext[1])
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(br, ext); err != nil {
				return nil, err
			}
			n = 0
			for _, b := range ext {
				n = n<<8 | int(b)
			}
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(br, mask[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i&3]
			}
		}
		switch opcode {
		case 0x1: // text
			return payload, nil
		case 0x8: // close
			return nil, io.EOF
		default: // ping/pong/continuation — skip
			continue
		}
	}
}
