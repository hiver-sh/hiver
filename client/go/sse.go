package client

import (
	"bufio"
	"io"
	"strings"
)

type sseFrame struct {
	id   string
	data string
}

// readSSE reads lines from r and emits one sseFrame per SSE event dispatched.
// The returned channel is closed when r returns EOF or an error.
func readSSE(r io.Reader) <-chan sseFrame {
	ch := make(chan sseFrame, 16)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		var cur sseFrame
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if cur.data != "" {
					ch <- cur
				}
				cur = sseFrame{}
			case strings.HasPrefix(line, "data:"):
				cur.data = strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			case strings.HasPrefix(line, "id:"):
				cur.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			}
		}
	}()
	return ch
}
