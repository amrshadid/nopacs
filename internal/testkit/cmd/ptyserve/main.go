// Command ptyserve serves the in-process fake server on a temporary local
// socket, for scripts/check/tui.py, which runs the built nopacs binary under
// a pseudo-terminal against it.
//
// It is test support and nothing else. It is not built by "make build", is not
// part of the nopacs binary, and must never be: it imports the fake server,
// which is test support too, and the testing package that the fake's helpers
// are written against. cmd/nopacs's tests fail if either is linked there.
//
// On standard output it writes one line, "DATADIR=<path>", when the socket is
// ready. It then reads commands from standard input, one per line: "stats"
// answers with one line of how many calls each method has had, and the end of
// input (or SIGTERM) stops it and removes the directory.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// runner is the part of testing.TB the fake server's helpers use. Embedding the
// interface satisfies all of it; the methods used here are written out, and any
// other would panic, which would show at once.
type runner struct {
	testing.TB
	cleanups []func()
}

// Helper does nothing: there is no test to attribute a failure to.
func (*runner) Helper() {}

// Fatalf ends the program with the message.
func (*runner) Fatalf(format string, args ...any) { log.Fatalf(format, args...) }

// Errorf logs the message.
func (*runner) Errorf(format string, args ...any) { log.Printf(format, args...) }

// Cleanup remembers a function to run on the way out.
func (r *runner) Cleanup(fn func()) { r.cleanups = append(r.cleanups, fn) }

// done runs the cleanups, last first, as a test would.
func (r *runner) done() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

func main() {
	tenants := flag.Int("tenants", 5, "how many tenants to make")
	delay := flag.Duration("delay", 0, "how long each call waits before it is answered")
	flag.Parse()

	fake := fakeserver.New()
	fake.Delay = *delay
	now := time.Now()
	for i := range *tenants {
		t := &v1.Tenant{
			Name: fmt.Sprintf("t%03d", i), DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE,
			State: v1.TenantState_TENANT_STATE_READY, CreateTime: timestamppb.New(now.Add(-time.Duration(i) * time.Hour)),
		}
		if i == 1 {
			t.State, t.StateReason, t.DatabaseType = v1.TenantState_TENANT_STATE_UNREACHABLE, "connection refused", v1.DatabaseType_DATABASE_TYPE_POSTGRES
		}
		fake.AddTenant(t)
	}
	// One scan in the first tenant, for the screens that act on the catalog.
	at := timestamppb.New(now.Add(-time.Hour))
	fake.AddSubject("t000", &v1.Subject{Label: "sub-a", SessionCount: 1, InstanceCount: 1, SizeBytes: 100, ArchiveTime: at})
	fake.AddSession("t000", &v1.Session{SubjectLabel: "sub-a", Label: "ses-1", ScanCount: 1, InstanceCount: 1, SizeBytes: 100, ArchiveTime: at})
	fake.AddScan("t000", &v1.Scan{SubjectLabel: "sub-a", SessionLabel: "ses-1", Label: "scan-1", Modality: "MR", InstanceCount: 1, SizeBytes: 100, ArchiveTime: at})
	fake.AddKey(&v1.Key{Id: "k00000000100", Holder: "nightly export", CreateTime: timestamppb.New(now)})
	fake.AddAudit(&v1.AuditEntry{Id: "a1", Time: timestamppb.New(now), Method: "/nopacs.v1.TenantService/ListTenants",
		Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, RequestId: "req-seed"})

	r := &runner{}
	dir := fake.ServeUnix(r)
	fmt.Printf("DATADIR=%s\n", dir)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-stop:
			r.done()
			return
		case line, ok := <-lines:
			if !ok {
				r.done()
				return
			}
			if strings.TrimSpace(line) == "stats" {
				counts := map[string]int{}
				for _, s := range fake.SeenRequests() {
					counts[s.URL[strings.LastIndex(s.URL, "/")+1:]]++
				}
				names := make([]string, 0, len(counts))
				for n := range counts {
					names = append(names, n)
				}
				sort.Strings(names)
				parts := make([]string, 0, len(names))
				for _, n := range names {
					parts = append(parts, fmt.Sprintf("%s=%d", n, counts[n]))
				}
				fmt.Printf("STATS %s\n", strings.Join(parts, " "))
			}
		}
	}
}
