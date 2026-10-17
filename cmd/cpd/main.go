// Command cpd is the CSTAM control-plane daemon: REST API, IPAM, controller and reconciler.
package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev" // set by -ldflags "-X main.version=..."

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}
}
