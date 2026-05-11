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

	if len(res.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(res.Tools))
	}

	toolNames := ""
	for idx, tool := range res.Tools {
		toolNames += tool.Name
		if idx > 0 {
			toolNames += ","
		}
	}

	want := "bash"
	if toolNames != want {
		t.Errorf("tool names = %q, want %q", toolNames, want)
	}
}
