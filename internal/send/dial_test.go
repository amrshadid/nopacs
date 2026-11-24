package send

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// changingLookup answers a host name from a list that a test changes
// between connections, and counts the lookups.
type changingLookup struct {
	mu    sync.Mutex
	addrs []netip.Addr
	calls atomic.Int64
}

// set changes the answer.
func (l *changingLookup) set(addrs ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addrs = nil
	for _, a := range addrs {
		l.addrs = append(l.addrs, netip.MustParseAddr(a))
	}
}

// lookup answers the list now.
func (l *changingLookup) lookup(context.Context, string) ([]netip.Addr, error) {
	l.calls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]netip.Addr{}, l.addrs...), nil
}

// TestResolveChecksEachAnswer guards the check of where a connection goes:
// the host is resolved at each call, and an answer that moves the name
// from a public address to a link-local one (a cloud's metadata address)
// is refused at the next connection; the first allowed address of an
// answer is the one taken, and the others follow it for a dial that fails.
func TestResolveChecksEachAnswer(t *testing.T) {
	f := newFixture(t)
	f.inst.dests = mustDestinations(t, "pacs.example")
	cfg := f.config()
	l := &changingLookup{}
	cfg.seams.lookup = l.lookup
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l.set("8.8.8.8")
	got, err := s.resolve(f.ctx, "pacs.example", 104)
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddrPort("8.8.8.8:104") {
		t.Fatalf("resolve answered %v, %v; want 8.8.8.8:104", got, err)
	}
	l.set("169.254.169.254")
	var ce *connectError
	if _, err := s.resolve(f.ctx, "pacs.example", 104); !errors.As(err, &ce) || ce.fault != tenant.SenderFaultAddressNotAllowed {
		t.Errorf("a name that now resolves to the metadata address: %v, want address_not_allowed", err)
	}
	l.set("10.0.0.1", "8.8.4.4", "8.8.8.8")
	if got, err := s.resolve(f.ctx, "pacs.example", 104); err != nil || len(got) != 2 || got[0].Addr().String() != "8.8.4.4" {
		t.Errorf("resolve answered %v, %v; want the two public addresses, in order", got, err)
	}
	if l.calls.Load() != 3 {
		t.Errorf("%d lookups for 3 connections, want one each", l.calls.Load())
	}
}

// TestDestinationIsCheckedAtEachConnection guards the rule of where a
// sender may connect, on the DIMSE and the stow dial paths: the name is
// looked up again at each connection, and an answer the setting allows is
// the address reached. Listed without private-address, a name answering a
// loopback address is refused (address_not_allowed) and nothing reaches
// the peer; with it, the same name sends, and when the first address of an
// answer does not take the connection the next one is tried.
func TestDestinationIsCheckedAtEachConnection(t *testing.T) {
	for _, kind := range []string{"dimse", "stow"} {
		for _, allowed := range []bool{false, true} {
			name := kind + "/refused"
			if allowed {
				name = kind + "/allowed"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				f.inst.plain = true
				line := "pacs.test"
				if allowed {
					line += " private-address"
				}
				f.inst.dests = mustDestinations(t, line)
				f.archive("ses-1", scanFiles{"mr", generated(t, 1, 256)})
				var arrivals func() int
				if kind == "dimse" {
					peer := &dimsePeer{}
					addr := netip.MustParseAddrPort(startPeer(t, peer))
					f.addDIMSE("peer", addr.String(), func(s *tenant.Sender) { s.DIMSE.Host = "pacs.test" })
					arrivals = peer.arrivals
				} else {
					peer := &webPeer{}
					srv := httptest.NewServer(peer)
					peer.t, peer.parts, peer.count = t, map[string][]byte{}, map[string]int{}
					t.Cleanup(srv.Close)
					port := netip.MustParseAddrPort(srv.Listener.Addr().String()).Port()
					f.addStow("peer", "http://pacs.test:"+strconv.Itoa(int(port))+"/dicom-web", func(s *tenant.Sender) {
						s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil
					})
					arrivals = func() int { peer.mu.Lock(); defer peer.mu.Unlock(); return peer.requests }
				}
				job := f.createJob("peer", "ses-1")
				l := &changingLookup{}
				// The first address listens nowhere: ::1 on the peer's port.
				l.set("::1", "127.0.0.1")
				cfg := f.config()
				cfg.seams.lookup = l.lookup
				s := f.start(cfg)
				if allowed {
					if j := f.waitJob(s, job.ID, 30*time.Second, ended); j.State != tenant.JobDone || arrivals() != 1 {
						t.Fatalf("job %s with %d at the peer, want done with 1", j.State, arrivals())
					}
					return
				}
				f.stepOnEach(s, "two refused connections", time.Minute, 2*time.Minute, failedConnections(s, "peer"), func() bool {
					h, _ := s.Health("lab", "peer")
					return h.Fault == tenant.SenderFaultAddressNotAllowed && h.FailedConnections >= 2
				})
				if arrivals() != 0 {
					t.Errorf("the peer received %d through a refused address", arrivals())
				}
				if l.calls.Load() < 2 {
					t.Errorf("%d lookups for two connections, want one each", l.calls.Load())
				}
			})
		}
	}
}
