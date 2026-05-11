package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type BashParams struct {
	Cmd string `json:"cmd" jsonschema:"The command to execute"`
}

type BashResponse struct {
	ExitCode int `json:"exitCode"`
}

func Bash(ctx context.Context, req *mcp.CallToolRequest, params *BashParams) (*mcp.CallToolResult, any, error) {
	res := BashResponse{ExitCode: 0}
	return &mcp.CallToolResult{
		StructuredContent: res,
	}, nil, nil
}
