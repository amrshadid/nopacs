package place

import (
	"net"
	"strconv"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestFreePortTakesTheLowestBindable guards the choice of a port for a
// receiver added without one: the lowest allotted port that no receiver
// holds and that binds, across ranges; none is a precondition failure.
func TestFreePortTakesTheLowestBindable(t *testing.T) {
	allotted := settings.Ports{{Lo: 11112, Hi: 11114}, {Lo: 12000, Hi: 12001}}
	taken := map[int]bool{11113: true, 12000: true} // by other programs
	probe := func(_ string, port int) bool { return !taken[port] }
	for _, c := range []struct {
		used map[int]bool
		want int
	}{
		{nil, 11112},
		{map[int]bool{11112: true}, 11114},
		{map[int]bool{11112: true, 11114: true}, 12001},
	} {
		got, err := FreePort(allotted, c.used, "", probe)
		if err != nil || got != c.want {
			t.Errorf("used %v: %d, %v; want %d", c.used, got, err, c.want)
		}
	}
	_, err := FreePort(allotted, map[int]bool{11112: true, 11114: true, 12001: true}, "", probe)
	if fault.KindOf(err) != fault.Precondition {
		t.Errorf("with every port held: %v, want a precondition failure", err)
	}
	if _, err := FreePort(nil, nil, "", probe); fault.KindOf(err) != fault.Precondition {
		t.Errorf("with no port allotted: %v, want a precondition failure", err)
	}
}

// TestSystemProbeSeesAPortHeld guards the probe against the system: a port
// another socket holds on the IPv4 wildcard does not bind, for a receiver
// of every address as for one of loopback; a free one does.
func TestSystemProbeSeesAPortHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("opens ports; left out under -short")
	}
	ln, err := net.Listen("tcp4", "0.0.0.0:0") // #nosec G102 -- the IPv4 wildcard is what the probe must see held.
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port
	if SystemProbe("", held) {
		t.Errorf("the port %d held on the IPv4 wildcard probes free for every address", held)
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	if !SystemProbe("127.0.0.1", port) {
		t.Errorf("the free port %s does not probe free", strconv.Itoa(port))
	}
}
