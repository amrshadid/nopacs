package place

import (
	"net"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Probe reports whether port can be bound on host now, host "" meaning
// every address.
type Probe func(host string, port int) bool

// SystemProbe binds the port for a moment and lets it go. With no host it
// binds the IPv4 wildcard and then every address, as a receiver of every
// address checks before it listens: on macOS and the BSDs a dual-stack bind
// succeeds beside another program that holds the port for IPv4, so the
// IPv4 bind is what shows that program.
func SystemProbe(host string, port int) bool {
	p := strconv.Itoa(port)
	if host == "" {
		ln, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", p))
		if err != nil {
			return false
		}
		_ = ln.Close()
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, p))
	if err != nil {
		return false
	}
	return ln.Close() == nil
}

// maxProbes bounds the ports one choice tries, so an allotment of
// thousands of ports, most of them taken by other programs, costs a bounded
// time rather than a bind for each.
const maxProbes = 256

// FreePort returns the lowest port of allotted that no port in used holds
// and that probe can bind on host now: what the server gives a receiver
// added without a port. None is a precondition failure naming the setting,
// so the installation's admin knows what to widen. A port it returns can
// still be taken before the receiver listens; the receiver is then down
// with the reason, as one given a port is.
func FreePort(allotted settings.Ports, used map[int]bool, host string, probe Probe) (int, error) {
	tried := 0
	for _, r := range allotted {
		for port := r.Lo; port <= r.Hi; port++ {
			if used[port] {
				continue
			}
			if tried == maxProbes {
				return 0, fault.New(fault.Precondition, "receiver.dimse.port: no port of the "+strconv.Itoa(maxProbes)+" first free in the tenant's setting "+settings.ReceiverPorts+" can be bound now; give one")
			}
			tried++
			if probe(host, port) {
				return port, nil
			}
		}
	}
	return 0, fault.New(fault.Precondition, "receiver.dimse.port: every port of the tenant's setting "+settings.ReceiverPorts+" is held by a receiver or by another program")
}
