package main

import (
	"flag"

	"github.com/sandbox-platform/agent-sandbox/internal/api"
)

func main() {
	var serverPort = flag.String("server-port", "9000", "port of the controller server")
	flag.Parse()

	s := api.NewControllerServer(*serverPort)
	s.ListenAndServe()
}
