package main

import (
	"os"

	"github.com/BYK/switchyard/internal/mesh"
)

func main() {
	os.Exit(mesh.Main(os.Args[1:]))
}
