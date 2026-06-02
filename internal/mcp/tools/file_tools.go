package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
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

type WriteParams struct {
	Path    string `json:"path" jsonschema:"Absolute path of the file to write"`
	Content string `json:"content" jsonschema:"File contents to write"`
}

type WriteResponse struct {
	Bytes int `json:"bytes"`
}

type EditParams struct {
	Path       string `json:"path" jsonschema:"Absolute path of the file to edit"`
	OldString  string `json:"oldString" jsonschema:"Substring to replace"`
	NewString  string `json:"newString" jsonschema:"Replacement string"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"Replace every occurrence; otherwise oldString must match exactly once"`
}

type EditResponse struct {
	Replacements int `json:"replacements"`
}

type GlobParams struct {
	Pattern string `json:"pattern" jsonschema:"Glob pattern. Supports *, ?, [class] and ** for any number of path segments"`
	Root    string `json:"root,omitempty" jsonschema:"Directory to search under. Defaults to the current working directory"`
}

type GlobResponse struct {
	Paths []string `json:"paths"`
}

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

// FileTools implements file-system tool operations against a mounted volume.
// ResolvePath maps an agent-visible absolute path to its host-side path.
// When nil, paths are used as-is (suitable for the standalone MCP server).
type FileTools struct {
	ResolvePath func(agentPath string) (string, error)
}

func (f *FileTools) resolve(agentPath string) (string, error) {
	if f.ResolvePath == nil {
		return agentPath, nil
	}
	return f.ResolvePath(agentPath)
}

func (f *FileTools) Read(_ context.Context, _ *mcpsdk.CallToolRequest, params *ReadParams) (*mcpsdk.CallToolResult, *ReadResponse, error) {
	hostPath, err := f.resolve(params.Path)
	if err != nil {
		return nil, nil, err
	}

	limit := params.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	file, err := os.Open(hostPath)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	sc := bufio.NewScanner(file)
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

func (f *FileTools) Write(_ context.Context, _ *mcpsdk.CallToolRequest, params *WriteParams) (*mcpsdk.CallToolResult, *WriteResponse, error) {
	hostPath, err := f.resolve(params.Path)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(hostPath, []byte(params.Content), 0o644); err != nil {
		return nil, nil, err
	}
	return nil, &WriteResponse{Bytes: len(params.Content)}, nil
}

func (f *FileTools) Edit(_ context.Context, _ *mcpsdk.CallToolRequest, params *EditParams) (*mcpsdk.CallToolResult, *EditResponse, error) {
	hostPath, err := f.resolve(params.Path)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(hostPath)
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

	if err := os.WriteFile(hostPath, []byte(out), 0o644); err != nil {
		return nil, nil, err
	}
	return nil, &EditResponse{Replacements: count}, nil
}

func (f *FileTools) Glob(_ context.Context, _ *mcpsdk.CallToolRequest, params *GlobParams) (*mcpsdk.CallToolResult, *GlobResponse, error) {
	root := params.Root
	if root == "" {
		root = "."
	}
	hostRoot, err := f.resolve(root)
	if err != nil {
		return nil, nil, err
	}

	var matches []string
	err = filepath.WalkDir(hostRoot, func(hostPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(hostRoot, hostPath)
		if err != nil {
			return nil
		}
		ok, _ := matchGlob(params.Pattern, rel)
		if !ok {
			ok, _ = matchGlob(params.Pattern, filepath.Base(hostPath))
		}
		if ok {
			matches = append(matches, filepath.Join(root, rel))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	return nil, &GlobResponse{Paths: matches}, nil
}

func (f *FileTools) Grep(ctx context.Context, _ *mcpsdk.CallToolRequest, params *GrepParams) (*mcpsdk.CallToolResult, *GrepResponse, error) {
	re, err := regexp.Compile(params.Pattern)
	if err != nil {
		return nil, nil, err
	}

	hostPath, err := f.resolve(params.Path)
	if err != nil {
		return nil, nil, err
	}

	info, err := os.Stat(hostPath)
	if err != nil {
		return nil, nil, err
	}

	res := &GrepResponse{}
	if !info.IsDir() {
		matches, err := grepFile(re, hostPath)
		if err != nil {
			return nil, nil, err
		}
		for _, m := range matches {
			m.Path = params.Path
			res.Matches = append(res.Matches, m)
		}
		return nil, res, nil
	}

	err = filepath.WalkDir(hostPath, func(hp string, d fs.DirEntry, walkErr error) error {
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
		matches, _ := grepFile(re, hp)
		rel, _ := filepath.Rel(hostPath, hp)
		agentPath := filepath.Join(params.Path, rel)
		for _, m := range matches {
			m.Path = agentPath
			res.Matches = append(res.Matches, m)
		}
		return nil
	})
	return nil, res, err
}

// matchGlob is like filepath.Match but supports "**" as "any number of path
// segments (including zero)".
func matchGlob(pattern, name string) (bool, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Match(pattern, name)
	}
	parts := strings.Split(pattern, "**")
	first := strings.TrimSuffix(parts[0], "/")
	if first != "" {
		prefix := first + "/"
		if !strings.HasPrefix(name+"/", prefix) {
			return false, nil
		}
		name = strings.TrimPrefix(name, prefix)
		name = strings.TrimPrefix(name, first)
	}
	for i, p := range parts[1:] {
		p = strings.TrimPrefix(p, "/")
		last := i == len(parts)-2
		if p == "" {
			return true, nil
		}
		found := -1
		for j := 0; j <= len(name); j++ {
			candidate := name[j:]
			if last {
				if ok, _ := filepath.Match(p, candidate); ok {
					return true, nil
				}
			} else {
				if idx := strings.Index(candidate, "/"); idx >= 0 {
					if ok, _ := filepath.Match(p, candidate[:idx]); ok {
						found = j + idx + 1
						break
					}
				}
			}
		}
		if last {
			return false, nil
		}
		if found < 0 {
			return false, nil
		}
		name = name[found:]
	}
	return true, nil
}

func grepFile(re *regexp.Regexp, path string) ([]GrepMatch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

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
