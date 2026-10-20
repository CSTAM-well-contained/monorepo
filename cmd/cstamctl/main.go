// Command cstamctl is the CSTAM admin CLI. Subcommands:
//
//	load   HTTP load test through the gateways (benchmark B8)
package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev" // set by -ldflags "-X main.version=..."

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cstamctl [-version] load -hosts h1,h2 [-url http://10.20.0.10/] [-c 32] [-d 20s]")
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	var err error
	switch flag.Arg(0) {
	case "load":
		err = loadCmd(flag.Args()[1:])
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
