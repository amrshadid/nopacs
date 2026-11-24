package dimse

import (
	"context"
	"net"
	"net/netip"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// admit decides an association before anything is negotiated (PS3.8
// 9.3.4), in an order that tells a caller no more than it may know:
//
//  1. a caller no rule of the receiver matches is rejected permanently,
//     calling-AE-title-not-recognized, whatever title it called, so a
//     stranger cannot learn the receiver's title by trying titles;
//  2. an admitted caller that called another title is rejected
//     permanently, called-AE-title-not-recognized;
//  3. while the archive cannot take files, or the listener is starting or
//     stopping, an admitted caller is rejected as transient
//     (temporary-congestion), so it keeps its copies and tries again.
//
// Each refusal but the start's and the stop's is reported with its reason;
// those are the server's own doing and the receiver's health says so.
func (l *Listener) admit(_ context.Context, req *network.AssociationRequest) *network.AssociationRejection {
	addr, _ := hostOf(req.RemoteAddr)
	caller := tenant.GroupCaller{CallingAETitle: req.CallingAE, CalledAETitle: req.CalledAE}
	if addr.IsValid() {
		caller.Address = addr.String()
	}
	d := l.cfg.Receiver.DIMSE
	if !d.Admits(req.CallingAE, addr) {
		l.cfg.Report(tenant.FailureCallerNotAllowed, caller)
		return network.RejectCallingAE()
	}
	if req.CalledAE != d.AETitle {
		l.cfg.Report(tenant.FailureCalledAETitleNotRecognized, caller)
		return network.RejectCalledAE()
	}
	if l.isStopping() {
		return network.RejectBusy()
	}
	if f := l.cfg.Refusal(); f != "" {
		l.cfg.Report(f, caller)
		return network.RejectBusy()
	}
	return nil
}

// hostOf returns the IP address and port of a connection's remote end, the
// address unmapped from IPv4-mapped IPv6 and without a zone, so a caller
// from 192.0.2.7 on a listener of every address matches "192.0.2.0/24" and
// is written the one way a range and an event write it. The address is not
// valid when it cannot be read.
func hostOf(a net.Addr) (netip.Addr, uint16) {
	var ap netip.AddrPort
	switch v := a.(type) {
	case *net.TCPAddr:
		ap = v.AddrPort()
	case nil:
		return netip.Addr{}, 0
	default:
		p, err := netip.ParseAddrPort(v.String())
		if err != nil {
			return netip.Addr{}, 0
		}
		ap = p
	}
	return ap.Addr().Unmap().WithZone(""), ap.Port()
}

// callerOf returns who called, as a received file records it: the AE titles
// and the address with its port.
func callerOf(callingAE, calledAE string, remote net.Addr) tenant.Caller {
	c := tenant.Caller{}
	if tenant.ValidateAETitle(callingAE) == nil {
		c.CallingAETitle = callingAE
	}
	if tenant.ValidateAETitle(calledAE) == nil {
		c.CalledAETitle = calledAE
	}
	if addr, port := hostOf(remote); addr.IsValid() {
		c.Address = netip.AddrPortFrom(addr, port).String()
	}
	return c
}
