package main

import (
	"context"
	"io"
	"os"
	"os/signal"

	"github.com/amrshadid/nopacs/internal/cli"
)

func main() {
	// An interrupt cancels the call in flight instead of killing the process
	// between a request and its answer.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := runIn(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the whole program with its arguments and writers passed in, so a test
// can drive it and read what it printed without starting a process.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runIn(ctx, args, nil, stdout, stderr)
}

// runIn is run with standard input, which only the interactive program reads:
// it opens when there are no arguments and input and output are terminals.
func runIn(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return cli.Run(ctx, args, cli.Env{Stdin: stdin, Stdout: stdout, Stderr: stderr})
}
