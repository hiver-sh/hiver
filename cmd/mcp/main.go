package main

import (
	"flag"

	"github.com/sandbox-platform/agent-sandbox/internal/mcp"
)

func main() {
	var serverPort = flag.String("server-port", "8081", "port of the MCP server")
	flag.Parse()

	s := mcp.NewServer(*serverPort)
	s.ListenAndServe()
}
