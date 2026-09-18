package main

import (
	"context"
	"github.com/zhoushoujianwork/memgov/internal/scenario"
	"os"
)

func main() {
	if scenario.Main(context.Background(), os.Stdin, os.Stdout, scenario.Command) != nil {
		os.Exit(1)
	}
}
