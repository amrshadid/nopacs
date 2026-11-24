package servertest

import (
	"context"
	"net/netip"
	"time"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// SetKeyRules replaces a key's rules through the access package.
func (s *accessService) SetKeyRules(ctx context.Context, req *nopacsv1.SetKeyRulesRequest) (*nopacsv1.SetKeyRulesResponse, error) {
	c, err := s.h.call(ctx)
	if err != nil {
		return nil, err
	}
	rules := master.KeyRules{ClientCert: req.GetClientCertSha256()}
	for _, n := range req.GetNetworks() {
		p, err := netip.ParsePrefix(n)
		if err != nil {
			a, aerr := netip.ParseAddr(n)
			if aerr != nil {
				return nil, fault.New(fault.Invalid, "networks: not a network")
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		rules.Networks = append(rules.Networks, p)
	}
	k, err := s.h.Access.SetKeyRules(ctx, c.Decision, req.GetId(), rules)
	if err != nil {
		return nil, err
	}
	return &nopacsv1.SetKeyRulesResponse{Key: keyOf(k)}, nil
}

// RotateKey rotates a key through the access package, with the overlap
// asked for (a day when none) and the longest lifetime a key may have.
func (s *accessService) RotateKey(ctx context.Context, req *nopacsv1.RotateKeyRequest) (*nopacsv1.RotateKeyResponse, error) {
	c, err := s.h.call(ctx)
	if err != nil {
		return nil, err
	}
	overlap := 24 * time.Hour
	if o := req.GetOverlap(); o != nil {
		overlap = o.AsDuration()
	}
	made, rotated, err := s.h.Access.RotateKey(ctx, c.Decision, req.GetId(), overlap, 3650*24*time.Hour)
	if err != nil {
		return nil, err
	}
	return &nopacsv1.RotateKeyResponse{Key: keyOf(made.Key), Secret: made.Secret.Key(), Rotated: keyOf(rotated)}, nil
}
