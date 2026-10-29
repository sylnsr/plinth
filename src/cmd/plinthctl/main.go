// Command plinthctl is Plinth's one binary. Spec: plinthctl.README.md at the repo root.
package main

import (
	"os"

	"plinth/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
