package main

import (
	"os"

	"github.com/gitmoot/gitmoot/internal/cli"
)

func main() {
	cli.MaybeRegisterAgyFromEnv(os.Args[1:])
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
