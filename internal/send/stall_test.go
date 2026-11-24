package send

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// stallProxy passes a TCP connection to target, and stops reading what the
// client sends after stallAfter bytes, as a peer that stops reading does;
// what the peer sends back still passes.
type stallProxy struct {
	l          net.Listener
	target     string
	stallAfter int64
	stalled    chan struct{}
	once       sync.Once
	wg         sync.WaitGroup
}

// startStallProxy starts a proxy to target on a loopback port.
func startStallProxy(t *testing.T, target string, stallAfter int64) *stallProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &stallProxy{l: l, target: target, stallAfter: stallAfter, stalled: make(chan struct{})}
	var conns []net.Conn
	var mu sync.Mutex
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, c, up)
			mu.Unlock()
			p.wg.Add(2)
			go func() {
				defer p.wg.Done()
				_, _ = io.Copy(c, up)
			}()
			go func() {
				defer p.wg.Done()
				_, _ = io.CopyN(up, c, p.stallAfter)
				p.once.Do(func() { close(p.stalled) })
				// Reads nothing more; the connection stays open until the
				// test ends.
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		p.wg.Wait()
	})
	return p
}

// TestDIMSEPeerThatStopsReading guards a peer that stops reading a data set
// midway: a response timeout after the last fragment it took, the
// association is cut, the unit fails for now with the
// reason a timeout gives, and a Stop while it is stalled returns within its
// own bound.
func TestDIMSEPeerThatStopsReading(t *testing.T) {
	files := generated(t, 1, 32<<20)
	t.Run("deadline", func(t *testing.T) {
		f := newFixture(t)
		peer := &dimsePeer{hash: true}
		proxy := startStallProxy(t, startPeer(t, peer), 1<<20)
		f.archive("ses-1", scanFiles{"mr", files})
		f.addDIMSE("peer", proxy.l.Addr().String(), func(s *tenant.Sender) { s.ResponseTimeout = 5 * time.Second })
		rec := f.record()
		f.createJob("peer", "ses-1")
		f.start(f.config())
		<-proxy.stalled
		start := time.Now()
		waitFor(t, 20*time.Second, "the stalled unit's failure", func() bool { _, failed := rec.counts(); return failed >= 1 })
		t.Logf("the stalled instance was cut %s after the peer stopped reading", time.Since(start).Round(100*time.Millisecond))
	})
	t.Run("stop", func(t *testing.T) {
		f := newFixture(t)
		peer := &dimsePeer{hash: true}
		proxy := startStallProxy(t, startPeer(t, peer), 1<<20)
		f.archive("ses-1", scanFiles{"mr", files})
		f.addDIMSE("peer", proxy.l.Addr().String(), func(s *tenant.Sender) { s.ResponseTimeout = time.Minute })
		f.createJob("peer", "ses-1")
		s, err := New(f.config())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Start(f.ctx); err != nil {
			t.Fatal(err)
		}
		<-proxy.stalled
		time.Sleep(200 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		_ = s.Stop(ctx)
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("Stop took %s with a bound of 1s while a peer stalled", d)
		}
	})
}

// TestDIMSEAssociationOutlivesConnectTimeout guards an association that
// lasts longer than twice the connect timeout: a peer that takes 100 ms over
// each of 30 instances with a connect timeout of 1 second is sent every
// instance on one association, and no attempt fails.
func TestDIMSEAssociationOutlivesConnectTimeout(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{delay: 100 * time.Millisecond}
	addr := startPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 30, 256)})
	f.addDIMSE("peer", addr, func(s *tenant.Sender) { s.ConnectTimeout = time.Second })
	rec := f.record()
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if _, failed := rec.counts(); j.State != tenant.JobDone || failed != 0 {
		t.Errorf("job %s %+v with %d failed attempts, want done with none", j.State, j.Counts, failed)
	}
}

// TestStowStalls guards a stow peer that stalls: one that never reads a
// large request, and one that trickles its answer a byte a second. Each
// is cut a response timeout after it stops making progress (the body no
// longer taken, or the answer not whole within the response timeout of the
// body's end), and the unit fails for now.
func TestStowStalls(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"never reads": func(t *testing.T) string {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var held []net.Conn
			var mu sync.Mutex
			done := make(chan struct{})
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						close(done)
						return
					}
					mu.Lock()
					held = append(held, c)
					mu.Unlock()
				}
			}()
			t.Cleanup(func() {
				_ = l.Close()
				<-done
				mu.Lock()
				for _, c := range held {
					_ = c.Close()
				}
				mu.Unlock()
			})
			return "http://" + l.Addr().String() + "/dicom-web"
		},
		"trickles": func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/dicom+json")
				w.WriteHeader(http.StatusOK)
				for range 60 {
					if _, err := w.Write([]byte(" ")); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(time.Second):
					}
				}
			}))
			t.Cleanup(srv.Close)
			return srv.URL + "/dicom-web"
		},
	}
	files := generated(t, 1, 32<<20)
	for name, peer := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.inst.plain = true
			url := peer(t)
			f.archive("ses-1", scanFiles{"mr", files})
			f.addStow("pacs", url, func(s *tenant.Sender) {
				s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil
				s.ResponseTimeout = 5 * time.Second
			})
			rec := f.record()
			f.createJob("pacs", "ses-1")
			f.start(f.config())
			waitFor(t, 25*time.Second, "the stalled request's failure", func() bool {
				_, failed := rec.counts()
				released, _ := rec.counts()
				return failed+released >= 1
			})
		})
	}
}

// TestStowAnswerIsBounded guards the read of a store response: an answer
// of 40 MiB is not read whole into memory; the request fails, and the heap
// grows by far less than the answer.
func TestStowAnswerIsBounded(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/dicom+json")
		pad := make([]byte, 1<<20)
		for i := range pad {
			pad[i] = ' '
		}
		for range 40 {
			if _, err := w.Write(pad); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	f.archive("ses-1", scanFiles{"mr", generated(t, 1, 256)})
	f.addStow("pacs", srv.URL+"/dicom-web", func(s *tenant.Sender) {
		s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil
	})
	rec := f.record()
	f.createJob("pacs", "ses-1")
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	base := m.HeapAlloc
	stop := testkit.HeapPeak()
	s := f.start(f.config())
	waitFor(t, 20*time.Second, "the request's failure", func() bool { r, fl := rec.counts(); return r+fl >= 1 })
	grew := float64(stop()) - float64(base)
	t.Logf("heap grew by %.1f MiB for an answer of 40 MiB", grew/(1<<20))
	if grew > 16<<20 {
		t.Errorf("the heap grew by %.0f bytes reading an answer of 40 MiB", grew)
	}
	if h, _ := s.Health("lab", "pacs"); h.Fault != tenant.SenderFaultWrongAnswer {
		t.Errorf("health %+v, want the fault of an answer the protocol does not allow", h)
	}
}
