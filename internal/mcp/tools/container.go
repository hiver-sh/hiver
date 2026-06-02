package tools

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ExecFunc runs a command inside the sandbox container and returns buffered
// stdout, stderr, and the process exit code. A non-zero exit code is not
// treated as a Go error.
type ExecFunc func(ctx context.Context, command string, cwd *string, env *map[string]string) (stdout, stderr string, exitCode int, err error)

type BashParams struct {
	Cmd string `json:"cmd" jsonschema:"Shell command to execute via /bin/sh -c. Supports pipes, redirection, quoting, and environment variable expansion. USE 'cwd' to scope a command to a directory."`
	Cwd string `json:"cwd,omitempty" jsonschema:"Absolute or relative working directory to run the command in. Defaults to the container's working directory."`
}

type BashResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// ContainerTools runs bash commands inside the sandbox container via ExecCommand.
type ContainerTools struct {
	ExecCommand ExecFunc
}

func (c *ContainerTools) Bash(ctx context.Context, _ *mcpsdk.CallToolRequest, params *BashParams) (*mcpsdk.CallToolResult, *BashResponse, error) {
	var cwd *string
	if params.Cwd != "" {
		cwd = &params.Cwd
	}
	stdout, stderr, exitCode, err := c.ExecCommand(ctx, params.Cmd, cwd, nil)
	if err != nil {
		return nil, nil, err
	}
	return nil, &BashResponse{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
}
