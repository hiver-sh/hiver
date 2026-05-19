package main

import (
	"flag"

	"github.com/blasten/hive/internal/api/controller"
)

func main() {
	var serverPort = flag.String("server-port", "9000", "port of the controller server")
	flag.Parse()

	s := controller.NewControllerServer(*serverPort)
	s.ListenAndServe()
}
