package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type EditParams struct {
	Path       string `json:"path" jsonschema:"Absolute path of the file to edit"`
	OldString  string `json:"oldString" jsonschema:"Substring to replace"`
	NewString  string `json:"newString" jsonschema:"Replacement string"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"Replace every occurrence; otherwise oldString must match exactly once"`
}

type EditResponse struct {
	Replacements int `json:"replacements"`
}

func Edit(_ context.Context, _ *mcp.CallToolRequest, params *EditParams) (*mcp.CallToolResult, *EditResponse, error) {
	data, err := os.ReadFile(params.Path)
	if err != nil {
		return nil, nil, err
	}
	src := string(data)

	count := strings.Count(src, params.OldString)
	if count == 0 {
		return nil, nil, fmt.Errorf("oldString not found in %s", params.Path)
	}
	if !params.ReplaceAll && count > 1 {
		return nil, nil, fmt.Errorf("oldString matches %d times in %s; pass replaceAll=true or include more context", count, params.Path)
	}

	var out string
	if params.ReplaceAll {
		out = strings.ReplaceAll(src, params.OldString, params.NewString)
	} else {
		out = strings.Replace(src, params.OldString, params.NewString, 1)
		count = 1
	}

	if err := os.WriteFile(params.Path, []byte(out), 0o644); err != nil {
		return nil, nil, err
	}
	return nil, &EditResponse{Replacements: count}, nil
}
