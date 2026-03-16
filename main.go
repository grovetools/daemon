package main

import (
	"os"

	"github.com/grovetools/daemon/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
