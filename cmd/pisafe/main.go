package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mpizenberg/pisafe/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if err := cli.Run(ctx, args, stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "pisafe: %v\n", err)
		return 1
	}
	return 0
}
