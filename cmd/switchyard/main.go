package main

import (
	"os"

	"github.com/BYK/switchyard/internal/switchyard"
)

func main() {
	os.Exit(switchyard.Main(os.Args[1:]))
}
