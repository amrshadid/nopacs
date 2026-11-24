package main

import (
	"context"
	"io"
	"os"
	"os/signal"

	"github.com/amrshadid/nopacs/internal/servecmd"
)

func main() {
	ctx, stop := context.Background(), func() {}
	// An interrupt cancels init, backup and tenant import, which undo what
	// they made, instead of killing the process half-way. Serve takes the
	// stop signals itself, to drain what is in flight and write the audit
	// entries held in memory, so it gets no canceling context.
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
	}
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the whole program with its arguments and writers passed in, so a test
// can drive it and read what it printed without starting a process.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return servecmd.Run(ctx, args, servecmd.Env{Stdout: stdout, Stderr: stderr})
}
