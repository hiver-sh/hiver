package mcp

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sandbox-platform/agent-sandbox/internal/mcp/tools"
	"go.uber.org/zap"
)

const (
	name = "sandbox-mcp-server"

	toolCallLogMaxArgs = 100
)

var toolLogger = func() *zap.Logger {
	l, err := zap.NewProduction()
	if err != nil {
		return zap.NewNop()
	}
	return l.Named(name)
}()

func newMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    name,
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
				args := json.RawMessage(p.Arguments)
				if len(args) > toolCallLogMaxArgs {
					toolLogger.Info("tools/call",
						zap.String("tool", p.Name),
						zap.String("params", truncate(string(args), toolCallLogMaxArgs)),
						zap.Bool("params_truncated", true),
					)
				} else {
					toolLogger.Info("tools/call",
						zap.String("tool", p.Name),
						zap.Any("params", args),
					)
				}
			}
		} else {
			toolLogger.Debug("rpc", zap.String("method", method))
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
