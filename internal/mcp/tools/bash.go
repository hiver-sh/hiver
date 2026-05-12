package tools

import (
	"bytes"
	"context"
	"errors"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type BashParams struct {
	Cmd string `json:"cmd" jsonschema:"Shell command to execute via /bin/sh -c. Supports pipes, redirection, quoting, and environment variable expansion. Each call runs in a fresh shell, so 'cd' and exported variables do not persist to later calls — use 'cwd' to scope a command to a directory instead."`
	Cwd string `json:"cwd,omitempty" jsonschema:"Absolute or relative working directory to run the command in. Defaults to the server's working directory."`
}

type BashResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func Bash(ctx context.Context, _ *mcp.CallToolRequest, params *BashParams) (*mcp.CallToolResult, *BashResponse, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", params.Cmd)
	cmd.Dir = params.Cwd
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
