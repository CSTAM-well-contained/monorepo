// Command hostagent is the CSTAM host agent: builds each team's network namespace and containers.
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
