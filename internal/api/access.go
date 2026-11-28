package api

import (
	"context"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/version"
)

// accessService is AccessService. Every change and read goes through package
// access, which has the master check the Decision's key again (in the
// change's transaction, or before a read), writes a change with its audit
// entry in one transaction, records a read through its audit writer,
// and records every failure itself; so an error from it is answered as it
// is, never recorded twice. What this file records is a request it refuses
// as invalid before calling access.
type accessService struct{ c *core }

var _ nopacsv1connect.AccessServiceHandler = accessService{}

// CreateKey makes a key and answers its secret, once.
func (s accessService) CreateKey(ctx context.Context, req *v1.CreateKeyRequest) (*v1.CreateKeyResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.AccessServiceCreateKeyProcedure)
	if err != nil {
		return nil, err
	}
	// The requirement is fixed, so the check's entry has no object; a
	// request refused here names it in its entry.
	facts := access.Facts{Details: map[string]string{"holder": req.GetHolder()}}
	if err := text("holder", req.GetHolder(), 1, MaxHolderLength); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	rules, err := keyRulesOf("", req.GetNetworks(), req.GetClientCertSha256())
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	maxLifetime, err := c.values().KeyMaxLifetime(ctx)
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	var exp access.Expiry
	switch e := req.GetExpiry().(type) {
	case *v1.CreateKeyRequest_ExpireAfter:
		if e.ExpireAfter.CheckValid() != nil {
			return nil, c.failWith(d, nil, facts, invalid("expire_after", "is not a valid duration"))
		}
		after := e.ExpireAfter.AsDuration()
		if after <= 0 || after > MaxKeyLifetime {
			return nil, c.failWith(d, nil, facts, invalid("expire_after", "is positive and at most 10 years (3650 days)"))
		}
		if after > maxLifetime {
			return nil, c.failWith(d, nil, facts, invalid("expire_after", "is at most the installation's key.max-lifetime, "+maxLifetime.String()))
		}
		exp = access.ExpireAfter(after)
	case *v1.CreateKeyRequest_NeverExpire:
		if !e.NeverExpire {
			return nil, c.failWith(d, nil, facts, invalid("never_expire", "is true when set; leave expiry unset for the installation's key.default-lifetime"))
		}
		exp = access.NeverExpire()
	default:
		// No expiry asked for: the installation's key.default-lifetime
		// applies, else its built-in default (access.proto,
		// CreateKeyRequest.expiry).
		lifetime, known, err := c.keyLifetime(ctx)
		if err != nil {
			return nil, c.failWith(d, nil, facts, err)
		}
		if known {
			exp = access.ExpireAfter(min(lifetime, maxLifetime))
		}
	}
	create := c.access.CreateKey
	if req.GetMasked() {
		create = c.access.CreateMaskedKey
	}
	made, err := create(ctx, d, req.GetHolder(), exp, rules)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.CreateKeyResponse{Key: keyMessage(made.Key), Secret: made.Secret.Key()}, nil
}

// GetKey answers one key, revoked and expired keys included.
func (s accessService) GetKey(ctx context.Context, req *v1.GetKeyRequest) (*v1.GetKeyResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AccessServiceGetKeyProcedure)
	if err != nil {
		return nil, err
	}
	// The requirement is fixed, so the check's entry has no object; a
	// request refused here names it in its entry.
	facts := access.Facts{Object: master.KeyObject(req.GetId())}
	if err := keyID("id", req.GetId()); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	k, err := c.access.GetKey(ctx, d, req.GetId())
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.GetKeyResponse{Key: keyMessage(k)}, nil
}

// ListKeys answers a page of every key, oldest first; or, with a tenant, of
// the keys holding a grant on it, as much of each as the caller may read.
func (s accessService) ListKeys(ctx context.Context, req *v1.ListKeysRequest) (*v1.ListKeysResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AccessServiceListKeysProcedure)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.fail(d, err)
	}
	var keys []master.Key
	var next string
	if req.GetTenant() != "" {
		if err := tenantName("tenant", req.GetTenant()); err != nil {
			return nil, c.fail(d, err)
		}
		keys, next, err = c.access.ListKeysOnTenant(ctx, d, req.GetTenant(), page)
	} else {
		keys, next, err = c.access.ListKeys(ctx, d, page)
	}
	if err != nil {
		return nil, c.toAPI(d, listError(err, page.Cursor))
	}
	out := &v1.ListKeysResponse{Keys: make([]*v1.Key, 0, len(keys)), NextPageToken: next}
	for _, k := range keys {
		out.Keys = append(out.Keys, keyMessage(k))
	}
	return out, nil
}

// RevokeKey ends a key for good; revoking it again answers its first revoke
// time.
func (s accessService) RevokeKey(ctx context.Context, req *v1.RevokeKeyRequest) (*v1.RevokeKeyResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.AccessServiceRevokeKeyProcedure)
	if err != nil {
		return nil, err
	}
	// The requirement is fixed, so the check's entry has no object; a
	// request refused here names it in its entry.
	facts := access.Facts{Object: master.KeyObject(req.GetId())}
	if err := keyID("id", req.GetId()); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	k, err := c.access.RevokeKey(ctx, d, req.GetId())
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.RevokeKeyResponse{Key: keyMessage(k)}, nil
}

// SetGrant sets a key's role on a scope, replacing the role it held there
// in one step.
func (s accessService) SetGrant(ctx context.Context, req *v1.SetGrantRequest) (*v1.SetGrantResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.AccessServiceSetGrantProcedure)
	if err != nil {
		return nil, err
	}
	// The requirement is fixed, so the check's entry has no object; a
	// request refused here names it in its entry.
	facts := access.Facts{Object: master.KeyObject(req.GetKeyId()), Details: map[string]string{detailScope: scopeDetail(req.GetScope())}}
	if err := keyID("key_id", req.GetKeyId()); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	role, ok := masterRole(req.GetRole())
	if !ok {
		return nil, c.failWith(d, nil, facts, invalid("role", "is required: reader, writer, operator or admin"))
	}
	if role == master.RoleWriter && scope.IsInstallation() {
		return nil, c.failWith(d, nil, facts, invalid("role", "writer is not a role on the installation"))
	}
	g, previous, err := c.access.SetGrant(ctx, d, req.GetKeyId(), scope, role)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.SetGrantResponse{Grant: grantMessage(g), PreviousRole: roleOf(previous)}, nil
}

// ListGrants answers a page of the grants of one key or on one scope.
func (s accessService) ListGrants(ctx context.Context, req *v1.ListGrantsRequest) (*v1.ListGrantsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AccessServiceListGrantsProcedure)
	if err != nil {
		return nil, err
	}
	// The requirement is fixed, so the check's entry has no object; a
	// request refused here names it in its entry.
	facts := access.Facts{Details: map[string]string{"filter": grantFilterDetail(req)}}
	var filter access.GrantFilter
	switch f := req.GetFilter().(type) {
	case *v1.ListGrantsRequest_KeyId:
		if err := keyID("key_id", f.KeyId); err != nil {
			return nil, c.failWith(d, nil, facts, err)
		}
		filter = access.GrantsOfKey(f.KeyId)
	case *v1.ListGrantsRequest_Scope:
		scope, err := scopeOf("scope", f.Scope)
		if err != nil {
			return nil, c.failWith(d, nil, facts, err)
		}
		filter = access.GrantsOnScope(scope)
	default:
		return nil, c.failWith(d, nil, facts, invalid("filter", "exactly one of key_id and scope is required"))
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	grants, next, err := c.access.ListGrants(ctx, d, filter, page)
	if err != nil {
		return nil, c.toAPI(d, listError(err, page.Cursor))
	}
	out := &v1.ListGrantsResponse{Grants: make([]*v1.Grant, 0, len(grants)), NextPageToken: next}
	for _, g := range grants {
		out.Grants = append(out.Grants, grantMessage(g))
	}
	return out, nil
}

// RemoveGrant takes back a key's grant on a scope; removing one it does not
// hold succeeds.
func (s accessService) RemoveGrant(ctx context.Context, req *v1.RemoveGrantRequest) (*v1.RemoveGrantResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.AccessServiceRemoveGrantProcedure)
	if err != nil {
		return nil, err
	}
	// The requirement is fixed, so the check's entry has no object; a
	// request refused here names it in its entry.
	facts := access.Facts{Object: master.KeyObject(req.GetKeyId()), Details: map[string]string{detailScope: scopeDetail(req.GetScope())}}
	if err := keyID("key_id", req.GetKeyId()); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	previous, err := c.access.RemoveGrant(ctx, d, req.GetKeyId(), scope)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.RemoveGrantResponse{PreviousRole: roleOf(previous)}, nil
}

// WhoAmI answers the caller: its key with the key's grants, or the local
// operator.
func (s accessService) WhoAmI(ctx context.Context, _ *v1.WhoAmIRequest) (*v1.WhoAmIResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AccessServiceWhoAmIProcedure)
	if err != nil {
		return nil, err
	}
	p, grants, err := c.access.WhoAmI(ctx, d)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	out := &v1.Principal{Grants: make([]*v1.Grant, 0, len(grants))}
	if k, ok := p.Key(); ok {
		out.Caller = &v1.Principal_Key{Key: keyMessage(k)}
	} else {
		out.Caller = &v1.Principal_LocalOperator{LocalOperator: &v1.LocalOperator{UserId: p.LocalUser()}}
	}
	for _, g := range grants {
		out.Grants = append(out.Grants, grantMessage(g))
	}
	// The build is read from the same place as "nopacs-server version", so a
	// client's comparison names the build that answered, not a copy that can
	// drift.
	info := version.Get()
	return &v1.WhoAmIResponse{Principal: out, ServerVersion: info.Version, ServerCommit: info.Commit}, nil
}

// grantFilterDetail returns the filter of a ListGrants request as its audit
// entry's details write it.
func grantFilterDetail(r *v1.ListGrantsRequest) string {
	switch f := r.GetFilter().(type) {
	case *v1.ListGrantsRequest_KeyId:
		return "key/" + master.KeyObject(f.KeyId)
	case *v1.ListGrantsRequest_Scope:
		return scopeDetail(f.Scope)
	}
	return "none"
}

// RotateKey replaces a key with a successor holding its grants and rules,
// and answers the successor's secret, once.
func (s accessService) RotateKey(ctx context.Context, req *v1.RotateKeyRequest) (*v1.RotateKeyResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.AccessServiceRotateKeyProcedure)
	if err != nil {
		return nil, err
	}
	// The overlap and the id are checked by package access after it has
	// decided whether the caller may rotate this key at all, so a caller
	// who may not learns nothing about what a valid request looks like.
	overlap := access.DefaultRotationOverlap
	if o := req.GetOverlap(); o != nil {
		overlap = -1
		if o.CheckValid() == nil {
			overlap = o.AsDuration()
		}
	}
	maxLifetime, err := c.values().KeyMaxLifetime(ctx)
	if err != nil {
		return nil, c.failWith(d, nil, access.Facts{Object: master.KeyObject(req.GetId())}, err)
	}
	made, rotated, err := c.access.RotateKey(ctx, d, req.GetId(), overlap, maxLifetime)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.RotateKeyResponse{Key: keyMessage(made.Key), Secret: made.Secret.Key(), Rotated: keyMessage(rotated)}, nil
}

// SetKeyRules replaces where a key may be used from.
func (s accessService) SetKeyRules(ctx context.Context, req *v1.SetKeyRulesRequest) (*v1.SetKeyRulesResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.AccessServiceSetKeyRulesProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Object: master.KeyObject(req.GetId())}
	if err := keyID("id", req.GetId()); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	rules, err := keyRulesOf("", req.GetNetworks(), req.GetClientCertSha256())
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	k, err := c.access.SetKeyRules(ctx, d, req.GetId(), rules)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.SetKeyRulesResponse{Key: keyMessage(k)}, nil
}

// ReinstateKeys lets suspended keys work again, by id or all of them. Only
// the local operator may: package access answers any key permission
// denied, whatever its grants, before the request is read, since after a
// restore every key is suspended and none may lift a suspension. With
// all, only the count is answered (access.proto, ReinstateKeysResponse).
func (s accessService) ReinstateKeys(ctx context.Context, req *v1.ReinstateKeysRequest) (*v1.ReinstateKeysResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.AccessServiceReinstateKeysProcedure)
	if err != nil {
		return nil, err
	}
	keys, n, err := c.access.ReinstateKeys(ctx, d, req.GetKeyIds(), req.GetAll())
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	out := &v1.ReinstateKeysResponse{Keys: make([]*v1.Key, 0, len(keys)), Reinstated: n}
	for _, k := range keys {
		out.Keys = append(out.Keys, keyMessage(k))
	}
	return out, nil
}

// keyRulesOf reads a key's rules from the networks and the fingerprint a
// request gives (access.proto, Key.networks and Key.client_cert_sha256):
// at most master.MaxKeyNetworks networks, each in its one written form and
// none twice, and a fingerprint of 64 lowercase hexadecimal digits or none.
// Each network is read alone, so a comma inside one cannot pass for two.
func keyRulesOf(prefix string, networks []string, cert string) (master.KeyRules, error) {
	if len(networks) > master.MaxKeyNetworks {
		return master.KeyRules{}, invalid(prefix+"networks", "lists at most "+strconv.Itoa(master.MaxKeyNetworks)+" networks")
	}
	var r master.KeyRules
	seen := make(map[string]bool, len(networks))
	for _, n := range networks {
		one, err := settings.ParseNetworks(n)
		if err != nil || len(one) != 1 {
			return master.KeyRules{}, invalid(prefix+"networks", "are each an address or address/bits, such as 10.0.0.0/8 or 2001:db8::/32, in its one written form: "+
				"IPv4 dotted, IPv6 as RFC 5952 writes it, no bit set past the prefix, a single address without /32 or /128")
		}
		if seen[n] {
			return master.KeyRules{}, invalid(prefix+"networks", "list "+n+" twice")
		}
		seen[n] = true
		r.Networks = append(r.Networks, one[0])
	}
	if cert != "" && !master.ValidClientCert(cert) {
		return master.KeyRules{}, invalid(prefix+"client_cert_sha256", "is the certificate's SHA-256 in 64 lowercase hexadecimal digits")
	}
	r.ClientCert = cert
	return r, nil
}

// expiringKeys returns the keys that expire within the installation's
// key.expiry-warning, as the status of every tenant answers them. A part
// of the status that cannot be read is left out and logged, never answered
// as an error, as every other part of a status is.
func (c *core) expiringKeys(ctx context.Context) []*v1.Key {
	within, err := c.values().KeyExpiryWarning(ctx)
	if err != nil {
		c.log.Warn("api: the keys about to expire were not read", "error", err)
		return nil
	}
	keys, err := c.access.ExpiringKeys(ctx, within)
	if err != nil {
		c.log.Warn("api: the keys about to expire were not read", "error", err)
		return nil
	}
	out := make([]*v1.Key, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyMessage(k))
	}
	return out
}
