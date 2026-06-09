package e2e_test

import (
	"testing"

	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestWebSocketE2E verifies that the transparent egress proxy correctly
// tunnels WebSocket (ws://) connections:
//
//   - An allowed ws:// upgrade reaches the host-side echo server, frames
//     are forwarded bidirectionally, and the proxy emits response_chunk audit
//     events for each frame.
//   - A ws:// upgrade to a host not in the egress allowlist is rejected
//     with 403 before the upstream is ever dialled.
//
// The fixture agent connects to upstream-ws:17082 (the WebSocket echo
// server started by setup.StartWSEchoServer) and upstream-denied:17082
// (blocked by the egress rules in spec.yaml).
func TestWebSocketE2E(t *testing.T) {
	setup.RunFixtureE2E(t, "agent-websocket")
}
