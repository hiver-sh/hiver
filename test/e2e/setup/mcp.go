package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConnectMCP dials a streamable-HTTP MCP server at url, polling up to
// 90 s for it to become reachable. podOut is appended to fatal messages.
func ConnectMCP(t *testing.T, ctx context.Context, url string, podOut *bytes.Buffer) *mcp.ClientSession {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0.0.0"}, nil)
	var lastErr error
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		session, err := client.Connect(dialCtx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
		cancel()
		if err == nil {
			return session
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("MCP server not reachable at %s: %v\npod output:\n%s", url, lastErr, podOut.String())
	return nil
}

// CallMCP invokes the named tool on sess and unmarshals the result JSON into out.
// It tries structured content first (Go MCP server) then falls back to text
// content (TypeScript MCP server).
func CallMCP(t *testing.T, ctx context.Context, sess *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tool %s returned error: %s", name, mcpContentText(res.Content))
	}
	var raw []byte
	if res.StructuredContent != nil {
		raw, err = json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal %s structured: %v", name, err)
		}
	} else {
		raw = []byte(mcpContentText(res.Content))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal %s into %T: %v\nraw: %s", name, out, err, raw)
	}
}

func mcpContentText(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
