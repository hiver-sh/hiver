package tools

import (
	"bytes"
	"context"
	"errors"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type BashParams struct {
	Cmd string `json:"cmd" jsonschema:"The command to execute"`
}

type BashResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func Bash(ctx context.Context, _ *mcp.CallToolRequest, params *BashParams) (*mcp.CallToolResult, *BashResponse, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", params.Cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &BashResponse{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return nil, nil, err
	}
	return nil, res, nil
}
