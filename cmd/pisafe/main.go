package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/mpizenberg/pisafe/internal/cli"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if err := cli.Run(ctx, args, stdout); err != nil {
		fmt.Fprintf(stderr, "pisafe: %v\n", err)
		return 1
	}
	return 0
}
