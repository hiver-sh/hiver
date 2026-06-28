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
	// connections. Only then resolve the stable path and bring up the relay.
	var once sync.Once
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(os.Stderr, line)
		if strings.Contains(line, "DevTools listening on ws://") {
			once.Do(func() { go onReady(port, chromePort) })
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
func onReady(port, chromePort int) {
	browserWsPath, err := resolveBrowserWsPath(chromePort)
	if err != nil {
		log.Printf("resolve browser ws failed: %v", err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		log.Fatalf("relay listen: %v", err)
	}
	go serveRelay(ln, chromePort, browserWsPath)

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
