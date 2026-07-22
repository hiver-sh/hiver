package handlers

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
)

// ingressFixture wires a broker-backed Sandbox in front of a backend workload
// and exposes it through a gin reverse-proxy frontend, returning the frontend
// base URL, the backend's port, and a func that drains every event published so
// far.
func ingressFixture(t *testing.T, backend *httptest.Server) (frontURL, port string, drain func() []any) {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatalf("split backend url: %v", err)
	}

	b := events.New(0, 0)
	_, ch, cancel := b.Subscribe(0)
	t.Cleanup(cancel)

	s := NewSandbox("k", 0)
	s.SetBroker(b)
	s.SetProxyHost(host)

	gin.SetMode(gin.TestMode)
	eng := gin.New()
	handler := func(c *gin.Context) { s.ProxyGet(c, c.Param("port"), c.Param("path")) }
	eng.GET("/proxy/:port/*path", handler)
	front := httptest.NewServer(eng)
	t.Cleanup(front.Close)

	drain = func() []any {
		var out []any
		deadline := time.After(500 * time.Millisecond)
		for {
			select {
			case e := <-ch:
				v, err := e.Event.ValueByDiscriminator()
				if err != nil {
					t.Fatalf("decode event: %v", err)
				}
				out = append(out, v)
			case <-deadline:
				return out
			}
		}
	}
	return front.URL, port, drain
}

func TestIngressSSEStreamsChunks(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("backend writer is not a Flusher")
			return
		}
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: msg%d\n\n", i)
			fl.Flush()
		}
	}))
	defer backend.Close()

	frontURL, port, drain := ingressFixture(t, backend)
	resp, err := http.Get(frontURL + "/proxy/" + port + "/sse")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "data: msg2") {
		t.Fatalf("client did not receive full SSE stream, got %q", body)
	}

	var reqID, respID, firstChunkID int
	var chunks []string
	var status int
	for _, ev := range drain() {
		switch e := ev.(type) {
		case gen.IngressRequestEvent:
			reqID = e.Id
		case gen.IngressResponseEvent:
			respID = e.Id
			status = e.Status
		case gen.IngressChunkEvent:
			if firstChunkID == 0 {
				firstChunkID = e.Id
			}
			chunks = append(chunks, e.Body)
		}
	}

	if reqID == 0 {
		t.Error("no ingress.request event")
	}
	if respID == 0 {
		t.Fatal("no ingress.response event")
	}
	if status != http.StatusOK {
		t.Errorf("ingress.response status = %d, want 200", status)
	}
	// The response event is emitted at header time — before any body chunk —
	// so a consumer sees the stream open immediately, not at close.
	if firstChunkID != 0 && respID > firstChunkID {
		t.Errorf("ingress.response id %d came after first chunk id %d", respID, firstChunkID)
	}
	joined := strings.Join(chunks, "")
	for i := 0; i < 3; i++ {
		if !strings.Contains(joined, fmt.Sprintf("data: msg%d", i)) {
			t.Errorf("chunks missing msg%d; got %q", i, joined)
		}
	}
}

func TestIngressPlainResponseBodyFlowsAsChunks(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()

	frontURL, port, drain := ingressFixture(t, backend)
	resp, err := http.Get(frontURL + "/proxy/" + port + "/api")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var sawResponse bool
	var body string
	for _, ev := range drain() {
		switch e := ev.(type) {
		case gen.IngressResponseEvent:
			sawResponse = true
			if e.Status != http.StatusOK {
				t.Errorf("status = %d, want 200", e.Status)
			}
		case gen.IngressChunkEvent:
			body += e.Body
		}
	}
	if !sawResponse {
		t.Fatal("no ingress.response event")
	}
	if body != `{"ok":true}` {
		t.Errorf("chunk body = %q, want the JSON payload", body)
	}
}

// TestIngressChunkNotCapturedWhenNotObserved checks that narrowing
// SandboxConfig.events to exclude ingress.request/ingress.chunk stops the
// proxy from capturing request/response bodies at all — not just from
// publishing them. ingress.response (kept observed here) still carries no
// body by design, so its presence confirms the stream isn't silenced
// wholesale, only the excluded types' capture work.
func TestIngressChunkNotCapturedWhenNotObserved(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "request payload" {
			t.Errorf("backend saw body %q, want %q", body, "request payload")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatalf("split backend url: %v", err)
	}
	b := events.New(0, 0)
	b.SetFilter([]string{"ingress.response"}) // request/chunk excluded
	_, ch, cancel := b.Subscribe(0)
	t.Cleanup(cancel)

	s := NewSandbox("k", 0)
	s.SetBroker(b)
	s.SetProxyHost(host)

	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.POST("/proxy/:port/*path", func(c *gin.Context) { s.ProxyGet(c, c.Param("port"), c.Param("path")) })
	front := httptest.NewServer(eng)
	t.Cleanup(front.Close)

	resp, err := http.Post(front.URL+"/proxy/"+port+"/api", "text/plain", strings.NewReader("request payload"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var sawResponse, sawRequest, sawChunk bool
	deadline := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case e := <-ch:
			v, err := e.Event.ValueByDiscriminator()
			if err != nil {
				t.Fatalf("decode event: %v", err)
			}
			switch v.(type) {
			case gen.IngressResponseEvent:
				sawResponse = true
			case gen.IngressRequestEvent:
				sawRequest = true
			case gen.IngressChunkEvent:
				sawChunk = true
			}
		case <-deadline:
			break drain
		}
	}
	if !sawResponse {
		t.Fatal("ingress.response should still be observed")
	}
	if sawRequest {
		t.Error("ingress.request should be filtered out, but one was published")
	}
	if sawChunk {
		t.Error("ingress.chunk should be filtered out, but one was published")
	}
}

func TestIngressWebSocketLogsFramesUpAndDown(t *testing.T) {
	// Backend: accept the upgrade and echo each frame back unmasked.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected ws", http.StatusBadRequest)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		br := bufio.NewReader(conn)
		for {
			opcode, payload, err := wsReadFrameTest(br)
			if err != nil || opcode == 0x08 {
				return
			}
			if _, err := conn.Write(wsMakeFrame(opcode, false, payload)); err != nil {
				return
			}
		}
	}))
	defer backend.Close()

	frontURL, port, drain := ingressFixture(t, backend)

	// Dial the frontend directly and drive a raw WebSocket handshake.
	fconn, err := net.DialTimeout("tcp", strings.TrimPrefix(frontURL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatalf("dial frontend: %v", err)
	}
	defer fconn.Close()
	fmt.Fprintf(fconn,
		"GET /proxy/%s/ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
		port,
	)
	br := bufio.NewReader(fconn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}

	msg := []byte("hello ingress ws")
	if _, err := fconn.Write(wsMakeFrame(0x1, true, msg)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	opcode, got, err := wsReadFrameTest(br)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if opcode != 0x1 || string(got) != string(msg) {
		t.Errorf("echo opcode=%d payload=%q, want 1 / %q", opcode, got, msg)
	}
	// Send a close so both tunnel goroutines exit and flush their audit events.
	_, _ = fconn.Write(wsMakeFrame(0x08, true, nil))
	time.Sleep(50 * time.Millisecond)

	var status int
	labels := map[string]string{}
	for _, ev := range drain() {
		switch e := ev.(type) {
		case gen.IngressResponseEvent:
			status = e.Status
		case gen.IngressChunkEvent:
			label := ""
			if e.Label != nil {
				label = *e.Label
			}
			if e.Body == string(msg) {
				labels[label] = e.Body
			}
		}
	}
	if status != http.StatusSwitchingProtocols {
		t.Errorf("ingress.response status = %d, want 101", status)
	}
	if _, ok := labels["up"]; !ok {
		t.Errorf("missing ingress.chunk label=up; labels=%+v", labels)
	}
	if _, ok := labels["down"]; !ok {
		t.Errorf("missing ingress.chunk label=down; labels=%+v", labels)
	}
}

// wsMakeFrame builds a single WebSocket frame; masked when fromClient.
func wsMakeFrame(opcode byte, fromClient bool, payload []byte) []byte {
	frame := []byte{0x80 | opcode} // FIN=1
	pl := len(payload)
	if fromClient {
		frame = append(frame, 0x80|byte(pl))
		key := [4]byte{0x37, 0xFA, 0x21, 0x3D}
		frame = append(frame, key[:]...)
		for i, b := range payload {
			frame = append(frame, b^key[i%4])
		}
	} else {
		frame = append(frame, byte(pl))
		frame = append(frame, payload...)
	}
	return frame
}

func wsReadFrameTest(r io.Reader) (opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return
	}
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	pl := int(hdr[1] & 0x7F)
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return
		}
	}
	payload = make([]byte, pl)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return
}
