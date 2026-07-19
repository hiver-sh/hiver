//go:build linux

package proxy

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/hiver-sh/hiver/internal/wsaudit"
)

// maxAuditBodyBytes caps the request body snapshot captured for audit so a
// large POST can't OOM the proxy. Upstream still receives the full body —
// the bound only affects the audit payload.
const maxAuditBodyBytes = 1 << 20 // 1 MiB

// forwardHTTP forwards a single HTTP request from client to upstream and
// streams the response back. On a 101 Switching Protocols response it
// upgrades to bidirectional WebSocket frame forwarding until either side
// closes. Caller owns both connections.
//
// upstreamBR is the buffered reader to read the response from — carried with a
// pooled connection so response framing survives reuse (for a freshly dialed
// conn the caller passes bufio.NewReader(upstream)). rawReqBytes is optional:
// when non-nil it's written verbatim to upstream instead of req.Write — used in
// the TLS-intercept WS path to preserve the agent's exact header casing.
//
// Returns true only when allowReuse was set and the exchange ended keep-alive-
// clean (upstream agreed to keep-alive and the body fully drained), so the caller
// may return the connection to the upstream pool; otherwise the caller must close
// it. WebSocket upgrades and any error path return false.
func (p *Proxy) forwardHTTP(client io.ReadWriter, upstream net.Conn, upstreamBR *bufio.Reader, req *http.Request, rawReqBytes []byte, ac *auditCtx, allowReuse bool) bool {
	ws := wsaudit.IsUpgrade(req)
	// keepAlive drives both the upstream request (omit "Connection: close") and the
	// reuse decision. Never for WebSocket (the conn becomes a tunnel) or when the
	// agent itself asked to close.
	keepAlive := allowReuse && !ws && !req.Close

	// Strip Sec-WebSocket-Extensions so the server can't negotiate any
	// RSV-using extension (notably permessage-deflate). Frames stay
	// plain on the wire and the audit log records native payloads.
	if ws {
		if rawReqBytes != nil {
			rawReqBytes = wsaudit.StripExtensionsRaw(rawReqBytes)
		} else {
			wsaudit.StripExtensions(req.Header)
		}
	}

	if err := writeUpstreamRequest(upstream, req, rawReqBytes, ws, keepAlive); err != nil {
		log.Printf("forward http: write request error host=%s: %v", req.Host, err)
		ac.responseError("write request: "+err.Error(), http.StatusBadGateway)
		return false
	}

	resp, err := http.ReadResponse(upstreamBR, req)
	if err != nil {
		log.Printf("forward http: read response error host=%s: %v", req.Host, err)
		ac.responseError("read response: "+err.Error(), http.StatusBadGateway)
		return false
	}
	log.Printf("forward http: response host=%s status=%d", req.Host, resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSwitchingProtocols {
		if err := writeWebSocketResponse(client, resp); err != nil {
			ac.responseError("write 101: "+err.Error(), http.StatusInternalServerError)
			return false
		}
		// Response event before the WS tunnel starts pumping — frame audit
		// events flow as response_chunks from wsaudit.Forward, so consumers see
		// the 101 immediately rather than at tunnel close.
		ac.responseHeaders = headerMap(resp.Header)
		ac.response(http.StatusSwitchingProtocols)
		// upstreamBR may already hold the first WS frame bytes
		// http.ReadResponse over-read past the headers — drain it before
		// reading further from upstream.
		done := make(chan struct{}, 2)
		go func() { wsaudit.Forward(client, upstream, wsaudit.DirUp, ac.host, ac.streamChunk); done <- struct{}{} }()
		go func() {
			wsaudit.Forward(io.MultiReader(upstreamBR, upstream), client, wsaudit.DirDown, ac.host, ac.streamChunk)
			done <- struct{}{}
		}()
		<-done
		return false
	}

	src := unwrapBody(resp)
	ac.responseHeaders = headerMap(resp.Header)
	if err := wsaudit.WriteResponseHeaders(client, resp); err != nil {
		ac.responseError("write response: "+err.Error(), http.StatusBadGateway)
		return false
	}
	// Emit the response event now — status, headers, and time-to-first-byte
	// are known. Body bytes flow as response_chunk events from chunkForward
	// so consumers (especially SSE) don't have to wait for the body to end.
	ac.response(resp.StatusCode)
	p.chunkForward(src, client, nil, ac, isTextContentType(resp.Header.Get("Content-Type")))

	if !keepAlive {
		return false
	}
	// Reuse only if the upstream is fully drained (so the next pooled response
	// starts clean) and it agreed to keep-alive. chunkForward consumes the body
	// view; drain the underlying body within a bound to confirm — leftover (a
	// mid-stream error or oversized residue) means the conn isn't safe to reuse.
	n, drainErr := io.CopyN(io.Discard, resp.Body, maxReuseDrainBytes)
	fullyDrained := drainErr == io.EOF || (drainErr == nil && n < maxReuseDrainBytes)
	return fullyDrained && !resp.Close && resp.ProtoAtLeast(1, 1) && upstreamBR.Buffered() == 0
}

// maxReuseDrainBytes bounds the post-stream drain used to decide whether an
// upstream conn is safe to pool — a large unread residue isn't worth draining
// just to reuse, so the conn is closed instead.
const maxReuseDrainBytes = 1 << 16

// writeUpstreamRequest writes req to upstream, choosing one of three paths:
//
//   - WS upgrade with captured raw bytes → write the bytes verbatim
//     (preserves header casing/order from the agent's original request).
//   - WS upgrade without raw bytes → wsaudit.WriteUpgradeRequest (hand-rolled
//     header writer that avoids the User-Agent injection req.Write would do).
//   - Anything else → req.Write with the proxy fingerprint suppressed
//     (blank User-Agent if the agent didn't send one). Connection is set to
//     close unless keepAlive is set, in which case the header is dropped so the
//     upstream may keep the connection alive for pooling/reuse.
func writeUpstreamRequest(upstream net.Conn, req *http.Request, rawReqBytes []byte, ws, keepAlive bool) error {
	switch {
	case ws && rawReqBytes != nil:
		log.Printf("forward http: ws upgrade host=%s path=%s rawBytes=%d", req.Host, req.URL.Path, len(rawReqBytes))
		_, err := upstream.Write(rawReqBytes)
		return err
	case ws:
		log.Printf("forward http: ws upgrade host=%s path=%s", req.Host, req.URL.Path)
		return wsaudit.WriteUpgradeRequest(upstream, req)
	default:
		if keepAlive {
			req.Header.Del("Connection") // HTTP/1.1 default is keep-alive
		} else {
			req.Header.Set("Connection", "close")
		}
		// Prevent req.Write from injecting "User-Agent: Go-http-client/1.1"
		// when the agent didn't send one — that header fingerprints the proxy.
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header["User-Agent"] = []string{""}
		}
		return req.Write(upstream)
	}
}

// applyRequestRule captures a bounded snapshot of req.Body for audit, looks
// up the matching egress rule, applies override (or denies via onDeny +
// audit), and emits the allow event. Returns the matched rule, or nil if
// denied. req.Body is replaced with a fresh reader that still yields the
// full original stream so the caller can forward it.
func (p *Proxy) applyRequestRule(req *http.Request, host string, port int, srcIP string, ac *auditCtx, onDeny func()) *EgressRule {
	if req.Body != nil {
		captured, replay, err := captureBody(req.Body, maxAuditBodyBytes)
		if err != nil {
			log.Printf("applyRequestRule: read body host=%s: %v", host, err)
		} else {
			req.Body = replay
			ac.requestBody = decodeBytes(req.Header.Get("Content-Encoding"), captured)
		}
	}
	ac.requestHeaders = headerMap(req.Header)
	rule := MatchEgress(p.rulesForSource(srcIP), req.Method, host, port, req.URL.Path)
	if rule == nil || rule.Access == "deny" {
		log.Printf("applyRequestRule: denied host=%s port=%d method=%s path=%s", host, port, req.Method, req.URL.Path)
		ac.deny("no matching rule", http.StatusForbidden)
		if onDeny != nil {
			onDeny()
		}
		return nil
	}
	if rule.Access == "allow" {
		applyOverride(req, rule.Override)
		ac.requestHeaders = headerMap(req.Header)
		if rule.Override != nil && rule.Override.Host != "" {
			ac.upstream = dialTarget(rule, net.JoinHostPort(host, strconv.Itoa(port)))
		}
	}
	ac.allow()
	if rule.OverrideScript != "" {
		if err := runOverrideScript(req, rule.OverrideScript); err != nil {
			log.Printf("applyRequestRule: override_script host=%s path=%s: %v", host, req.URL.Path, err)
		}
	}
	req.RequestURI = ""
	return rule
}

// captureBody reads up to maxBytes from body and returns a (snapshot,
// replayBody) pair. The caller can audit the snapshot while still forwarding
// the full body downstream — replayBody yields the captured bytes followed
// by whatever the underlying reader still holds.
func captureBody(body io.ReadCloser, maxBytes int) ([]byte, io.ReadCloser, error) {
	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(body, buf)
	captured := buf[:n]
	switch err {
	case nil:
		// Body has at least maxBytes — splice captured in front of the rest.
		return captured, io.NopCloser(io.MultiReader(bytes.NewReader(captured), body)), nil
	case io.EOF, io.ErrUnexpectedEOF:
		// Body fit within maxBytes; the underlying reader is drained.
		return captured, io.NopCloser(bytes.NewReader(captured)), nil
	default:
		return nil, body, err
	}
}
