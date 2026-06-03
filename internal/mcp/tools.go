package mcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/blasten/hive/internal/mcp/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

const (
	name              = "sandbox-mcp-server"
	mcpRequestMessage = "MCP request"

	toolCallLogMaxArgs = 100
)

var toolLogger = func() *zap.Logger {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stdout"}
	l, err := cfg.Build()
	if err != nil {
		return zap.NewNop()
	}
	return l.Named(name)
}()

// NewContainerHandler returns an http.Handler for the MCP Streamable HTTP
// transport. Bash commands run inside the workload via execFn (the Exec
// handler's core logic). File operations (read/write/edit/glob/grep) go
// through fsys — the isolation backend's filesystem — so they work for both
// the container and microvm backends.
func NewContainerHandler(execFn tools.ExecFunc, fsys tools.FS) http.Handler {
	s := newContainerMCPServer(execFn, fsys)
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		nil,
	)
}

func newContainerMCPServer(execFn tools.ExecFunc, fsys tools.FS) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    name,
		Version: "1.0.0",
	}, nil)
	s.AddReceivingMiddleware(logToolCalls)

	ct := &tools.ContainerTools{ExecCommand: execFn}
	ft := &tools.FileTools{FS: fsys}

	mcp.AddTool(s, &mcp.Tool{
		Name: "bash",
		Description: `Execute a shell command and return stdout, stderr, and exit code.
Use 'read'/'write'/'edit'/'glob'/'grep' before falling back to 'bash' equivalents — they are typed, faster, and produce cleaner diffs.`,
	}, ct.Bash)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "read",
		Description: "Read the contents of a file. Use this instead of 'cat' when you only need to inspect a file.",
	}, ft.Read)
	mcp.AddTool(s, &mcp.Tool{
		Name: "write",
		Description: `Write contents to a file, creating parent directories as needed.
Use this instead of shell redirection so the file is captured atomically.`,
	}, ft.Write)
	mcp.AddTool(s, &mcp.Tool{
		Name: "edit",
		Description: `Replace a substring in a file.
Cheaper than rewriting the whole file when you're tweaking a script or report.`,
	}, ft.Edit)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "glob",
		Description: "Find files matching a glob pattern. (e.g. '**/*.csv').",
	}, ft.Glob)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "grep",
		Description: "Search files for lines matching a regular expression.",
	}, ft.Grep)

	return s
}

func logToolCalls(h mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				args := json.RawMessage(p.Arguments)
				if len(args) > toolCallLogMaxArgs {
					toolLogger.Info(mcpRequestMessage,
						zap.String("method", "tools/call"),
						zap.String("tool", p.Name),
						zap.String("params", truncate(string(args), toolCallLogMaxArgs)),
						zap.Bool("params_truncated", true),
					)
				} else {
					toolLogger.Info(mcpRequestMessage,
						zap.String("method", "tools/call"),
						zap.String("tool", p.Name),
						zap.Any("params", args),
					)
				}
			}
		} else {
			toolLogger.Info(mcpRequestMessage, zap.String("method", method))
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
