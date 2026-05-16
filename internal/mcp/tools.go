package mcp

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sandbox-platform/agent-sandbox/internal/mcp/tools"
)

const toolCallLogMaxArgs = 100

func newMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "hive-sandbox",
		Version: "1.0.0",
	}, nil)

	s.AddReceivingMiddleware(logToolCalls)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "bash",
		Description: "Execute a shell command and return stdout, stderr, and exit code",
	}, tools.Bash)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "read",
		Description: "Read the contents of a file",
	}, tools.Read)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "write",
		Description: "Write contents to a file, creating parent directories as needed",
	}, tools.Write)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "edit",
		Description: "Replace a substring in a file",
	}, tools.Edit)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "glob",
		Description: "Find files matching a glob pattern",
	}, tools.Glob)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "grep",
		Description: "Search files for lines matching a regular expression",
	}, tools.Grep)

	return s
}

func logToolCalls(h mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				log.Printf("mcp tool=%s params=%s", p.Name, truncate(string(p.Arguments), toolCallLogMaxArgs))
			}
		} else {
			log.Printf("mcp method=%s", method)
		}
		return h(ctx, method, req)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
