package tools

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GrepParams struct {
	Pattern string `json:"pattern" jsonschema:"Regular expression to search for (Go regexp syntax)"`
	Path    string `json:"path" jsonschema:"File or directory to search. Directories are searched recursively"`
}

type GrepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type GrepResponse struct {
	Matches []GrepMatch `json:"matches"`
}

func Grep(ctx context.Context, _ *mcp.CallToolRequest, params *GrepParams) (*mcp.CallToolResult, *GrepResponse, error) {
	re, err := regexp.Compile(params.Pattern)
	if err != nil {
		return nil, nil, err
	}

	info, err := os.Stat(params.Path)
	if err != nil {
		return nil, nil, err
	}

	res := &GrepResponse{}
	if !info.IsDir() {
		matches, err := grepFile(re, params.Path)
		if err != nil {
			return nil, nil, err
		}
		res.Matches = matches
		return nil, res, nil
	}

	err = filepath.WalkDir(params.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		matches, _ := grepFile(re, path)
		res.Matches = append(res.Matches, matches...)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, res, nil
}

func grepFile(re *regexp.Regexp, path string) ([]GrepMatch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Skip likely binary files.
	head := make([]byte, 512)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}

	var matches []GrepMatch
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if re.MatchString(text) {
			matches = append(matches, GrepMatch{Path: path, Line: line, Text: text})
		}
	}
	return matches, scanner.Err()
}
