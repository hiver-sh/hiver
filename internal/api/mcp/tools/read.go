package tools

import (
	"bufio"
	"context"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultReadLimit = 2000

type ReadParams struct {
	Path   string `json:"path" jsonschema:"Absolute path of the file to read"`
	Offset int    `json:"offset,omitempty" jsonschema:"0-based line index to start reading from. Defaults to 0"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of lines to return. Defaults to 2000"`
}

type ReadResponse struct {
	Content   string `json:"content"`
	StartLine int    `json:"startLine"`
	LineCount int    `json:"lineCount"`
	Truncated bool   `json:"truncated"`
}

func Read(_ context.Context, _ *mcp.CallToolRequest, params *ReadParams) (*mcp.CallToolResult, *ReadResponse, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	f, err := os.Open(params.Path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var b strings.Builder
	var taken, seen int
	truncated := false
	for sc.Scan() {
		if seen < params.Offset {
			seen++
			continue
		}
		if taken >= limit {
			truncated = true
			break
		}
		if taken > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(sc.Text())
		taken++
		seen++
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}

	return nil, &ReadResponse{
		Content:   b.String(),
		StartLine: params.Offset,
		LineCount: taken,
		Truncated: truncated,
	}, nil
}
