package main

import (
	"os"

	"github.com/value-quest/igor/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
