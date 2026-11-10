package main

import (
	"fmt"
	"log/slog"
	"os"
)

var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "joinprobe":
		err = joinprobe()
	case "joinconsole":
		err = joinconsole()
	case "healthcheck":
		err = healthcheck()
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("usage: minecraft [serve|joinprobe|joinconsole|healthcheck|version]")
	}
	if err != nil {
		slog.Error("minecraft", "err", err)
		os.Exit(1)
	}
}
