package server_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// BenchmarkRequest measures one request end to end with the check in place:
// a gRPC call of ListTenants (whose stand-in does nothing, so what is
// measured is the transport, the envelope, the request path and the check)
// over the local socket as the local operator, and over TLS on loopback
// with a key (whose check reads the key and its roles from the SQLite
// master), each with 1, 8 and 64 clients calling at once over one HTTP/2
// connection. It reports the time per request and requests per second. A
// baseline, not a claim: run with
//
//	go test -run '^$' -bench BenchmarkRequest -benchtime 3s ./internal/server
func BenchmarkRequest(b *testing.B) {
	e := newEnv(b, options{})
	key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	for _, tr := range []struct {
		name string
		cl   clients
	}{
		{"socket", e.local()},
		{"tls", e.network("Bearer " + key)},
	} {
		for _, n := range []int{1, 8, 64} {
			b.Run(tr.name+"/clients="+strconv.Itoa(n), func(b *testing.B) {
				req := &nopacsv1.ListTenantsRequest{}
				if _, err := tr.cl.tenants.ListTenants(e.ctx, req); err != nil {
					b.Fatal(err)
				}
				var next sync.Mutex
				left := b.N
				take := func() bool {
					next.Lock()
					defer next.Unlock()
					if left == 0 {
						return false
					}
					left--
					return true
				}
				b.ResetTimer()
				start := time.Now()
				var wg sync.WaitGroup
				for range n {
					wg.Go(func() {
						for take() {
							if _, err := tr.cl.tenants.ListTenants(e.ctx, req); err != nil {
								b.Error(err)
								return
							}
						}
					})
				}
				wg.Wait()
				took := time.Since(start)
				b.ReportMetric(float64(b.N)/took.Seconds(), "req/s")
			})
		}
	}
}
