package send

import (
	"io"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// startThrottle starts a TCP proxy to target on a loopback port that passes
// what the client sends at rate bytes a second, steadily, as a slow link
// does, and what the peer sends back at once.
func startThrottle(t *testing.T, target string, rate int) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
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
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(c, up)
			}()
			go func() {
				defer wg.Done()
				const ticks = 50
				buf := make([]byte, rate/ticks)
				tick := time.NewTicker(time.Second / ticks)
				defer tick.Stop()
				for range tick.C {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := up.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						_ = up.(*net.TCPConn).CloseWrite()
						return
					}
				}
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
		wg.Wait()
	})
	return l.Addr().String()
}

// TestSlowSteadyLinksComplete guards that a peer that is slow but keeps
// taking bytes is never cut, whatever an instance's size: a stall, not a
// total, ends a send. An instance of 8 MiB over a link of 512 KiB/s (16
// seconds, three times the response timeout of 5 seconds) is sent by
// C-STORE and by STOW-RS, and two instances of 2 MiB on two associations
// sharing the sender's 256 KiB/s are both sent; no attempt fails.
func TestSlowSteadyLinksComplete(t *testing.T) {
	big := generated(t, 1, 8<<20)
	two := generated(t, 2, 2<<20)
	t.Run("dimse", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		peer := &dimsePeer{hash: true}
		f.archive("ses-1", scanFiles{"mr", big})
		f.addDIMSE("peer", startThrottle(t, startPeer(t, peer), 512<<10), func(s *tenant.Sender) { s.ResponseTimeout = 5 * time.Second })
		rec := f.record()
		job := f.createJob("peer", "ses-1")
		s := f.start(f.config())
		j := f.waitJob(s, job.ID, 90*time.Second, ended)
		if _, failed := rec.counts(); j.State != tenant.JobDone || failed != 0 {
			t.Errorf("job %s %+v with %d failed attempts, want done with none", j.State, j.Counts, failed)
		}
	})
	t.Run("stow", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.inst.plain = true
		peer := &webPeer{}
		peer.t, peer.parts, peer.count = t, map[string][]byte{}, map[string]int{}
		srv := httptest.NewServer(peer)
		t.Cleanup(srv.Close)
		f.archive("ses-1", scanFiles{"mr", big})
		f.addStow("pacs", "http://"+startThrottle(t, srv.Listener.Addr().String(), 512<<10)+"/dicom-web", func(s *tenant.Sender) {
			s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil
			s.ResponseTimeout = 5 * time.Second
		})
		rec := f.record()
		job := f.createJob("pacs", "ses-1")
		s := f.start(f.config())
		j := f.waitJob(s, job.ID, 90*time.Second, ended)
		if _, failed := rec.counts(); j.State != tenant.JobDone || failed != 0 {
			t.Errorf("job %s %+v with %d failed attempts, want done with none", j.State, j.Counts, failed)
		}
	})
	t.Run("shared rate", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		peer := &dimsePeer{hash: true}
		f.archive("ses-1", scanFiles{"a", two[:1]}, scanFiles{"b", two[1:]})
		f.addDIMSE("peer", startPeer(t, peer), func(s *tenant.Sender) {
			s.ResponseTimeout = 5 * time.Second
			s.MaxAssociations = 2
			s.MaxBytesPerSecond = 256 << 10
		})
		rec := f.record()
		job := f.createJob("peer", "ses-1")
		s := f.start(f.config())
		j := f.waitJob(s, job.ID, 90*time.Second, ended)
		if _, failed := rec.counts(); j.State != tenant.JobDone || failed != 0 {
			t.Errorf("job %s %+v with %d failed attempts, want done with none", j.State, j.Counts, failed)
		}
	})
}
