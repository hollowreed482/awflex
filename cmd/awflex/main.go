// awflex is a small ansible runner for Kubernetes: one binary that is both
// the server (awflex serve) and the JSON CLI for it.
package main

import (
	"log"
	"os"

	"awflex/internal/cli"
	"awflex/internal/core"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if os.Getpid() == 1 {
			os.Exit(core.RunAsInit())
		}
		log.SetFlags(log.LstdFlags | log.LUTC)
		if err := core.Serve(); err != nil {
			log.Fatalf("awflex: %v", err)
		}
		return
	}
	os.Exit(cli.Main(os.Args[1:]))
}
