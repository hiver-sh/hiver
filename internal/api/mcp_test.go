package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestListTools(t *testing.T) {
	ctx := context.Background()

	ts := httptest.NewServer(NewServer("0").Handler)
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/v1/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer session.Close()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{"bash", "read", "write", "edit", "glob", "grep"} {
		if !got[name] {
			t.Errorf("missing tool %q; got %v", name, got)
		}
	}
}
