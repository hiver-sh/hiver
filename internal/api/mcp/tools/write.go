package tools

import (
	"context"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type WriteParams struct {
	Path    string `json:"path" jsonschema:"Absolute path of the file to write"`
	Content string `json:"content" jsonschema:"File contents to write"`
}

type WriteResponse struct {
	Bytes int `json:"bytes"`
}

func Write(_ context.Context, _ *mcp.CallToolRequest, params *WriteParams) (*mcp.CallToolResult, *WriteResponse, error) {
	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(params.Path, []byte(params.Content), 0o644); err != nil {
		return nil, nil, err
	}
	return nil, &WriteResponse{Bytes: len(params.Content)}, nil
}
