package main

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
)

// BenchmarkRealServer measures one request end to end through the real
// program: a serve process of this binary (the real server, the real check,
// the real handlers, the SQLite master and a SQLite tenant), called with
// WhoAmI and ListTenants over the local socket as the local operator and
// over TLS on loopback with an installation reader's key, each with 1, 8 and
// 64 clients at once over one connection. It reports the time per request
// and requests per second. A baseline, not a claim: run with
//
//	go test -run '^$' -bench BenchmarkRealServer -benchtime 3s ./cmd/nopacs
func BenchmarkRealServer(b *testing.B) {
	s := newBenchSession(b)
	for _, tr := range []struct {
		name string
		cfg  client.Config
	}{
		{"socket", client.Config{DataDir: s.dir}},
		{"tls", client.Config{Server: s.server, CAFile: s.cert, Getenv: func(k string) string {
			if k == client.EnvKey {
				return s.secret
			}
			return ""
		}}},
	} {
		c, err := client.New(tr.cfg)
		if err != nil {
			b.Fatal(err)
		}
		for _, method := range []string{"WhoAmI", "ListTenants"} {
			callOnce := func() error {
				var err error
				if method == "WhoAmI" {
					_, err = c.Access.WhoAmI(b.Context(), &v1.WhoAmIRequest{})
				} else {
					_, err = c.Tenants.ListTenants(b.Context(), &v1.ListTenantsRequest{})
				}
				return err
			}
			for _, n := range []int{1, 8, 64} {
				b.Run(tr.name+"/"+method+"/clients="+strconv.Itoa(n), func(b *testing.B) {
					if err := callOnce(); err != nil {
						b.Fatal(err)
					}
					var mu sync.Mutex
					left := b.N
					take := func() bool {
						mu.Lock()
						defer mu.Unlock()
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
								if err := callOnce(); err != nil {
									b.Error(err)
									return
								}
							}
						})
					}
					wg.Wait()
					b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "req/s")
				})
			}
		}
		c.Close()
	}
}

// benchSession is a served installation with one tenant and a key that
// reads every tenant.
type benchSession struct {
	dir, server, cert, secret string
}

// newBenchSession starts serve on a new installation and makes the tenant
// and the key with the real commands.
func newBenchSession(b *testing.B) benchSession {
	b.Helper()
	s := newSession(b)
	s.ok(nil, "init", "--data-dir", s.dir)
	s.serve()
	s.ok(s.local, "tenant", "create", "lab1")
	r := s.run(s.local, "key", "create", "--holder", "bench", "--no-expiry")
	secret := strings.TrimSpace(r.stdout)
	id := strings.Split(secret, "_")[1]
	s.ok(s.local, "grant", "set", id, "--role", "reader", "--installation")
	return benchSession{dir: s.dir, server: "https://127.0.0.1:" + strconv.Itoa(s.port), cert: s.cert, secret: secret}
}
