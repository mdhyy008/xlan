package main

import (
	"os"

	"xlan/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
