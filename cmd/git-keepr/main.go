// Command git-keepr is a lightweight, self-hosted Git backup service.
package main

import (
	"os"

	"github.com/amirkode/git-keepr/internal/cli"
	"github.com/amirkode/git-keepr/internal/credentials"
)

func main() {
	credentials.MaybeRunAskpass() // git re-executes this binary to obtain credentials
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
