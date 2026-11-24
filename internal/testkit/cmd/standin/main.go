// Command standin runs the real "init" and "serve" commands with the
// stand-in handlers of package servertest, so outside clients (grpcurl,
// Python grpcio, curl) can be pointed at a real server process before the
// real handlers exist. scripts/check/server.sh builds and drives it. It is
// never part of a release: make build builds ./cmd/nopacs only.
//
// NOPACS_STANDIN_DELAY sets how long ListTenants holds a request whose
// page token is "wait", as a Go duration (default 200ms).
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/servecmd"
	"github.com/amrshadid/nopacs/internal/testkit/servertest"
)

func main() {
	delay := 200 * time.Millisecond
	if v := os.Getenv("NOPACS_STANDIN_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "standin: NOPACS_STANDIN_DELAY is not a duration")
			os.Exit(2)
		}
		delay = d
	}
	opts := servecmd.Options{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Services: func(d servecmd.Deps) (servecmd.Built, error) {
			h := &servertest.Standins{Access: d.Access, Delay: delay}
			return servecmd.Built{Register: h.Register, Methods: servertest.Contract()}, nil
		},
	}
	root := &cobra.Command{Use: "standin", SilenceUsage: true}
	// As internal/cli does: --data-dir belongs to the root, for every command.
	root.PersistentFlags().String(servecmd.DataDirFlag, "", "the data directory")
	root.AddCommand(servecmd.Init(opts), servecmd.Serve(opts))
	if err := root.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
