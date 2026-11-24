package place

import (
	"context"
	"net/netip"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReceiverValues answers the settings a receiver of one tenant is checked
// against.
type ReceiverValues interface {
	// Ports returns the ports the installation allotted to the tenant
	// (receiver.ports, as it applies on the tenant).
	Ports(ctx context.Context) (settings.Ports, error)
	// BindAddresses returns the addresses a receiver may listen on
	// (receiver.bind-addresses, the installation's).
	BindAddresses(ctx context.Context) (settings.BindAddresses, error)
}

// CheckReceiver checks what a receiver may use against what the
// installation allows: its port among the tenant's allotted ports, and its
// bind address, when it names one, among the installation's. A refusal is a
// precondition; a failure to read a value is returned as it is. The bind
// addresses are read only for a receiver that names one.
func CheckReceiver(ctx context.Context, v ReceiverValues, r tenant.Receiver) error {
	if r.Type != tenant.ReceiverDIMSE {
		return nil
	}
	ports, err := v.Ports(ctx)
	if err != nil {
		return err
	}
	if !ports.Contains(r.DIMSE.Port) {
		return fault.New(fault.Precondition, "receiver.dimse.port: the port "+strconv.Itoa(r.DIMSE.Port)+" is not among the ports the installation allotted to this tenant (its setting "+settings.ReceiverPorts+")")
	}
	if r.DIMSE.BindAddress == "" {
		return nil
	}
	addrs, err := v.BindAddresses(ctx)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return fault.New(fault.Precondition, "receiver.dimse.bind_address: the installation lists no bind address, so a receiver listens on every address and names none")
	}
	a, err := netip.ParseAddr(r.DIMSE.BindAddress)
	if err != nil || !addrs.Contains(a) {
		return fault.New(fault.Precondition, "receiver.dimse.bind_address: the address is not one the installation's setting "+settings.ReceiverBindAddresses+" lists")
	}
	return nil
}
