package main

import (
	"os"

	"github.com/khaleelhabeeb/relay/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
