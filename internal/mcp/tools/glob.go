package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GlobParams struct {
	Pattern string `json:"pattern" jsonschema:"Glob pattern. Supports *, ?, [class] and ** for any number of path segments"`
	Root    string `json:"root,omitempty" jsonschema:"Directory to search under. Defaults to the current working directory"`
}

type GlobResponse struct {
	Paths []string `json:"paths"`
}

func Glob(_ context.Context, _ *mcp.CallToolRequest, params *GlobParams) (*mcp.CallToolResult, *GlobResponse, error) {
	root := params.Root
	if root == "" {
		root = "."
	}

	var matches []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		ok, _ := matchGlob(params.Pattern, rel)
		if !ok {
			// Also allow matching the basename for patterns like "*.go".
			if basename, _ := matchGlob(params.Pattern, filepath.Base(path)); basename {
				ok = true
			}
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

// matchGlob is like filepath.Match but supports "**" as "any number of path
// segments (including zero)".
func matchGlob(pattern, name string) (bool, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Match(pattern, name)
	}
	parts := strings.Split(pattern, "**")
	// Anchor the first segment.
	first := strings.TrimSuffix(parts[0], "/")
	if first != "" {
		prefix := first + "/"
		if !strings.HasPrefix(name+"/", prefix) {
			return false, nil
		}
		name = strings.TrimPrefix(name, prefix)
		name = strings.TrimPrefix(name, first)
	}
	// Each remaining segment must match somewhere later in name, in order.
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
