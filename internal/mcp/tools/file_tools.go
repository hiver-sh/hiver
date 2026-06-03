package tools

import (
	"bytes"
	"context"
	"fmt"
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

// DirEntry is one filesystem entry returned by an FS.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// FS is the filesystem the file tools operate on, keyed by agent-visible
// absolute paths. It is satisfied by the isolation backend's FileBridge (so
// container and microvm both work) and by [OSFS] for the standalone server.
type FS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadDir(path string) ([]DirEntry, error)
	Stat(path string) (DirEntry, error)
}

// FileTools implements file-system tool operations against an FS.
type FileTools struct {
	FS FS
}

// OSFS is a passthrough FS rooted at the host filesystem (paths used as-is),
// for the standalone MCP server with no isolation backend.
type OSFS struct{}

func (OSFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
func (OSFS) WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
func (OSFS) ReadDir(path string) ([]DirEntry, error) {
	es, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(es))
	for _, e := range es {
		var size int64
		if info, err := e.Info(); err == nil && !e.IsDir() {
			size = info.Size()
		}
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}
	return out, nil
}
func (OSFS) Stat(path string) (DirEntry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return DirEntry{}, err
	}
	var size int64
	if !info.IsDir() {
		size = info.Size()
	}
	return DirEntry{Name: filepath.Base(path), IsDir: info.IsDir(), Size: size}, nil
}

func (f *FileTools) fs() FS {
	if f.FS == nil {
		return OSFS{}
	}
	return f.FS
}

func (f *FileTools) Read(_ context.Context, _ *mcpsdk.CallToolRequest, params *ReadParams) (*mcpsdk.CallToolResult, *ReadResponse, error) {
	data, err := f.fs().ReadFile(params.Path)
	if err != nil {
		return nil, nil, err
	}
	limit := params.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var b strings.Builder
	taken, truncated := 0, false
	for i := params.Offset; i < len(lines); i++ {
		if taken >= limit {
			truncated = true
			break
		}
		if taken > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(lines[i])
		taken++
	}
	return nil, &ReadResponse{
		Content:   b.String(),
		StartLine: params.Offset,
		LineCount: taken,
		Truncated: truncated,
	}, nil
}

func (f *FileTools) Write(_ context.Context, _ *mcpsdk.CallToolRequest, params *WriteParams) (*mcpsdk.CallToolResult, *WriteResponse, error) {
	if err := f.fs().WriteFile(params.Path, []byte(params.Content)); err != nil {
		return nil, nil, err
	}
	return nil, &WriteResponse{Bytes: len(params.Content)}, nil
}

func (f *FileTools) Edit(_ context.Context, _ *mcpsdk.CallToolRequest, params *EditParams) (*mcpsdk.CallToolResult, *EditResponse, error) {
	data, err := f.fs().ReadFile(params.Path)
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

	if err := f.fs().WriteFile(params.Path, []byte(out)); err != nil {
		return nil, nil, err
	}
	return nil, &EditResponse{Replacements: count}, nil
}

func (f *FileTools) Glob(_ context.Context, _ *mcpsdk.CallToolRequest, params *GlobParams) (*mcpsdk.CallToolResult, *GlobResponse, error) {
	root := params.Root
	if root == "" {
		root = "/"
	}
	var matches []string
	err := walkFS(f.fs(), root, func(path string, _ DirEntry) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		ok, _ := matchGlob(params.Pattern, rel)
		if !ok {
			ok, _ = matchGlob(params.Pattern, filepath.Base(path))
		}
		if ok {
			matches = append(matches, path)
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

	info, err := f.fs().Stat(params.Path)
	if err != nil {
		return nil, nil, err
	}

	res := &GrepResponse{}
	if !info.IsDir {
		data, err := f.fs().ReadFile(params.Path)
		if err != nil {
			return nil, nil, err
		}
		for _, m := range grepBytes(re, data) {
			m.Path = params.Path
			res.Matches = append(res.Matches, m)
		}
		return nil, res, nil
	}

	err = walkFS(f.fs(), params.Path, func(path string, _ DirEntry) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, err := f.fs().ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range grepBytes(re, data) {
			m.Path = path
			res.Matches = append(res.Matches, m)
		}
		return nil
	})
	return nil, res, err
}

// walkFS recursively visits every regular file under root, calling fn with
// the absolute agent path. Implemented on top of FS.ReadDir so it works for
// both the host-backed and vsock-backed filesystems.
func walkFS(fsys FS, root string, fn func(path string, e DirEntry) error) error {
	entries, err := fsys.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name)
		if e.IsDir {
			if err := walkFS(fsys, p, fn); err != nil {
				return err
			}
			continue
		}
		if err := fn(p, e); err != nil {
			return err
		}
	}
	return nil
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

// grepBytes returns the matching lines in data. Binary content (a NUL in the
// first 512 bytes) is skipped.
func grepBytes(re *regexp.Regexp, data []byte) []GrepMatch {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil
	}
	var matches []GrepMatch
	for i, line := range strings.Split(string(data), "\n") {
		if re.MatchString(line) {
			matches = append(matches, GrepMatch{Line: i + 1, Text: line})
		}
	}
	return matches
}
