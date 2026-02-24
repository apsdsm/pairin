package main

import (
	"os"

	"github.com/apsdsm/pairin/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
