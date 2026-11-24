package server_test

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// leakMarks are the frames of a goroutine that belongs to a server, a
// client connection or a database: any left once every test has stopped
// its server is a goroutine without an owner.
var leakMarks = []string{
	"nopacs/internal/server",
	"nopacs/internal/access",
	"net/http.(*conn)",
	"net/http.(*Server)",
	"net/http.(*persistConn)",
	"net/http.(*http2",
	"database/sql.(*DB)",
}

// TestMain runs the tests, or the other-user helper when this binary was
// started as one (see socket_other_test.go), and then checks that no
// goroutine of a server, a connection or a database is left running.
func TestMain(m *testing.M) {
	if os.Getenv(otherUserHelperEnv) != "" {
		os.Exit(runOtherUserHelper())
	}
	code := m.Run()
	if code == 0 {
		if leaked := leakedGoroutines(5 * time.Second); leaked != "" {
			fmt.Fprintln(os.Stderr, "goroutines left running after every test stopped its server:\n"+leaked)
			code = 1
		}
	}
	os.Exit(code)
}

// leakedGoroutines waits up to wait for the goroutines with a leak mark to
// end, and returns the stacks of those still running.
func leakedGoroutines(wait time.Duration) string {
	deadline := time.Now().Add(wait)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		var leaked []string
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "leakedGoroutines") {
				continue
			}
			for _, mark := range leakMarks {
				if strings.Contains(g, mark) {
					leaked = append(leaked, g)
					break
				}
			}
		}
		if len(leaked) == 0 || time.Now().After(deadline) {
			return strings.Join(leaked, "\n\n")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
