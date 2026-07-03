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

	// profileDir is the single, consistent Chrome profile location. A stable dir
	// (rather than a fresh throwaway tmp dir per launch) means sign-in state and
	// stored credentials persist across launches, and the profile path captured in
	// a VM snapshot is deterministic instead of a random /tmp/hiver-chrome-* path.
	profileDir = "/opt/hiver/chrome-profile"

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

// passkeyStorePath is where created passkeys are persisted so they survive a
// restart (not just a VM snapshot). It lives inside the persistent profile dir.
func passkeyStorePath() string { return profileDir + "/passkeys.json" }

// loadPasskeys reads the persisted passkeys, if any. A missing file (first run)
// is not an error — it just means no credentials to restore yet.
func loadPasskeys() []map[string]any {
	b, err := os.ReadFile(passkeyStorePath())
	if err != nil {
		return nil
	}
	var creds []map[string]any
	if err := json.Unmarshal(b, &creds); err != nil {
		log.Printf("passkeys: load %s: %v", passkeyStorePath(), err)
		return nil
	}
	return creds
}

// savePasskeys writes the passkeys atomically (temp file + rename) so a crash
// mid-write can't corrupt the store.
func savePasskeys(creds []map[string]any) error {
	b, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := passkeyStorePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, passkeyStorePath())
}

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

	// Single consistent profile location (see profileDir) — persists sign-in state
	// and stored credentials across launches instead of a throwaway tmp dir.
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		log.Fatalf("create user-data-dir: %v", err)
	}

	userAgent := env("HIVER_BROWSER_USER_AGENT", defaultUserAgent)
	cmd := exec.Command(env("HIVER_CHROME_BIN", chromeBin), chromeArgs(chromePort, profileDir, userAgent)...)
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

	// Apply per-session CDP setup the launch flags can't express: the Sec-CH-UA
	// client-hint override and a WebAuthn virtual authenticator for passkeys.
	go configureTargets(chromePort, browserWsPath, userAgent)

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

// configureTargets applies the per-session CDP setup that Chrome's launch flags
// can't express, on *every* page target — existing and future — via
// Target.setAutoAttach, so it covers freshly created newContext()/newPage()
// targets, not just the ones open at startup:
//
//   - Emulation.setUserAgentOverride — fixes the Sec-CH-UA client hints (and
//     navigator.userAgentData), whose brand list is baked into the Chrome build
//     and reports "HeadlessChrome" under --headless=new; the --user-agent launch
//     flag can't touch them.
//
//   - WebAuthn virtual authenticator — headless Chrome has no platform
//     authenticator (no screen lock / biometric / TPM), so passkey ceremonies
//     fail with "a passkey can't be created on this device". A CDP virtual
//     authenticator (transport "internal", resident-key + auto-verified) makes
//     both create() and get() succeed unattended. The authenticator is bound to
//     the page's frame host, so a client that drives the page over its own CDP
//     session (Playwright connectOverCDP) inherits it. Passkeys are persisted to
//     disk (see passkeyStorePath) and reloaded onto every target, and any new
//     passkey is cross-injected into the other live targets — so a credential
//     registered in one page is usable for login in any other page and survives a
//     restart. Otherwise the authenticator comes up empty and the site's get()
//     hangs forever waiting for an assertion (the "your device will ask for your
//     fingerprint…" loop).
//
// The CDP connection is held open for the life of the process: both overrides are
// scoped to the attached session and would be reverted the moment we detached.
//
// Failures are logged, never fatal: broken setup must not take Chrome down.
func configureTargets(chromePort int, browserWsPath, userAgent string) {
	if browserWsPath == "" {
		log.Printf("configure: no browser ws path; skipping client-hint override and virtual authenticator")
		return
	}
	addr := fmt.Sprintf("127.0.0.1:%d", chromePort)

	ws, err := wsConnect(addr, browserWsPath)
	if err != nil {
		log.Printf("configure: connect: %v", err)
		return
	}

	m := newSessionManager(ws, userAgent)
	for _, c := range loadPasskeys() {
		if id, ok := c["credentialId"].(string); ok {
			m.creds[id] = c
		}
	}
	log.Printf("configure: loaded %d persisted passkey(s)", len(m.creds))

	// Auto-attach to every page target, existing and future, so both overrides
	// cover all pages/contexts. flatten routes each session by the sessionId field
	// on this one browser connection.
	if _, err := ws.call("Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true,
	}, ""); err != nil {
		log.Printf("configure: setAutoAttach: %v", err)
		ws.close()
		return
	}

	go m.persistLoop()

	// Dispatch target lifecycle events for the life of the process.
	for {
		select {
		case msg := <-ws.events:
			m.handleEvent(msg)
		case <-ws.closed:
			return
		}
	}
}

// authRef identifies one page target's virtual authenticator.
type authRef struct {
	sessionID string
	authID    string
}

// sessionManager provisions per-page CDP overrides and keeps every page target's
// virtual authenticator in sync with the persisted passkey store.
type sessionManager struct {
	ws       *wsConn
	uaParams map[string]any

	mu        sync.Mutex
	auths     map[string]authRef        // sessionID -> authenticator
	creds     map[string]map[string]any // credentialId -> canonical credential
	lastSaved string                    // marshaled creds last written to disk (persistLoop only)
}

func newSessionManager(ws *wsConn, userAgent string) *sessionManager {
	return &sessionManager{
		ws: ws,
		uaParams: map[string]any{
			"userAgent":         userAgent,
			"userAgentMetadata": userAgentMetadata(),
		},
		auths: make(map[string]authRef),
		creds: make(map[string]map[string]any),
	}
}

func (m *sessionManager) handleEvent(msg map[string]any) {
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)
	switch method {
	case "Target.attachedToTarget":
		sid, _ := params["sessionId"].(string)
		info, _ := params["targetInfo"].(map[string]any)
		if sid == "" || info == nil || info["type"] != "page" {
			return
		}
		go m.provision(sid)
	case "Target.detachedFromTarget":
		if sid, _ := params["sessionId"].(string); sid != "" {
			m.mu.Lock()
			delete(m.auths, sid)
			m.mu.Unlock()
		}
	}
}

// provision applies the UA override and a virtual authenticator to a newly
// attached page target, then restores every known passkey into it.
func (m *sessionManager) provision(sessionID string) {
	if _, err := m.ws.call("Emulation.setUserAgentOverride", m.uaParams, sessionID); err != nil {
		log.Printf("configure: setUserAgentOverride on %s: %v", sessionID, err)
	}
	authID, err := m.ws.addVirtualAuthenticator(sessionID)
	if err != nil {
		log.Printf("configure: addVirtualAuthenticator on %s: %v", sessionID, err)
		return
	}
	m.mu.Lock()
	m.auths[sessionID] = authRef{sessionID, authID}
	known := make([]map[string]any, 0, len(m.creds))
	for _, c := range m.creds {
		known = append(known, c)
	}
	m.mu.Unlock()

	for _, c := range known {
		if err := m.ws.addCredential(sessionID, authID, c); err != nil {
			log.Printf("configure: addCredential on %s: %v", sessionID, err)
		}
	}
	log.Printf("configure: session %s ready (UA override + authenticator + %d passkey(s))", sessionID, len(known))
}

// persistLoop periodically reconciles all live authenticators with the passkey
// store: it collects newly created passkeys, writes them to disk, and
// cross-injects them into every other page so a credential registered in one page
// works for login in any page. Runs until the CDP connection drops.
func (m *sessionManager) persistLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ws.closed:
			return
		case <-ticker.C:
			m.reconcile()
		}
	}
}

func (m *sessionManager) reconcile() {
	m.mu.Lock()
	auths := make([]authRef, 0, len(m.auths))
	for _, a := range m.auths {
		auths = append(auths, a)
	}
	m.mu.Unlock()

	// Poll every live authenticator.
	authCreds := make(map[string][]map[string]any, len(auths))
	var dead []string
	for _, a := range auths {
		creds, err := m.ws.getCredentials(a.sessionID, a.authID)
		if err != nil {
			dead = append(dead, a.sessionID)
			continue
		}
		authCreds[a.sessionID] = creds
	}

	// Union by credentialId, keeping the highest sign counter.
	union := make(map[string]map[string]any)
	for _, creds := range authCreds {
		for _, c := range creds {
			id, _ := c["credentialId"].(string)
			if id == "" {
				continue
			}
			if ex, ok := union[id]; !ok || signCount(c) > signCount(ex) {
				union[id] = c
			}
		}
	}

	m.mu.Lock()
	for _, sid := range dead {
		delete(m.auths, sid)
	}
	// Keep disk-known credentials that no live authenticator reported (e.g. no
	// page open yet) so they survive in the store and get restored later.
	for id, c := range m.creds {
		if _, ok := union[id]; !ok {
			union[id] = c
		}
	}
	m.creds = union
	auths = auths[:0]
	for _, a := range m.auths {
		auths = append(auths, a)
	}
	m.mu.Unlock()

	// Cross-inject: ensure every live authenticator holds every credential.
	for _, a := range auths {
		have := make(map[string]bool)
		for _, c := range authCreds[a.sessionID] {
			if id, ok := c["credentialId"].(string); ok {
				have[id] = true
			}
		}
		for id, c := range union {
			if !have[id] {
				if err := m.ws.addCredential(a.sessionID, a.authID, c); err != nil {
					log.Printf("configure: cross-inject on %s: %v", a.sessionID, err)
				}
			}
		}
	}

	// Persist to disk when the credential set changes.
	b, err := json.Marshal(union) // map keys are marshaled sorted → stable
	if err != nil || string(b) == m.lastSaved {
		return
	}
	slice := make([]map[string]any, 0, len(union))
	for _, c := range union {
		slice = append(slice, c)
	}
	if err := savePasskeys(slice); err != nil {
		log.Printf("passkeys: save: %v", err)
		return
	}
	m.lastSaved = string(b)
	log.Printf("passkeys: persisted %d credential(s) to %s", len(union), passkeyStorePath())
}

// signCount reads a credential's WebAuthn sign counter (0 if absent).
func signCount(c map[string]any) float64 {
	v, _ := c["signCount"].(float64)
	return v
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
// (handling fragmentation + ping).
//
// A single readLoop goroutine owns all reads and routes each frame either to the
// call() that is waiting on its id or to the events channel. That lets many
// goroutines issue call()s concurrently (needed for Target.setAutoAttach, where
// targets are provisioned in parallel as they attach). Writes are serialized by
// wmu.

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex // serializes frame writes

	mu      sync.Mutex // guards nextID + pending
	nextID  int
	pending map[int]chan map[string]any

	events    chan map[string]any // CDP events (messages with a "method")
	closed    chan struct{}       // closed once the connection is torn down
	closeOnce sync.Once
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
	w := &wsConn{
		conn:    conn,
		br:      br,
		pending: make(map[int]chan map[string]any),
		events:  make(chan map[string]any, 64),
		closed:  make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

func (w *wsConn) close() { w.shutdown() }

// shutdown tears the connection down once, unblocking readLoop, call()s waiting
// on a response, and anything selecting on closed.
func (w *wsConn) shutdown() {
	w.closeOnce.Do(func() {
		close(w.closed)
		_ = w.conn.Close()
	})
}

// readLoop is the sole reader: it dispatches each frame to the waiting call() (by
// id) or onto the events channel, until the connection drops.
func (w *wsConn) readLoop() {
	defer w.shutdown()
	for {
		data, err := w.readMessage()
		if err != nil {
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if idf, ok := msg["id"].(float64); ok {
			w.mu.Lock()
			ch := w.pending[int(idf)]
			delete(w.pending, int(idf))
			w.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
			continue
		}
		if _, ok := msg["method"]; ok {
			select {
			case w.events <- msg:
			case <-w.closed:
				return
			}
		}
	}
}

// writeFrame writes a single final client frame (always masked, per RFC 6455).
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()

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

// call issues a CDP command and blocks until readLoop delivers the matching
// response (or the connection drops). Safe to call concurrently.
func (w *wsConn) call(method string, params any, sessionID string) (map[string]any, error) {
	w.mu.Lock()
	w.nextID++
	id := w.nextID
	ch := make(chan map[string]any, 1)
	w.pending[id] = ch
	w.mu.Unlock()

	unregister := func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	}

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	b, err := json.Marshal(msg)
	if err != nil {
		unregister()
		return nil, err
	}
	if err := w.writeFrame(0x1, b); err != nil {
		unregister()
		return nil, err
	}
	select {
	case resp := <-ch:
		if e, ok := resp["error"]; ok {
			return nil, fmt.Errorf("cdp %s: %v", method, e)
		}
		return resp, nil
	case <-w.closed:
		unregister()
		return nil, fmt.Errorf("cdp %s: connection closed", method)
	}
}

// addVirtualAuthenticator provisions a CDP virtual platform authenticator on the
// given flat session so WebAuthn passkey ceremonies work in headless Chrome,
// which otherwise has no platform authenticator and reports "a passkey can't be
// created on this device". transport:"internal" presents it as a platform
// authenticator (so isUserVerifyingPlatformAuthenticatorAvailable() is true);
// hasResidentKey enables discoverable credentials (passkeys); isUserVerified +
// automaticPresenceSimulation make create()/get() complete with no user gesture
// or biometric prompt.
//
// Credentials created here live in the authenticator (Chrome's process memory),
// not in the --user-data-dir profile — so they ride a VM snapshot but not a plain
// restart. Export/re-inject with WebAuthn.getCredentials / WebAuthn.addCredential
// for durable persistence.
func (w *wsConn) addVirtualAuthenticator(sessionID string) (string, error) {
	if _, err := w.call("WebAuthn.enable", map[string]any{"enableUI": false}, sessionID); err != nil {
		return "", fmt.Errorf("WebAuthn.enable: %w", err)
	}
	resp, err := w.call("WebAuthn.addVirtualAuthenticator", map[string]any{
		"options": map[string]any{
			"protocol":                    "ctap2",
			"transport":                   "internal",
			"hasResidentKey":              true,
			"hasUserVerification":         true,
			"isUserVerified":              true,
			"automaticPresenceSimulation": true,
		},
	}, sessionID)
	if err != nil {
		return "", err
	}
	result, _ := resp["result"].(map[string]any)
	id, _ := result["authenticatorId"].(string)
	if id == "" {
		return "", fmt.Errorf("addVirtualAuthenticator: no authenticatorId in response")
	}
	return id, nil
}

// addCredential injects a previously exported passkey into an authenticator. The
// CDP WebAuthn.Credential type is shared by getCredentials (output) and
// addCredential (input), so a credential round-trips through the on-disk store
// without any field massaging.
func (w *wsConn) addCredential(sessionID, authID string, cred map[string]any) error {
	_, err := w.call("WebAuthn.addCredential", map[string]any{
		"authenticatorId": authID,
		"credential":      cred,
	}, sessionID)
	return err
}

// getCredentials returns every credential currently held by an authenticator,
// including newly created passkeys and the advanced sign counter.
func (w *wsConn) getCredentials(sessionID, authID string) ([]map[string]any, error) {
	resp, err := w.call("WebAuthn.getCredentials", map[string]any{"authenticatorId": authID}, sessionID)
	if err != nil {
		return nil, err
	}
	result, _ := resp["result"].(map[string]any)
	raw, _ := result["credentials"].([]any)
	creds := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			creds = append(creds, m)
		}
	}
	return creds, nil
}
