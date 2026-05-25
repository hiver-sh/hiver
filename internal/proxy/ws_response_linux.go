//go:build linux

package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// writeWebSocketResponse writes a 101 Switching Protocols response with RFC
// 6455 header casing for the WebSocket-spec headers. Go's http.Header
// canonicalizes "Sec-WebSocket-Accept" to "Sec-Websocket-Accept" (lower-case
// W/A) because CanonicalMIMEHeaderKey uppercases only the first letter of
// each '-' segment. Strict WS clients (notably tungstenite) case-match the
// RFC spelling and reject the mismatch with "Key mismatch" / "Attack attempt
// detected". Writing by hand sidesteps the canonicalizer.
//
// Non-handshake headers (Date, Server, Cf-Ray, …) pass through with Go's
// canonical casing — clients only case-match the WS-spec headers.
func writeWebSocketResponse(w io.Writer, resp *http.Response) error {
	bw := bufio.NewWriter(w)
	statusText := http.StatusText(resp.StatusCode)
	if statusText == "" {
		statusText = "Switching Protocols"
	}
	if _, err := fmt.Fprintf(bw, "HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText); err != nil {
		return err
	}
	hdrs := resp.Header.Clone()
	hdrs.Del("Transfer-Encoding")
	rfcCasing := map[string]string{
		"sec-websocket-accept":     "Sec-WebSocket-Accept",
		"sec-websocket-protocol":   "Sec-WebSocket-Protocol",
		"sec-websocket-extensions": "Sec-WebSocket-Extensions",
		"sec-websocket-version":    "Sec-WebSocket-Version",
	}
	for canon, values := range hdrs {
		name := canon
		if rfc, ok := rfcCasing[strings.ToLower(canon)]; ok {
			name = rfc
		}
		for _, v := range values {
			if _, err := fmt.Fprintf(bw, "%s: %s\r\n", name, v); err != nil {
				return err
			}
		}
	}
	if _, err := io.WriteString(bw, "\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}
